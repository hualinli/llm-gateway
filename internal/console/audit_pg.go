package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"
)

// PGSink 把审计事件写进 console_audit_events，并支持按组织分页读取
// （GET /api/v1/orgs/{org}/audit）。它是 AuditSink 与 AuditStore 的唯一生产实现。
//
// 约束（ADR-0002 §6、ADR-0003 §2.3）：
//   - 只写 actor/org/action/target/outcome 与 request/trace id，以及非秘密的
//     detail 元数据；明文 token、密码、CSRF token 永远不得进入本表；
//   - 写入使用独立的 2s 超时且不受请求取消影响：客户端断开不应该抹掉审计；
//   - 写入失败只记录日志，不改变已经提交的业务结果（业务事实已经发生，返回 500
//     会误导调用方）；因此审计是“尽力而为但必然可观测”的。
type PGSink struct {
	DB *sql.DB
	// Logger 接收写入失败；nil 表示 log.Default()。
	Logger *log.Logger
	// Timeout 是单次写入/读取的上限；<= 0 表示 2s。
	Timeout time.Duration
}

// NewPGSink 返回绑定到 db 的审计 sink。db 为 nil 时所有方法都是安全的 no-op /
// 明确错误，便于在未配置数据库时保持其它测试可用。
func NewPGSink(db *sql.DB) *PGSink { return &PGSink{DB: db} }

const defaultAuditTimeout = 2 * time.Second

// AuditRecord 是 console_audit_events 的一行（只读视图）。Detail 原样保留 JSON，
// 因此 API 不会因为 detail 的子结构变化而丢字段。
type AuditRecord struct {
	ID          int64
	ActorUserID *int64
	ActorOrgID  *int64
	Action      string
	TargetType  string
	TargetID    string
	Outcome     string
	Detail      json.RawMessage
	RequestID   *string
	TraceID     *string
	CreatedAt   time.Time
}

// AuditStore 是审计表的读入口（写入口是 AuditSink）。
type AuditStore interface {
	// List 返回某组织的审计事件，按 id DESC（最新在前）分页。beforeID 为 0 表示
	// 第一页；next 为空表示没有下一页。
	List(ctx context.Context, orgID int64, limit int, beforeID int64) ([]AuditRecord, string, error)
}

// unavailableAuditStore 是未配置审计存储时的占位实现：读取直接报错（HTTP 500），
// 而不是返回空列表假装“没有审计”。
type unavailableAuditStore struct{}

func (unavailableAuditStore) List(context.Context, int64, int, int64) ([]AuditRecord, string, error) {
	return nil, "", errors.New("console: audit store is not configured")
}

const (
	queryInsertAuditEvent = `INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	// 多取一行用于判断是否还有下一页；ORDER BY id DESC 与游标 id < beforeID 配套。
	queryListAuditEvents = `SELECT id, actor_user_id, actor_org_id, action, target_type, target_id,
       outcome, detail, request_id, trace_id, created_at
FROM console_audit_events
WHERE actor_org_id = $1 AND ($2 = 0 OR id < $2)
ORDER BY id DESC
LIMIT $3`
)

func (s *PGSink) logger() *log.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return log.Default()
}

func (s *PGSink) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return defaultAuditTimeout
}

// Record 实现 AuditSink。它从不 panic，也从不让审计失败影响响应。
func (s *PGSink) Record(ctx context.Context, event AuditEvent) {
	if s == nil || s.DB == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	var detail any
	if len(event.Detail) > 0 {
		encoded, err := json.Marshal(event.Detail)
		if err != nil {
			s.logger().Printf("console audit: encode detail action=%s err=%v", event.Action, err)
		} else {
			detail = encoded
		}
	}

	// 不受客户端断开影响：审计是服务端事实。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout())
	defer cancel()
	_, err := s.DB.ExecContext(writeCtx, queryInsertAuditEvent,
		nullableID(event.ActorUserID), nullableID(event.OrgID), event.Action,
		event.TargetType, event.TargetID, event.Outcome, detail,
		nullableText(event.RequestID), nullableText(event.TraceID), event.Time)
	if err != nil {
		s.logger().Printf("console audit: write failed action=%s request_id=%s err=%v",
			event.Action, event.RequestID, err)
	}
}

// List 实现 AuditStore：按 id DESC 返回，cursor 是本页最后一条 id。
func (s *PGSink) List(ctx context.Context, orgID int64, limit int, beforeID int64) ([]AuditRecord, string, error) {
	if s == nil || s.DB == nil {
		return nil, "", errors.New("console: audit store is not configured")
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	readCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	rows, err := s.DB.QueryContext(readCtx, queryListAuditEvents, orgID, beforeID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	items := make([]AuditRecord, 0, limit)
	for rows.Next() {
		var (
			record    AuditRecord
			actorUser sql.NullInt64
			actorOrg  sql.NullInt64
			detail    []byte
			requestID sql.NullString
			traceID   sql.NullString
		)
		if err := rows.Scan(&record.ID, &actorUser, &actorOrg, &record.Action, &record.TargetType,
			&record.TargetID, &record.Outcome, &detail, &requestID, &traceID, &record.CreatedAt); err != nil {
			return nil, "", err
		}
		if actorUser.Valid {
			value := actorUser.Int64
			record.ActorUserID = &value
		}
		if actorOrg.Valid {
			value := actorOrg.Int64
			record.ActorOrgID = &value
		}
		if detail != nil {
			record.Detail = json.RawMessage(detail)
		}
		if requestID.Valid {
			value := requestID.String
			record.RequestID = &value
		}
		if traceID.Valid {
			value := traceID.String
			record.TraceID = &value
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = formatID(items[limit-1].ID)
	}
	return items, next, nil
}

// nullableID 把 0 写成 SQL NULL：审计里“没有组织/没有 actor”与 id=0 是两回事。
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
