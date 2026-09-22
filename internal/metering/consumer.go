package metering

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/metrics"

	"github.com/redis/go-redis/v9"
)

const (
	GroupName = "metering-group"
	// ConsumerName is the historical fixed name. New processes use
	// InstanceConsumerName unless ConsumerOptions.Name is set.
	ConsumerName         = "consumer-1"
	DLQStreamName        = StreamName + ":dlq"
	consumerErrorBackoff = 100 * time.Millisecond
)

// Consumer 从 Stream 拉事件，幂等落台账，成功后 XACK
type Consumer struct {
	rdb    *redis.Client
	ledger Ledger
	name   string
	block  time.Duration
	count  int64
	claim  time.Duration
	idle   time.Duration
}

type ConsumerOptions struct {
	// Name is this process's consumer identity inside the group.
	// Empty uses InstanceConsumerName so two processes do not share a PEL.
	Name           string
	ReadBlock      time.Duration
	ReadCount      int64
	ReclaimEvery   time.Duration
	PendingMinIdle time.Duration
}

func NewConsumer(rdb *redis.Client, ledger Ledger) *Consumer {
	return NewConsumerWithOptions(rdb, ledger, ConsumerOptions{})
}

func NewConsumerWithOptions(rdb *redis.Client, ledger Ledger, options ConsumerOptions) *Consumer {
	if options.ReadBlock <= 0 {
		options.ReadBlock = 2 * time.Second
	}
	if options.ReadCount <= 0 {
		options.ReadCount = 50
	}
	if options.ReclaimEvery <= 0 {
		options.ReclaimEvery = 10 * time.Second
	}
	if options.PendingMinIdle <= 0 {
		options.PendingMinIdle = 30 * time.Second
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name = InstanceConsumerName()
	}
	return &Consumer{
		rdb:    rdb,
		ledger: ledger,
		name:   name,
		block:  options.ReadBlock,
		count:  options.ReadCount,
		claim:  options.ReclaimEvery,
		idle:   options.PendingMinIdle,
	}
}

// Name is the Redis consumer-group member used for reads and claims.
func (c *Consumer) Name() string {
	return c.name
}

// InstanceConsumerName identifies one OS process. Two processes on the same
// host get different names because the pid differs.
func InstanceConsumerName() string {
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	host = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, host)
	host = strings.Trim(host, "-")
	if host == "" {
		host = "unknown"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}

// EnsureGroup 启动时建组，已存在则忽略
func (c *Consumer) EnsureGroup(ctx context.Context) {
	_ = c.rdb.XGroupCreateMkStream(ctx, StreamName, GroupName, "0").Err()
	// BUSYGROUP 已存在不算错
}

// Run 阻塞消费，ctx 取消即退出
func (c *Consumer) Run(ctx context.Context) {
	c.EnsureGroup(ctx)
	log.Printf("metering consumer group=%s consumer=%s stream=%s", GroupName, c.name, StreamName)

	// 后台定期认领 pending（崩溃后重放）
	go c.reclaimLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// XREADGROUP 阻塞 2s，无消息则继续
		streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    GroupName,
			Consumer: c.name,
			Streams:  []string{StreamName, ">"},
			Count:    c.count,
			Block:    c.block,
		}).Result()
		if err != nil && err != redis.Nil {
			// Redis 故障时不要 tight-loop；消息仍由 Redis Stream 保留，
			// 等待退避后重新读取，不改变 pending/ACK 语义。
			if !waitConsumerRetry(ctx, consumerErrorBackoff) {
				return
			}
			continue
		}
		for _, s := range streams {
			for _, m := range s.Messages {
				c.handle(ctx, m)
			}
		}
	}
}

func waitConsumerRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// handle 处理单条消息:
//   - 字段缺失/解析异常 -> 写入 DLQ 成功后 ack,避免毒丸无限重投且不丢原始数据
//     (DLQ 写入失败则不 ack,留在 PEL 由 XAutoClaim 重试"搬进 DLQ"本身)
//   - ledger 写入失败   -> 不 ack,留在 PEL 由 reclaimLoop 经 XAutoClaim 重投
//     (PGLedger 用 ON CONFLICT DO NOTHING 保证重投幂等)
//   - 成功/去重         -> ack
func (c *Consumer) handle(ctx context.Context, m redis.XMessage) {
	raw, ok := m.Values["data"].(string)
	if !ok {
		log.Printf("metering: missing/invalid data field id=%s", m.ID)
		c.deadLetter(ctx, m, "missing_field:data")
		return
	}
	var e Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		log.Printf("metering: bad event id=%s err=%v", m.ID, err)
		c.deadLetter(ctx, m, "bad_event: "+err.Error())
		return
	}
	if e.UsageMissing || e.UsageInvalid {
		// Keep the original event in DLQ for investigation/reconciliation;
		// never silently record a successful request as a zero-cost charge or
		// allow invalid usage to create a negative charge.
		reason := "usage_missing"
		if e.UsageInvalid {
			reason = "usage_invalid"
		}
		log.Printf("metering: %s id=%s event_id=%s", reason, m.ID, e.EventID)
		c.deadLetter(ctx, m, reason)
		return
	}

	added, err := c.ledger.Add(e)
	if err != nil {
		// 写入失败:不 ack,保留在 PEL 等待重投
		metrics.MeteringLedgerAddFailedTotal.Inc()
		log.Printf("metering: ledger add failed event_id=%s err=%v", e.EventID, err)
		return
	}
	if added {
		log.Printf("ledger add event_id=%s client_request_id=%s trace_id=%s tenant=%s cost=%d", e.EventID, e.ClientRequestID, e.TraceID, e.TenantID, e.CostCents)
	} else {
		log.Printf("ledger dedup event_id=%s client_request_id=%s trace_id=%s", e.EventID, e.ClientRequestID, e.TraceID)
	}
	if err := c.rdb.XAck(ctx, StreamName, GroupName, m.ID).Err(); err != nil {
		log.Printf("metering: xack failed id=%s err=%v", m.ID, err)
	}
}

// deadLetter 把无法处理的原始消息搬进 DLQ,成功后才 ack。
// 写入失败时不 ack:XAdd 与 XAck 走同一 Redis,写不进 DLQ 时 ACK 大概率同样失败,
// 消息留在 PEL 由 XAutoClaim 重试整个"搬进 DLQ"动作,退化为 fail-stop 而非静默丢单。
func (c *Consumer) deadLetter(ctx context.Context, m redis.XMessage, reason string) {
	b, _ := json.Marshal(m.Values) // 原始字段整体转 JSON 存档,缺字段时也能还原现场
	// 死信是人工补单队列而非审计日志,有界(approximate trimming ~1 万条)可接受
	if err := c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: DLQStreamName,
		MaxLen: 10000,
		Approx: true,
		Values: map[string]any{"original_id": m.ID, "reason": reason, "raw": string(b)},
	}).Err(); err != nil {
		log.Printf("metering: DLQ write failed, keeping pending id=%s reason=%s err=%v", m.ID, reason, err)
		return
	}
	metrics.MeteringDLQTotal.Inc()
	if err := c.rdb.XAck(ctx, StreamName, GroupName, m.ID).Err(); err != nil {
		log.Printf("metering: xack failed id=%s err=%v", m.ID, err)
	}
}

// reclaimLoop 每 10s 用 XAUTOCLAIM 认领超时未 ACK 的 pending
func (c *Consumer) reclaimLoop(ctx context.Context) {
	ticker := time.NewTicker(c.claim)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 尝试认领 Idle >30s 的 pending
			msgs, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
				Stream:   StreamName,
				Group:    GroupName,
				Consumer: c.name,
				MinIdle:  c.idle,
				Start:    "0-0",
				Count:    c.count,
			}).Result()
			if err != nil || len(msgs) == 0 {
				continue
			}
			metrics.MeteringPendingClaimTotal.Add(float64(len(msgs)))
			for _, m := range msgs {
				c.handle(ctx, m)
			}
		}
	}
}
