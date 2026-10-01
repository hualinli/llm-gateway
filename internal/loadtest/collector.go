package loadtest

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// MaxLoadgenDroppedPercent is the generator-validity threshold from
// docs/plans/BENCHMARK-AND-ACCEPTANCE-2026-10-01.md §2.1: when the load
// generator itself drops more than this share of scheduled arrivals, the round
// says nothing about the capacity of the system under test and must be re-run
// with a bigger generator (more resources or a separate load host).
const MaxLoadgenDroppedPercent = 0.1

// MinArrivalRateRatio is the schedule-keeping guard: when the generator
// delivers arrivals at less than this share of the planned rate, the round
// silently reduced the offered load (the historical blocking scheduler did
// exactly that) and is likewise not a capacity measurement.
const MinArrivalRateRatio = 0.99

// CapacityRule is embedded verbatim in every JSON report so the rule travels
// with the numbers instead of living only in code comments.
const CapacityRule = "generator-side drops above 0.1% of scheduled arrivals, " +
	"and arrivals delivered below 99% of the planned rate, invalidate that round for capacity " +
	"conclusions; re-run with more generator resources or a separate load host " +
	"(BENCHMARK-AND-ACCEPTANCE-2026-10-01 §2.1)"

const httpStatusOK = http.StatusOK

// Latency summarizes one duration distribution. Count is the number of
// observations that actually carried the metric, so a zero Count means "no
// observation" rather than "zero latency".
type Latency struct {
	Count int
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	Min   time.Duration
	Max   time.Duration
	Avg   time.Duration
}

// Stats is the scheduling-side truth produced by the Scheduler. It is kept
// separate from the request observations so a scheduler bug cannot hide inside
// the result aggregates.
type Stats struct {
	Mode              Mode
	Scheduled         int
	Started           int
	NotScheduled      int
	Abandoned         int
	LoadgenDropped    int
	DroppedNoFreeSlot int
	DroppedQueueFull  int
	PeakActive        int
	ActiveAtEnd       int
	GoroutinesPeak    int

	PlannedStartRatePerSecond  float64
	ArrivalRatePerSecond       float64
	AchievedStartRatePerSecond float64

	ArrivalWindowSeconds float64
	StartWindowSeconds   float64
	RunSeconds           float64

	ArrivalLag          Latency
	DispatchLag         Latency
	ScheduledToTerminal Latency
}

// Collector aggregates request observations and merges the scheduler stats into
// one report. It is safe for concurrent use.
type Collector struct {
	mu sync.Mutex

	added   int
	status  map[int]int
	classes map[StreamClass]int
	cancel  map[string]int

	firstEvents    []time.Duration
	firstContents  []time.Duration
	streamDuration []time.Duration
	totals         []time.Duration
	chunks         []int
	contentEvents  []int
	bytes          int64

	stats Stats
}

func NewCollector(capacity int) *Collector {
	if capacity < 0 {
		capacity = 0
	}
	return &Collector{
		status:         make(map[int]int),
		classes:        make(map[StreamClass]int),
		cancel:         make(map[string]int),
		firstEvents:    make([]time.Duration, 0, capacity),
		firstContents:  make([]time.Duration, 0, capacity),
		streamDuration: make([]time.Duration, 0, capacity),
		totals:         make([]time.Duration, 0, capacity),
		chunks:         make([]int, 0, capacity),
		contentEvents:  make([]int, 0, capacity),
	}
}

// Add records one finished request. A Result with an empty Class is classified
// from Status/Err, which keeps historical callers (and tests) working.
func (c *Collector) Add(r Result) {
	class := r.Class
	if class == "" {
		switch {
		case r.Err != nil:
			class = ClassTransportError
		case r.Status == httpStatusOK:
			class = ClassCompleted
		case r.Status != 0:
			class = ClassRejected
		default:
			class = ClassTransportError
		}
	}
	events := r.Events
	if events == 0 {
		events = r.Chunks
	}
	firstEvent := r.FirstEvent
	if firstEvent == 0 {
		firstEvent = r.TTFT
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.added++
	c.classes[class]++
	if r.Status != 0 {
		c.status[r.Status]++
	}
	if class == ClassCancelled {
		reason := r.CancelReason
		if reason == "" {
			reason = CancelExplicit
		}
		c.cancel[reason]++
	}
	if firstEvent > 0 {
		c.firstEvents = append(c.firstEvents, firstEvent)
	}
	if r.FirstContent > 0 {
		c.firstContents = append(c.firstContents, r.FirstContent)
	}
	// Network distributions cover every request that reached an HTTP 200
	// stream (completed, incomplete, cancelled mid-stream, parse error), not
	// only successful ones: a truncated stream still measured real latency.
	// Rejections and transport failures are excluded so a fast 429 never
	// pollutes stream latency.
	if class != ClassRejected && class != ClassTransportError && r.Total > 0 {
		c.totals = append(c.totals, r.Total)
		c.chunks = append(c.chunks, events)
		c.contentEvents = append(c.contentEvents, r.ContentEvents)
		c.streamDuration = append(c.streamDuration, r.Total)
	}
	c.bytes += r.Bytes
}

// SetStats attaches the scheduler-side accounting to the next Report.
func (c *Collector) SetStats(stats Stats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats = stats
}

// Report produces the merged, JSON-ready summary.
type Report struct {
	Mode string

	// Historical keys. Total is the number of started requests and
	// Success is the number of streams that ended with [DONE]; Errors counts
	// every started request that did not complete cleanly on the client side
	// (transport error, incomplete, cancelled, parse error) and excludes
	// rejected statuses, which are reported separately.
	Total   int
	Success int
	Errors  int
	Status  map[int]int

	// TTFT is the historical name of FirstEvent and carries the same numbers.
	TTFT      Latency
	TotalDur  Latency
	AvgChunks float64

	// Open-loop accounting.
	Scheduled              int
	Started                int
	Completed              int
	Rejected               int
	TransportError         int
	Incomplete             int
	Cancelled              int
	ParseError             int
	NotScheduled           int
	Abandoned              int
	LoadgenDropped         int
	DroppedNoFreeSlot      int
	DroppedQueueFull       int
	CancelExplicit         int
	CancelTimeout          int
	PeakActive             int
	ActiveAtEnd            int
	GoroutinesPeak         int
	LoadgenDroppedPercent  float64
	CapacityRule           string
	CapacityRoundValid     bool
	CapacityInvalidReasons []string

	// Rates. Planned is the configured target; Arrival is what the generator
	// actually delivered; AchievedStart is what the system under test could
	// start; Success is completed streams per second over the whole run.
	PlannedStartRatePerSecond  float64
	ArrivalRatePerSecond       float64
	AchievedStartRatePerSecond float64
	SuccessPerSecond           float64
	ArrivalWindowSeconds       float64
	StartWindowSeconds         float64
	RunSeconds                 float64

	// Latencies. FirstEvent/FirstContent are measured from dispatch to the
	// network; DispatchLag is scheduled-to-started and ScheduledToTerminal is
	// scheduled-to-terminal, so queueing time is never folded into the network
	// metrics.
	FirstEvent          Latency
	FirstContent        Latency
	StreamDuration      Latency
	DispatchLag         Latency
	ScheduledToTerminal Latency
	ArrivalLag          Latency

	AvgEvents         float64
	AvgContentEvents  float64
	AvgBytesPerStream float64
	BytesRead         int64
}

func (c *Collector) Report() Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	var rep Report
	rep.Mode = string(c.stats.Mode)
	if c.stats.Mode == "" {
		rep.Mode = "closed"
	}
	rep.Status = make(map[int]int, len(c.status))
	for code, count := range c.status {
		rep.Status[code] = count
	}

	rep.Started = c.added
	rep.Completed = c.classes[ClassCompleted]
	rep.Rejected = c.classes[ClassRejected]
	rep.TransportError = c.classes[ClassTransportError]
	rep.Incomplete = c.classes[ClassIncomplete]
	rep.Cancelled = c.classes[ClassCancelled]
	rep.ParseError = c.classes[ClassParseError]
	rep.CancelExplicit = c.cancel[CancelExplicit]
	rep.CancelTimeout = c.cancel[CancelTimeout]
	rep.Total = c.added
	rep.Success = rep.Completed
	rep.Errors = rep.TransportError + rep.Incomplete + rep.Cancelled + rep.ParseError

	rep.Scheduled = c.stats.Scheduled
	rep.NotScheduled = c.stats.NotScheduled
	rep.Abandoned = c.stats.Abandoned
	rep.LoadgenDropped = c.stats.LoadgenDropped
	rep.DroppedNoFreeSlot = c.stats.DroppedNoFreeSlot
	rep.DroppedQueueFull = c.stats.DroppedQueueFull
	rep.PeakActive = c.stats.PeakActive
	rep.ActiveAtEnd = c.stats.ActiveAtEnd
	rep.GoroutinesPeak = c.stats.GoroutinesPeak
	rep.PlannedStartRatePerSecond = c.stats.PlannedStartRatePerSecond
	rep.ArrivalRatePerSecond = c.stats.ArrivalRatePerSecond
	rep.AchievedStartRatePerSecond = c.stats.AchievedStartRatePerSecond
	rep.ArrivalWindowSeconds = c.stats.ArrivalWindowSeconds
	rep.StartWindowSeconds = c.stats.StartWindowSeconds
	rep.RunSeconds = c.stats.RunSeconds
	rep.ArrivalLag = c.stats.ArrivalLag
	rep.DispatchLag = c.stats.DispatchLag
	rep.ScheduledToTerminal = c.stats.ScheduledToTerminal
	if rep.RunSeconds > 0 {
		rep.SuccessPerSecond = float64(rep.Completed) / rep.RunSeconds
	}

	rep.FirstEvent = summarize(c.firstEvents)
	rep.TTFT = rep.FirstEvent
	rep.FirstContent = summarize(c.firstContents)
	rep.TotalDur = summarize(c.totals)
	rep.StreamDuration = summarize(c.streamDuration)
	if len(c.chunks) > 0 {
		var sum int
		for _, value := range c.chunks {
			sum += value
		}
		rep.AvgChunks = float64(sum) / float64(len(c.chunks))
		rep.AvgEvents = rep.AvgChunks
	}
	if len(c.contentEvents) > 0 {
		var sum int
		for _, value := range c.contentEvents {
			sum += value
		}
		rep.AvgContentEvents = float64(sum) / float64(len(c.contentEvents))
	}
	rep.BytesRead = c.bytes
	if rep.Started > 0 {
		rep.AvgBytesPerStream = float64(c.bytes) / float64(rep.Started)
	}

	rep.CapacityRule = CapacityRule
	rep.evaluateCapacityValidity(c.stats)
	return rep
}

// evaluateCapacityValidity applies the published generator-validity rules.
func (r *Report) evaluateCapacityValidity(stats Stats) {
	reasons := make([]string, 0, 4)
	if r.Scheduled > 0 {
		r.LoadgenDroppedPercent = 100 * float64(r.LoadgenDropped) / float64(r.Scheduled)
		if r.LoadgenDroppedPercent > MaxLoadgenDroppedPercent {
			reasons = append(reasons, fmt.Sprintf(
				"loadgen_dropped %d/%d = %.3f%% > %.1f%%: the generator shaped this round, not the system under test",
				r.LoadgenDropped, r.Scheduled, r.LoadgenDroppedPercent, MaxLoadgenDroppedPercent))
		}
	}
	if r.PlannedStartRatePerSecond > 0 && r.ArrivalRatePerSecond > 0 {
		if r.ArrivalRatePerSecond < MinArrivalRateRatio*r.PlannedStartRatePerSecond {
			reasons = append(reasons, fmt.Sprintf(
				"arrival rate %.2f/s < %.0f%% of the planned %.2f/s: the schedule itself was stretched, so the offered load was lower than configured",
				r.ArrivalRatePerSecond, MinArrivalRateRatio*100, r.PlannedStartRatePerSecond))
		}
	}
	if r.Abandoned > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d admitted arrivals never started; the run was cut short before draining the admission queue", r.Abandoned))
	}
	if r.NotScheduled > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d planned arrivals were never scheduled; the run was cancelled early", r.NotScheduled))
	}
	if stats.Started > 0 && stats.Started != r.Started {
		reasons = append(reasons, fmt.Sprintf(
			"scheduler started %d requests but only %d observations were recorded", stats.Started, r.Started))
	}
	if r.Scheduled == 0 {
		reasons = append(reasons, "no arrivals were scheduled")
	}
	r.CapacityRoundValid = len(reasons) == 0
	r.CapacityInvalidReasons = reasons
}

func summarize(ds []time.Duration) Latency {
	var lat Latency
	if len(ds) == 0 {
		return lat
	}
	ordered := append([]time.Duration(nil), ds...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	lat.Count = len(ordered)
	lat.Min = ordered[0]
	lat.Max = ordered[len(ordered)-1]
	var sum time.Duration
	for _, d := range ordered {
		sum += d
	}
	lat.Avg = sum / time.Duration(len(ordered))
	lat.P50 = ordered[len(ordered)*50/100]
	lat.P95 = ordered[(len(ordered)*95)/100]
	lat.P99 = ordered[(len(ordered)*99)/100]
	return lat
}
