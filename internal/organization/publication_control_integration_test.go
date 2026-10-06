package organization_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/controlclient"
	"llm-gateway/internal/organization"
	pb "llm-gateway/proto/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PG + gRPC 集成测试：真实 PostgreSQL（policy_publications 状态机）、真实
// controlclient（真实 gRPC 传输）与进程内 fake AdminService。它覆盖的是
// 「组织变更 → 发布行 → worker 声明 → 适配器全量提交 → 状态落盘」的完整链路。

type recordingAdminService struct {
	pb.UnimplementedAdminServiceServer

	mu       sync.Mutex
	requests []*pb.ApplyOrgPolicyRequest
	failures int
	calls    int
}

func (s *recordingAdminService) ApplyOrgPolicy(_ context.Context, req *pb.ApplyOrgPolicyRequest) (*pb.ApplyOrgPolicyResponse, error) {
	s.mu.Lock()
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	if s.failures > 0 {
		s.mu.Lock()
		s.failures--
		s.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "control restarted mid-publish")
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	return &pb.ApplyOrgPolicyResponse{
		Policy:           &pb.OrgPolicy{OrgId: req.GetOrgId(), PolicyVersion: 1},
		ResultingVersion: int64(calls),
	}, nil
}

func (s *recordingAdminService) snapshot() ([]*pb.ApplyOrgPolicyRequest, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.ApplyOrgPolicyRequest(nil), s.requests...), s.calls
}

func startRecordingAdminService(t *testing.T) (*recordingAdminService, *controlclient.Client) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	service := &recordingAdminService{}
	server := grpc.NewServer()
	pb.RegisterAdminServiceServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	// The local demo transport: TLS/mTLS is covered by internal/controlclient's
	// own tests; here the subject is the publication protocol.
	client, err := controlclient.Dial(listener.Addr().String(), controlclient.Options{})
	if err != nil {
		t.Fatalf("dial fake control: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return service, client
}

func newPublicationWorker(t *testing.T, db *sql.DB, client *controlclient.Client, opts organization.PublicationOptions) *organization.PublicationWorker {
	t.Helper()
	publisher, err := organization.NewControlPublisher(client, organization.NewOrganizationPolicySource(db), organization.ControlPublisherOptions{})
	if err != nil {
		t.Fatalf("NewControlPublisher: %v", err)
	}
	return organization.NewPublicationWorker(db, publisher, opts)
}

// TestPublicationWorkerPublishesThroughRealGRPC is the PG+gRPC end-to-end case:
// one membership change produces one pending publication, the worker claims it,
// the adapter submits the complete policy over gRPC, and the row becomes
// published with the deterministic operation id.
func TestPublicationWorkerPublishesThroughRealGRPC(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner-publish@example.com")
	member := createUser(t, db, "member-publish@example.com")
	org := seedEnterprise(t, db, svc, "publish-acme", []int64{owner.ID, member.ID}, []string{"owner", "member"})
	revision := orgRevisionOf(t, db, org.ID)
	if revision != 1 {
		t.Fatalf("revision=%d, want 1 after creation", revision)
	}

	// A membership change bumps org_revision and writes the publication row in
	// the same transaction.
	if err := svc.ChangeMemberStatus(ctx, owner.ID, org.ID, member.ID, "suspended"); err != nil {
		t.Fatalf("ChangeMemberStatus: %v", err)
	}
	revision = orgRevisionOf(t, db, org.ID)
	rows := listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].Status != "pending" || rows[0].OrgRevision != revision {
		t.Fatalf("publications=%+v, want one pending row at revision %d", rows, revision)
	}

	service, client := startRecordingAdminService(t)
	worker := newPublicationWorker(t, db, client, organization.PublicationOptions{
		RetryBase: time.Millisecond,
		RetryMax:  10 * time.Millisecond,
	})
	processed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed=%d, want 1", processed)
	}

	requests, _ := service.snapshot()
	if len(requests) != 1 {
		t.Fatalf("control received %d apply requests, want 1", len(requests))
	}
	request := requests[0]
	wantOperationID := publicationOperationIDForTest(org.ID, revision)
	if request.GetOperationId() != wantOperationID {
		t.Fatalf("operation_id=%q, want %q", request.GetOperationId(), wantOperationID)
	}
	if request.GetOrgId() != org.ID || request.GetActorOrgId() != org.ID || request.GetActorUserId() != 0 {
		t.Fatalf("发布 actor/org 不正确: %+v", request)
	}
	if request.GetModelAllowlist() != nil || request.RequestsPerMinute != nil || request.Burst != nil {
		t.Fatalf("默认期望策略必须是「继承 + 无限制」的全量替换: %+v", request)
	}
	if request.GetRequestId() == "" {
		t.Fatal("发布必须带 request_id 以便审计关联")
	}

	rows = listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].Status != "published" {
		t.Fatalf("publications=%+v, want the row published", rows)
	}
	if !rows[0].OperationID.Valid || rows[0].OperationID.String != wantOperationID {
		t.Fatalf("operation_id 未落盘: %+v", rows[0])
	}

	// A second round has nothing to do: publication is not repeated.
	processed, err = worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if processed != 0 {
		t.Fatalf("second round processed=%d, want 0", processed)
	}
	if requests, _ := service.snapshot(); len(requests) != 1 {
		t.Fatalf("已发布的策略被重复提交了 %d 次", len(requests))
	}
}

// TestPublicationWorkerDropsSupersededRevisions pins the "old revision must not
// overwrite new state" rule (ADR-0003 §2.6) at the adapter boundary: a
// publication row whose revision is behind the organization is deleted rather
// than published.
func TestPublicationWorkerDropsSupersededRevisions(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner-super@example.com")
	member := createUser(t, db, "member-super@example.com")
	org := seedEnterprise(t, db, svc, "publish-super", []int64{owner.ID, member.ID}, []string{"owner", "member"})

	// Two membership changes: revision 2 (active -> suspended) and
	// revision 3 (suspended -> active).
	if err := svc.ChangeMemberStatus(ctx, owner.ID, org.ID, member.ID, "suspended"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangeMemberStatus(ctx, owner.ID, org.ID, member.ID, "active"); err != nil {
		t.Fatal(err)
	}
	revision := orgRevisionOf(t, db, org.ID)
	if revision != 3 {
		t.Fatalf("revision=%d, want 3", revision)
	}
	// Simulate a stale row that survived: mark the newest row pending again and
	// insert one for the previous revision by hand (the UNIQUE constraint makes
	// this the only way to construct the race deterministically).
	if _, err := db.Exec(`DELETE FROM policy_publications WHERE org_id = $1`, org.ID); err != nil {
		t.Fatal(err)
	}
	for _, staleRevision := range []int64{revision - 1, revision} {
		if _, err := db.Exec(`INSERT INTO policy_publications
  (org_id, org_revision, scope, target_id, status, attempts, created_at, updated_at)
VALUES ($1, $2, 'org', '', 'pending', 0, now(), now())`, org.ID, staleRevision); err != nil {
			t.Fatal(err)
		}
	}

	service, client := startRecordingAdminService(t)
	worker := newPublicationWorker(t, db, client, organization.PublicationOptions{})
	processed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if processed != 2 {
		t.Fatalf("processed=%d, want 2 (one dropped, one published)", processed)
	}
	rows := listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].OrgRevision != revision || rows[0].Status != "published" {
		t.Fatalf("publications=%+v, want only the current revision published", rows)
	}
	requests, _ := service.snapshot()
	if len(requests) != 1 || requests[0].GetOperationId() != publicationOperationIDForTest(org.ID, revision) {
		t.Fatalf("旧 revision 不得覆盖新状态: %+v", requests)
	}
}

// TestPublicationWorkerRetriesUntilControlRecovers covers the bounded backoff
// path: a transport failure leaves the row failed with last_error and a
// next_retry_at, and the same deterministic operation id publishes it later.
func TestPublicationWorkerRetriesUntilControlRecovers(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner-retry@example.com")
	member := createUser(t, db, "member-retry@example.com")
	org := seedEnterprise(t, db, svc, "publish-retry", []int64{owner.ID, member.ID}, []string{"owner", "member"})
	if err := svc.ChangeMemberStatus(ctx, owner.ID, org.ID, member.ID, "suspended"); err != nil {
		t.Fatal(err)
	}
	revision := orgRevisionOf(t, db, org.ID)

	service, client := startRecordingAdminService(t)
	// The client retries transport failures automatically (the deterministic
	// operation id makes that safe), so the injection must outlive every attempt
	// of the first round.
	service.failures = 100
	worker := newPublicationWorker(t, db, client, organization.PublicationOptions{
		RetryBase: time.Millisecond,
		RetryMax:  5 * time.Millisecond,
	})
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	rows := listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("publications=%+v, want a failed row after the transport error", rows)
	}
	if !rows[0].LastError.Valid || rows[0].LastError.String == "" {
		t.Fatalf("失败原因必须写入 last_error: %+v", rows[0])
	}
	if !rows[0].NextRetryAt.Valid {
		t.Fatalf("失败后必须写入 next_retry_at: %+v", rows[0])
	}
	if requests, _ := service.snapshot(); len(requests) != 0 {
		t.Fatal("失败的调用不应留下成功记录")
	}

	// The backoff window is 1ms*2 in this configuration; wait it out, then the
	// retry must succeed and keep the same idempotency key.
	service.mu.Lock()
	service.failures = 0
	service.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("retry RunOnce: %v", err)
	}
	rows = listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].Status != "published" {
		t.Fatalf("publications=%+v, want the retry to publish", rows)
	}
	requests, _ := service.snapshot()
	if len(requests) != 1 {
		t.Fatalf("apply requests=%d, want 1", len(requests))
	}
	if requests[0].GetOperationId() != publicationOperationIDForTest(org.ID, revision) {
		t.Fatalf("重试必须使用同一个幂等键: %q", requests[0].GetOperationId())
	}
}

// TestControlPublisherAgainstRealAdminServiceFailsOnUnreachableControl is the
// negative half: an unreachable control plane must surface as an error, not as
// a silently published policy.
func TestControlPublisherAgainstRealAdminServiceFailsOnUnreachableControl(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "owner-down@example.com")
	member := createUser(t, db, "member-down@example.com")
	org := seedEnterprise(t, db, svc, "publish-down", []int64{owner.ID, member.ID}, []string{"owner", "member"})
	if err := svc.ChangeMemberStatus(ctx, owner.ID, org.ID, member.ID, "suspended"); err != nil {
		t.Fatal(err)
	}

	// Port 1 is reserved and never listening: the call must fail fast and be
	// bounded by the client deadline.
	client, err := controlclient.Dial("127.0.0.1:1", controlclient.Options{CallTimeout: 500 * time.Millisecond, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	worker := newPublicationWorker(t, db, client, organization.PublicationOptions{})
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce must record the failure, not return it: %v", err)
	}
	rows := listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("publications=%+v, want failed", rows)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("unexpected canceled context")
	}
}

// publicationOperationIDForTest mirrors the worker's deterministic scheme
// (policy:<org>:<revision>:<scope>:<target>). It is duplicated here on purpose:
// the scheme is part of the Control-facing contract, so a change must break this
// test.
func publicationOperationIDForTest(orgID, revision int64) string {
	return "policy:" + strconv.FormatInt(orgID, 10) + ":" + strconv.FormatInt(revision, 10) + ":org:"
}
