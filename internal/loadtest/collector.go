package loadtest

import (
	"sort"
	"sync"
	"time"
)

// Collector 聚合 N 次 Result，算分布。后续可替换为 hdrhistogram 做流式分位数
type Collector struct {
	mu     sync.Mutex
	ttfts  []time.Duration
	totals []time.Duration
	chunks []int

	success int
	status  map[int]int
	errs    int
}

func NewCollector(cap int) *Collector {
	return &Collector{
		ttfts:  make([]time.Duration, 0, cap),
		totals: make([]time.Duration, 0, cap),
		chunks: make([]int, 0, cap),
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
	var rep Report
	rep.Total = c.success + c.errs
	for k, v := range c.status {
		if rep.Status == nil {
			rep.Status = make(map[int]int)
		}
		rep.Status[k] = v
	}
	// 非 200 的也计入 total
	for _, v := range c.status {
		rep.Total += v
	}
	// 去重：success 已含 200，其余 status 叠加会多算，校正：
	rep.Total = c.success + c.errs
	for code, cnt := range c.status {
		if code != 200 {
			rep.Total += cnt
		}
	}
	rep.Success = c.success
	rep.Errors = c.errs

	if len(c.ttfts) > 0 {
		rep.TTFT.P50, rep.TTFT.P95, rep.TTFT.P99, rep.TTFT.Min, rep.TTFT.Max, rep.TTFT.Avg = quantiles(c.ttfts)
	}
	if len(c.totals) > 0 {
		rep.TotalDur.P50, rep.TotalDur.P95, rep.TotalDur.P99, rep.TotalDur.Min, rep.TotalDur.Max, rep.TotalDur.Avg = quantiles(c.totals)
	}
	if len(c.chunks) > 0 {
		var sum int
		for _, v := range c.chunks {
			sum += v
		}
		rep.AvgChunks = float64(sum) / float64(len(c.chunks))
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
