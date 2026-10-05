package organization

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// 策略发布（ADR-0003 §2.6）：Console 只负责把“组织状态变了”这件事有界、可重试
// 地交给 Control；Gateway 何时应用由 T09 的心跳/租约判定，本任务不宣称即时全局
// 撤销。
//
// 每一条 policy_publications 行描述一个 (org, revision, scope, target) 的期望
// 状态。worker 的协议是：
//
//  1. 短事务声明一个有界批次（FOR UPDATE SKIP LOCKED + 租约），立即提交；
//  2. 锁外调用 Publisher（真实的 gRPC AdminService.ApplyOrgPolicy 在下一个任务
//     落地；本任务只依赖这里的接口，测试用 fake）；
//  3. 发布前重新读取 organizations.org_revision：旧 revision 不得覆盖新状态，
//     被取代的行直接删除（status 词表里没有 superseded，published/failed 都会
//     撒谎；当前 revision 永远有自己的行）；
//  4. 每个结果用一个新的短事务落盘，失败按有界指数退避重试，last_error 可查。
//
// operation_id 由 (org, revision, scope, target) 确定性生成，因此重试与重复
// worker 在 Control 侧天然幂等（同 ID 同 digest 返回已存结果）。

// PublishRequest 是一次 org 策略发布的全部上下文。它刻意不携带任何明文密钥：
// 发布的只是“期望状态”的版本指针。
type PublishRequest struct {
	PublicationID int64
	OperationID   string
	OrgID         int64
	OrgRevision   int64
	Scope         string
	TargetID      string
}

// Publisher 把一条期望状态交给 Control。真实实现是 Control AdminService 的
// gRPC 客户端（下一个任务）；测试用 fake。实现必须幂等：同一个 OperationID
// 重复调用只能产生一次效果。
type Publisher interface {
	PublishOrgPolicy(ctx context.Context, req PublishRequest) error
}

// PublicationOptions 覆盖 worker 的默认节奏。零值即生产默认。
type PublicationOptions struct {
	// BatchSize 是一次声明的最大行数（有界批次）。
	BatchSize int
	// Lease 是声明后的可见性超时：崩溃的 worker 留下的行在租约到期后被重拾。
	Lease time.Duration
	// RetryBase / RetryMax 界定指数退避区间。
	RetryBase time.Duration
	RetryMax  time.Duration
	// Logger 记录发布失败；nil 用 log.Default()。
	Logger *log.Logger
	// Now 只用于测试注入。
	Now func() time.Time
}

const (
	defaultPublicationBatchSize = 20
	defaultPublicationLease     = 30 * time.Second
	defaultPublicationRetryBase = 5 * time.Second
	defaultPublicationRetryMax  = 5 * time.Minute
)

// PublicationWorker 是唯一驱动 policy_publications 状态机的组件。
type PublicationWorker struct {
	db        *sql.DB
	publisher Publisher
	logger    *log.Logger
	now       func() time.Time
	opts      PublicationOptions
}

// NewPublicationWorker 归一化选项并返回 worker。publisher 为 nil 时 RunOnce
// 直接报错，避免“看起来在发布、实际什么都没做”。
func NewPublicationWorker(db *sql.DB, publisher Publisher, opts PublicationOptions) *PublicationWorker {
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultPublicationBatchSize
	}
	if opts.Lease <= 0 {
		opts.Lease = defaultPublicationLease
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = defaultPublicationRetryBase
	}
	if opts.RetryMax < opts.RetryBase {
		opts.RetryMax = defaultPublicationRetryMax
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &PublicationWorker{db: db, publisher: publisher, logger: opts.Logger, now: opts.Now, opts: opts}
}

type claimedPublication struct {
	ID          int64
	OrgID       int64
	OrgRevision int64
	Scope       string
	TargetID    string
	Status      string
	Attempts    int
}

// Run 阻塞直到 ctx 结束，每个 interval 触发一轮 RunOnce。interval <= 0 时用
// 1s。错误只记录：发布 loop 是后台收敛过程，不能因为一次数据库抖动退出。
func (w *PublicationWorker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Printf("console policy publication: %v", err)
			}
		}
	}
}

// RunOnce 处理一个有界批次，返回处理过的行数（包括被删除的过期行）。它不持有
// 任何行锁调用 Publisher。
func (w *PublicationWorker) RunOnce(ctx context.Context) (int, error) {
	if w.publisher == nil {
		return 0, errors.New("organization: publication worker has no publisher")
	}
	now := w.now()
	claimed, err := w.claim(ctx, now)
	if err != nil {
		return 0, err
	}

	processed := 0
	for _, row := range claimed {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		if err := w.publish(ctx, row, now); err != nil {
			// 单行失败不阻断批次：其余行继续，失败行已按退避落盘。
			w.logger.Printf("console policy publication: org=%d revision=%d scope=%s target=%q err=%v",
				row.OrgID, row.OrgRevision, row.Scope, row.TargetID, err)
		}
		processed++
	}
	return processed, nil
}

// claim 在一个短事务里选出批次并把它们标记为“已声明”（attempts+1 且
// next_retry_at = now+lease），然后立即提交。行锁绝不在 Publisher 调用期间持有。
func (w *PublicationWorker) claim(ctx context.Context, now time.Time) ([]claimedPublication, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, queryLockPublicationBatch, now, w.opts.BatchSize)
	if err != nil {
		return nil, err
	}
	var claimed []claimedPublication
	for rows.Next() {
		var row claimedPublication
		if err := rows.Scan(&row.ID, &row.OrgID, &row.OrgRevision, &row.Scope, &row.TargetID, &row.Status, &row.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	leaseUntil := now.Add(w.opts.Lease)
	for _, row := range claimed {
		if _, err := tx.ExecContext(ctx, queryClaimPublications, row.ID, leaseUntil, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

// publish 处理一行：先比较 revision，再调用 Publisher，最后用一个短事务落盘。
func (w *PublicationWorker) publish(ctx context.Context, row claimedPublication, now time.Time) error {
	current, err := w.orgRevision(ctx, row.OrgID)
	if errors.Is(err, sql.ErrNoRows) {
		// 组织已不存在：外键级联会删掉这一行，这里做一次防御性清理。
		return w.deleteRow(ctx, row.ID)
	}
	if err != nil {
		return w.markFailed(ctx, row, now, err)
	}
	if row.OrgRevision < current {
		// 旧 revision 不得覆盖新状态：被取代的行直接删除（见文件头注释）。
		return w.deleteRow(ctx, row.ID)
	}
	if row.OrgRevision > current {
		return w.markFailed(ctx, row, now,
			fmt.Errorf("publication revision %d is ahead of organization revision %d", row.OrgRevision, current))
	}

	request := PublishRequest{
		PublicationID: row.ID,
		OperationID:   publicationOperationID(row),
		OrgID:         row.OrgID,
		OrgRevision:   row.OrgRevision,
		Scope:         row.Scope,
		TargetID:      row.TargetID,
	}
	if err := w.publisher.PublishOrgPolicy(ctx, request); err != nil {
		// 发布过程中组织状态又前进了：这一行描述的是旧 revision，重试只会再次
		// 发现被取代。直接删除，让新 revision 的那一行去收敛（与上面的
		// revision 比较分支同一语义）。
		if errors.Is(err, ErrPublicationSuperseded) {
			return w.deleteRow(ctx, row.ID)
		}
		return w.markFailed(ctx, row, now, err)
	}
	if _, err := w.db.ExecContext(ctx, queryMarkPublicationPublished, row.ID, request.OperationID, w.now()); err != nil {
		return err
	}
	return nil
}

func (w *PublicationWorker) orgRevision(ctx context.Context, orgID int64) (int64, error) {
	var revision int64
	err := w.db.QueryRowContext(ctx, queryGetOrgRevision, orgID).Scan(&revision)
	return revision, err
}

// markFailed 记录失败并按 attempts 做有界指数退避：下一次可重试时间写进
// next_retry_at，查询这一列即可看到“什么时候再试”。
func (w *PublicationWorker) markFailed(ctx context.Context, row claimedPublication, now time.Time, failure error) error {
	attempts := row.Attempts + 1 // claim 已经 +1，这里用同一个口径计算退避
	delay := w.opts.RetryBase
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= w.opts.RetryMax {
			delay = w.opts.RetryMax
			break
		}
	}
	_, err := w.db.ExecContext(ctx, queryMarkPublicationFailed,
		row.ID, truncateError(failure), now.Add(delay), now)
	return err
}

func (w *PublicationWorker) deleteRow(ctx context.Context, id int64) error {
	_, err := w.db.ExecContext(ctx, queryDeletePublication, id)
	return err
}

// publicationOperationID 是确定性幂等键：同一个 (org, revision, scope, target)
// 永远是同一个 operation_id，重试不会在 Control 侧产生第二次效果。
func publicationOperationID(row claimedPublication) string {
	return fmt.Sprintf("policy:%d:%d:%s:%s", row.OrgID, row.OrgRevision, row.Scope, row.TargetID)
}

// truncateError 把错误文本限制在 last_error 的合理长度内（TEXT 无上限，但错误
// 上下文不需要无限长），并且绝不包含 token/密码（调用方只传发布错误）。
func truncateError(err error) string {
	const maxErrorBytes = 512
	message := err.Error()
	if len(message) <= maxErrorBytes {
		return message
	}
	return message[:maxErrorBytes] + "..."
}
