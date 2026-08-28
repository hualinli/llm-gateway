package metering

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

const StreamName = "usage:events"

// Producer 异步批量把 Event 推入 Redis Stream，不阻塞代理热路径
type Producer struct {
	rdb      *redis.Client
	ch       chan Event
	batch    int
	interval time.Duration
}

func NewProducer(rdb *redis.Client, buffer int, batch int, interval time.Duration) *Producer {
	if buffer <= 0 {
		buffer = 1000
	}
	if batch <= 0 {
		batch = 50
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	p := &Producer{
		rdb:      rdb,
		ch:       make(chan Event, buffer),
		batch:    batch,
		interval: interval,
	}
	go p.loop()
	return p
}

// Enqueue 非阻塞入队，队列满时丢弃并告警（后续可落本地 spool）
func (p *Producer) Enqueue(e Event) {
	select {
	case p.ch <- e:
	default:
		log.Printf("metering queue full, drop request_id=%s", e.RequestID)
	}
}

func (p *Producer) loop() {
	buf := make([]Event, 0, p.batch)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pipe := p.rdb.Pipeline()
		for _, e := range buf {
			b, _ := json.Marshal(e)
			// 用 RequestID 作 Stream ID 的幂等辅助，真正去重靠消费端唯一约束
			pipe.XAdd(ctx, &redis.XAddArgs{
				Stream: StreamName,
				Values: map[string]interface{}{"data": string(b)},
			})
		}
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("metering flush failed batch=%d err=%v", len(buf), err)
			// 失败不重入队，避免热路径阻塞；后续靠对账补
		}
		buf = buf[:0]
	}

	for {
		select {
		case e := <-p.ch:
			buf = append(buf, e)
			if len(buf) >= p.batch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
