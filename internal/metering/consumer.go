package metering

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	GroupName    = "metering-group"
	ConsumerName = "consumer-1"
)

// Consumer 从 Stream 拉事件，幂等落台账，成功后 XACK
type Consumer struct {
	rdb    *redis.Client
	ledger Ledger
}

func NewConsumer(rdb *redis.Client, ledger Ledger) *Consumer {
	return &Consumer{rdb: rdb, ledger: ledger}
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
			Count:    50,
			Block:    2 * time.Second,
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

func (c *Consumer) handle(ctx context.Context, m redis.XMessage) {
	raw, _ := m.Values["data"].(string)
	var e Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		log.Printf("bad event id=%s err=%v", m.ID, err)
		_ = c.rdb.XAck(ctx, StreamName, GroupName, m.ID).Err()
		return
	}
	added, _ := c.ledger.Add(e)
	if added {
		log.Printf("ledger add request_id=%s tenant=%s cost=%d", e.RequestID, e.TenantID, e.CostCents)
	} else {
		log.Printf("ledger dedup request_id=%s", e.RequestID)
	}
	_ = c.rdb.XAck(ctx, StreamName, GroupName, m.ID).Err()
}

// reclaimLoop 每 10s 用 XAUTOCLAIM 认领超时未 ACK 的 pending
func (c *Consumer) reclaimLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
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
				MinIdle:  30 * time.Second,
				Start:    "0-0",
				Count:    50,
			}).Result()
			if err != nil || len(msgs) == 0 {
				continue
			}
			for _, m := range msgs {
				c.handle(ctx, m)
			}
		}
	}
}
