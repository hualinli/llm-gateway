package metering

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"llm-gateway/internal/metrics"

	"github.com/redis/go-redis/v9"
)

const (
	GroupName     = "metering-group"
	ConsumerName  = "consumer-1"
	DLQStreamName = StreamName + ":dlq"
)

// Consumer 从 Stream 拉事件，幂等落台账，成功后 XACK
type Consumer struct {
	rdb    *redis.Client
	ledger Ledger
	block  time.Duration
	count  int64
	claim  time.Duration
	idle   time.Duration
}

type ConsumerOptions struct {
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
	return &Consumer{
		rdb:    rdb,
		ledger: ledger,
		block:  options.ReadBlock,
		count:  options.ReadCount,
		claim:  options.ReclaimEvery,
		idle:   options.PendingMinIdle,
	}
}

// EnsureGroup 启动时建组，已存在则忽略
func (c *Consumer) EnsureGroup(ctx context.Context) {
	_ = c.rdb.XGroupCreateMkStream(ctx, StreamName, GroupName, "0").Err()
	// BUSYGROUP 已存在不算错
}

// Run 阻塞消费，ctx 取消即退出
func (c *Consumer) Run(ctx context.Context) {
	c.EnsureGroup(ctx)
	log.Printf("metering consumer group=%s consumer=%s stream=%s", GroupName, ConsumerName, StreamName)

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
			Consumer: ConsumerName,
			Streams:  []string{StreamName, ">"},
			Count:    c.count,
			Block:    c.block,
		}).Result()
		if err != nil && err != redis.Nil {
			// 超时或空流，继续
			continue
		}
		for _, s := range streams {
			for _, m := range s.Messages {
				c.handle(ctx, m)
			}
		}
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

	added, err := c.ledger.Add(e)
	if err != nil {
		// 写入失败:不 ack,保留在 PEL 等待重投
		metrics.MeteringLedgerAddFailedTotal.Inc()
		log.Printf("metering: ledger add failed request_id=%s err=%v", e.RequestID, err)
		return
	}
	if added {
		log.Printf("ledger add request_id=%s tenant=%s cost=%d", e.RequestID, e.TenantID, e.CostCents)
	} else {
		log.Printf("ledger dedup request_id=%s", e.RequestID)
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
				Consumer: ConsumerName,
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
