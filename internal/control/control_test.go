package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/metadata"
)

// --- test doubles ---

// fakeStream 实现 grpc.ServerStreamingServer[*pb.Config]，收集 Send 到的版本。
type fakeStream struct {
	ctx  context.Context
	mu   sync.Mutex
	sent []*pb.Config
}

func (f *fakeStream) Send(cfg *pb.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, cfg)
	return nil
}
func (f *fakeStream) Context() context.Context     { return f.ctx }
func (f *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}
func (f *fakeStream) SendMsg(m any) error          { return nil }
func (f *fakeStream) RecvMsg(m any) error          { return nil }

func (f *fakeStream) versions() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var vs []int64
	for _, c := range f.sent {
		vs = append(vs, c.Version)
	}
	return vs
}

// failStore 模拟持久化故障：Save 永远报错。
type failStore struct{ *MemoryStore }

func (f *failStore) Save(context.Context, *pb.Config) error {
	return errors.New("pg down")
}

func routes() map[string]*pb.ProviderList {
	return RoutesToProto(map[string][]string{"default": {"mock-a"}})
}

// --- tests ---

func TestApplyConfigMonotonicAndPersisted(t *testing.T) {
	st := NewMemoryStore()
	srv, err := NewServer(st)
	if err != nil {
		t.Fatal(err)
	}
	for want := int64(1); want <= 3; want++ {
		cfg, err := srv.ApplyConfig(map[string]string{"sk-1": "t1"}, routes())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Version != want {
			t.Fatalf("apply %d: version=%d, want %d", want, cfg.Version, want)
		}
	}
	latest, err := st.LoadLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.Version != 3 {
		t.Fatalf("store latest=%+v, want v3", latest)
	}
}

// 持久化失败必须拒绝写入：版本不推进、内存不变。
// 否则控制面重启后 Store 里没有这一版，"单调"就是假的。
func TestApplyConfigSerializesConcurrentVersions(t *testing.T) {
	st := NewMemoryStore()
	srv, err := NewServer(st)
	if err != nil {
		t.Fatal(err)
	}

	const n = 32
	versions := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := srv.ApplyConfig(map[string]string{"sk-1": "t1"}, routes())
			if err != nil {
				t.Errorf("ApplyConfig: %v", err)
				return
			}
			versions <- cfg.Version
		}()
	}
	wg.Wait()
	close(versions)

	seen := make(map[int64]bool, n)
	for version := range versions {
		if seen[version] {
			t.Fatalf("duplicate version %d", version)
		}
		seen[version] = true
	}
	if len(seen) != n || srv.CurrentConfig().Version != n {
		t.Fatalf("versions=%d current=%d, want %d unique versions", len(seen), srv.CurrentConfig().Version, n)
	}
	for version := int64(1); version <= n; version++ {
		if !seen[version] {
			t.Fatalf("missing version %d", version)
		}
	}
}

func TestApplyConfigPersistenceFailureRollsBack(t *testing.T) {
	st := &failStore{NewMemoryStore()}
	srv, err := NewServer(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"sk-1": "t1"}, routes()); err == nil {
		t.Fatal("want persist error, got nil")
	}
	if v := srv.CurrentConfig().Version; v != 0 {
		t.Fatalf("version advanced to %d despite save failure, want 0", v)
	}
}

// 重启恢复：新实例必须从 Store 拿到最高版本，接着递增，而不是回到 1。
func TestRestoreAfterRestart(t *testing.T) {
	st := NewMemoryStore()
	srv, _ := NewServer(st)
	srv.ApplyConfig(map[string]string{"sk-1": "t1"}, routes())
	srv.ApplyConfig(map[string]string{"sk-1": "t1", "sk-2": "t2"}, routes())

	restarted, err := NewServer(st)
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.CurrentConfig().Version; got != 2 {
		t.Fatalf("restored version=%d, want 2", got)
	}
	cfg, _ := restarted.ApplyConfig(map[string]string{"sk-1": "t1"}, routes())
	if cfg.Version != 3 {
		t.Fatalf("post-restore version=%d, want 3 (no reset)", cfg.Version)
	}
}

// WatchConfig 必须真实推送，而不是"发一次然后睡到流结束"。
func TestWatchConfigPushesUpdates(t *testing.T) {
	st := NewMemoryStore()
	srv, _ := NewServer(st)
	srv.ApplyConfig(map[string]string{"sk-1": "t1"}, routes()) // v1

	ctx, cancel := context.WithCancel(context.Background())
	fs := &fakeStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- srv.WatchConfig(&pb.WatchRequest{Version: 0}, fs) }()

	// 补发当前版
	waitFor(t, func() bool { return len(fs.versions()) == 1 && fs.versions()[0] == 1 }, "initial catch-up")

	// 更新即时送达，无需重连
	srv.ApplyConfig(map[string]string{"sk-1": "t1", "sk-2": "t2"}, routes()) // v2
	waitFor(t, func() bool {
		vs := fs.versions()
		return len(vs) == 2 && vs[1] == 2
	}, "watch push v2")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchConfig returned err=%v after ctx cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WatchConfig did not return after ctx cancel")
	}
}

// 客户端已持有最新版的订阅：不应重复推送当前版。
func TestWatchNoSpuriousInitialSend(t *testing.T) {
	st := NewMemoryStore()
	srv, _ := NewServer(st)
	srv.ApplyConfig(map[string]string{"sk-1": "t1"}, routes()) // v1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeStream{ctx: ctx}
	go srv.WatchConfig(&pb.WatchRequest{Version: 1}, fs)

	time.Sleep(50 * time.Millisecond)
	if n := len(fs.versions()); n != 0 {
		t.Fatalf("got %d initial sends for up-to-date client, want 0", n)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
