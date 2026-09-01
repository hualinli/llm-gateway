package loadtest

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// Collector 聚合 N 次 Result，算分布。后续可替换为 hdrhistogram 做流式分位数
const httpStatusOK = http.StatusOK

type Collector struct {
	mu     sync.Mutex
	ttfts  []time.Duration
	totals []time.Duration
	chunks []int

	success int
	status  map[int]int
	errs    int
}

func NewCollector(capacity int) *Collector {
	if capacity < 0 {
		capacity = 0
	}
	return &Collector{
		ttfts:  make([]time.Duration, 0, capacity),
		totals: make([]time.Duration, 0, capacity),
		chunks: make([]int, 0, capacity),
		status: make(map[int]int),
	}
}

func (c *Collector) Add(r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Err != nil {
		c.errs++
		return
	}
	c.status[r.Status]++
	if r.Status != 200 {
		return
	}
	c.success++
	c.ttfts = append(c.ttfts, r.TTFT)
	c.totals = append(c.totals, r.Total)
	c.chunks = append(c.chunks, r.Chunks)
}

// Report 产出人类可读的摘要，后续 reporter 会再出 JSON/Prometheus 格式
type Report struct {
	Total   int
	Success int
	Errors  int
	Status  map[int]int

	TTFT struct {
		P50, P95, P99 time.Duration
		Min, Max, Avg time.Duration
	}
	TotalDur struct {
		P50, P95, P99 time.Duration
		Min, Max, Avg time.Duration
	}
	AvgChunks float64
}

func (c *Collector) Report() Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	var rep Report
	rep.Success = c.success
	rep.Errors = c.errs
	rep.Total = c.success + c.errs
	rep.Status = make(map[int]int, len(c.status))
	for code, count := range c.status {
		rep.Status[code] = count
		if code != httpStatusOK {
			rep.Total += count
		}
	}

	// Work on copies so Report does not reorder slices owned by Add and remains
	// safe if a caller reports while workers are still completing.
	ttfts := append([]time.Duration(nil), c.ttfts...)
	totals := append([]time.Duration(nil), c.totals...)
	chunks := append([]int(nil), c.chunks...)
	if len(ttfts) > 0 {
		rep.TTFT.P50, rep.TTFT.P95, rep.TTFT.P99, rep.TTFT.Min, rep.TTFT.Max, rep.TTFT.Avg = quantiles(ttfts)
	}
	if len(totals) > 0 {
		rep.TotalDur.P50, rep.TotalDur.P95, rep.TotalDur.P99, rep.TotalDur.Min, rep.TotalDur.Max, rep.TotalDur.Avg = quantiles(totals)
	}
	if len(chunks) > 0 {
		var sum int
		for _, value := range chunks {
			sum += value
		}
		rep.AvgChunks = float64(sum) / float64(len(chunks))
	}
	return rep
}

func quantiles(ds []time.Duration) (p50, p95, p99, min, max, avg time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	min = ds[0]
	max = ds[len(ds)-1]
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	avg = sum / time.Duration(len(ds))
	p50 = ds[len(ds)*50/100]
	p95 = ds[len(ds)*95/100]
	p99 = ds[len(ds)*99/100]
	return
}
