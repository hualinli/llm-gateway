package loadtest

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"
)

// Mode selects the arrival scheduler. It is flag-selectable so the historical
// blocking behaviour stays available as the A side of the B00 comparison.
type Mode string

const (
	// ModeClosed keeps exactly Concurrency streams in flight and starts the
	// next one as soon as a slot frees (fixed active streams, resource upper
	// bound). No external arrival schedule exists.
	ModeClosed Mode = "closed"
	// ModeOpen is true open-loop scheduling: every arrival keeps its planned
	// monotonic timestamp, admission is bounded, and a full admission queue
	// produces an explicit drop instead of stalling the schedule.
	ModeOpen Mode = "open"
	// ModeFixed reproduces the historical behaviour: a ticker drives arrivals
	// and the scheduler blocks on the concurrency semaphore when slots are
	// full, so the schedule silently stretches. It exists to demonstrate the
	// B00 A side and is not for capacity measurements.
	ModeFixed Mode = "fixed"
)

// Drop reasons.
const (
	DropNoFreeSlot = "no_free_slot"
	DropQueueFull  = "queue_full"
)

// Executor runs one admitted arrival. Worker implements it; tests inject a
// deterministic stand-in.
type Executor interface {
	Do(ctx context.Context) Result
}

// SchedulerConfig describes one load run.
type SchedulerConfig struct {
	Mode        Mode
	Total       int
	Concurrency int
	// Rate is the planned arrival rate in arrivals per second for ModeOpen and
	// ModeFixed. ModeClosed ignores it.
	Rate float64
	// QueueSize is the bounded admission queue used by ModeOpen. Zero means
	// "admit only when a slot is free right now, otherwise drop immediately".
	QueueSize int
	// Timeout is the per-request timeout. It starts when the request is
	// dispatched, not when it is scheduled, so queueing is never charged to
	// the stream. Zero disables it.
	Timeout   time.Duration
	Executor  Executor
	Collector *Collector
	// Clock overrides the monotonic clock; nil uses SystemClock.
	Clock Clock
	// OnArrival runs after each arrival has been scheduled and accounted for,
	// before the scheduler waits for the next one. Tests use it to advance a
	// virtual clock deterministically; production leaves it nil.
	OnArrival func(index int, at time.Time)
}

// Scheduler runs the arrival schedule. A Scheduler is single-use.
type Scheduler struct {
	cfg   SchedulerConfig
	clock Clock

	mu        sync.Mutex
	stats     Stats
	active    int
	freeSlots int
	queue     []arrival
	inflight  sync.WaitGroup

	arrivalLag          []time.Duration
	dispatchLag         []time.Duration
	scheduledToTerminal []time.Duration

	firstScheduled time.Time
	lastScheduled  time.Time
	firstArrival   time.Time
	lastArrival    time.Time
	firstStart     time.Time
	lastStart      time.Time
	lastTerminal   time.Time
}

// NewScheduler builds a scheduler for one run.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Total < 0 {
		cfg.Total = 0
	}
	if cfg.QueueSize < 0 {
		cfg.QueueSize = 0
	}
	if cfg.Mode == "" {
		if cfg.Rate > 0 {
			cfg.Mode = ModeOpen
		} else {
			cfg.Mode = ModeClosed
		}
	}
	return &Scheduler{cfg: cfg, clock: clockOrSystem(cfg.Clock)}
}

type arrival struct {
	index     int
	scheduled time.Time
}

// Run executes the whole schedule and returns the scheduling-side accounting.
func (s *Scheduler) Run(ctx context.Context) (Stats, error) {
	switch s.cfg.Mode {
	case ModeClosed:
		s.runClosed(ctx)
	case ModeFixed:
		s.runFixed(ctx)
	default:
		s.runOpen(ctx)
	}
	return s.finish(), nil
}

// runOpen is the open-loop scheduler. Admission is decided by explicit slot
// tokens under one mutex, so the arrival loop can never block on concurrency:
// an arrival takes a free slot, waits in the bounded admission queue, or is
// dropped with a counted reason, and its scheduled timestamp is untouched.
func (s *Scheduler) runOpen(ctx context.Context) {
	start := s.clock.Now()
	s.mu.Lock()
	s.stats.Mode = ModeOpen
	s.stats.PlannedStartRatePerSecond = s.cfg.Rate
	s.freeSlots = s.cfg.Concurrency
	s.mu.Unlock()

	for index := 0; index < s.cfg.Total; index++ {
		if ctx.Err() != nil {
			s.recordNotScheduled(s.cfg.Total - index)
			break
		}
		scheduled := start.Add(offsetFor(index, s.cfg.Rate))
		s.waitUntil(scheduled)
		at := s.clock.Now()
		s.recordArrival(scheduled, at)
		if s.cfg.OnArrival != nil {
			s.cfg.OnArrival(index, at)
		}
		s.admit(ctx, arrival{index: index, scheduled: scheduled})
	}
	// Every dispatched request returns its slot in release(), which re-dispatches
	// the next queued arrival before the count can reach zero, so this also
	// drains the admission queue.
	s.inflight.Wait()
}

// admit takes a free slot, enqueues into the bounded admission queue, or drops.
func (s *Scheduler) admit(ctx context.Context, item arrival) {
	s.mu.Lock()
	if s.freeSlots > 0 {
		s.freeSlots--
		s.mu.Unlock()
		s.dispatch(ctx, item)
		return
	}
	if len(s.queue) < s.cfg.QueueSize {
		s.queue = append(s.queue, item)
		s.mu.Unlock()
		return
	}
	reason := DropNoFreeSlot
	if s.cfg.QueueSize > 0 {
		reason = DropQueueFull
	}
	s.mu.Unlock()
	s.recordDrop(reason)
}

// dispatch starts one admitted arrival and records the dispatch instant, which
// is what separates dispatch lag from network latency.
func (s *Scheduler) dispatch(ctx context.Context, item arrival) {
	started := s.clock.Now()
	s.recordStart(item, started)
	s.beginActive()
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		result := s.execute(ctx, item)
		finished := s.clock.Now()
		s.endActive()
		s.recordTerminal(item, finished, result)
		s.release(ctx)
	}()
}

// release returns a slot and immediately re-dispatches the oldest queued
// arrival, so a queued item never waits for a fresh arrival loop iteration.
func (s *Scheduler) release(ctx context.Context) {
	s.mu.Lock()
	if len(s.queue) > 0 {
		next := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		s.dispatch(ctx, next)
		return
	}
	s.freeSlots++
	s.mu.Unlock()
}

// runFixed keeps the historical ticker-plus-blocking-semaphore behaviour. It is
// deliberately still here: it is the A side that shows the old tool hiding
// saturation by stretching the schedule instead of dropping.
func (s *Scheduler) runFixed(ctx context.Context) {
	start := s.clock.Now()
	s.mu.Lock()
	s.stats.Mode = ModeFixed
	s.stats.PlannedStartRatePerSecond = s.cfg.Rate
	s.mu.Unlock()

	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup
	for index := 0; index < s.cfg.Total; index++ {
		if ctx.Err() != nil {
			s.recordNotScheduled(s.cfg.Total - index)
			break
		}
		scheduled := start.Add(offsetFor(index, s.cfg.Rate))
		s.waitUntil(scheduled)
		at := s.clock.Now()
		s.recordArrival(scheduled, at)
		if s.cfg.OnArrival != nil {
			s.cfg.OnArrival(index, at)
		}

		// This is the historical blocking point: when every slot is busy the
		// arrival loop stops producing, so the offered load silently drops.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			s.recordNotScheduled(s.cfg.Total - index)
			wg.Wait()
			return
		}
		item := arrival{index: index, scheduled: scheduled}
		started := s.clock.Now()
		s.recordStart(item, started)
		s.beginActive()
		wg.Add(1)
		go func(item arrival, started time.Time) {
			defer wg.Done()
			defer func() { <-sem }()
			result := s.execute(ctx, item)
			finished := s.clock.Now()
			s.endActive()
			s.recordTerminal(item, finished, result)
		}(item, started)
	}
	wg.Wait()
}

// runClosed keeps exactly Concurrency streams in flight: a slot that frees up
// is itself the next arrival, which is the fixed-active-stream model.
func (s *Scheduler) runClosed(ctx context.Context) {
	s.mu.Lock()
	s.stats.Mode = ModeClosed
	s.mu.Unlock()

	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				scheduled := s.clock.Now()
				started := s.clock.Now()
				item := arrival{index: index, scheduled: scheduled}
				s.recordArrival(scheduled, started)
				if s.cfg.OnArrival != nil {
					s.cfg.OnArrival(index, started)
				}
				s.recordStart(item, started)
				s.beginActive()
				result := s.execute(ctx, item)
				finished := s.clock.Now()
				s.endActive()
				s.recordTerminal(item, finished, result)
			}
		}()
	}

produce:
	for index := 0; index < s.cfg.Total; index++ {
		select {
		case jobs <- index:
		case <-ctx.Done():
			s.recordNotScheduled(s.cfg.Total - index)
			break produce
		}
	}
	close(jobs)
	wg.Wait()
}

func (s *Scheduler) execute(ctx context.Context, item arrival) Result {
	executor := s.cfg.Executor
	if executor == nil {
		return Result{Class: ClassCompleted}
	}
	reqCtx := ctx
	if s.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()
	}
	return executor.Do(reqCtx)
}

// waitUntil blocks until the clock reaches the deadline. It re-checks the
// deadline after arming the timer: a clock that jumps forward while the timer
// is being registered (a virtual clock in tests, or a stopped process) must not
// strand the arrival behind a timer that was armed too late.
func (s *Scheduler) waitUntil(deadline time.Time) {
	if !s.clock.Now().Before(deadline) {
		return
	}
	ch := s.clock.After(deadline.Sub(s.clock.Now()))
	if !s.clock.Now().Before(deadline) {
		return
	}
	<-ch
}

// offsetFor is the exact planned offset of arrival index: computing it from the
// index instead of accumulating an interval keeps the schedule from drifting.
func offsetFor(index int, rate float64) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(math.Round(float64(index) * float64(time.Second) / rate))
}

func (s *Scheduler) recordArrival(scheduled, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Scheduled++
	if s.firstScheduled.IsZero() {
		s.firstScheduled = scheduled
	}
	s.lastScheduled = scheduled
	if s.firstArrival.IsZero() {
		s.firstArrival = at
	}
	s.lastArrival = at
	s.arrivalLag = append(s.arrivalLag, nonNegative(at.Sub(scheduled)))
	// Sampling the generator's own goroutine count on every arrival would
	// perturb very high arrival rates, so it is sampled sparsely.
	if s.stats.Scheduled%64 == 1 {
		if goroutines := runtime.NumGoroutine(); goroutines > s.stats.GoroutinesPeak {
			s.stats.GoroutinesPeak = goroutines
		}
	}
}

func (s *Scheduler) recordDrop(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.LoadgenDropped++
	switch reason {
	case DropQueueFull:
		s.stats.DroppedQueueFull++
	default:
		s.stats.DroppedNoFreeSlot++
	}
}

func (s *Scheduler) recordNotScheduled(count int) {
	if count <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.NotScheduled += count
}

func (s *Scheduler) beginActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active++
	if s.active > s.stats.PeakActive {
		s.stats.PeakActive = s.active
	}
}

func (s *Scheduler) endActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
}

func (s *Scheduler) recordStart(item arrival, started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Started++
	if s.firstStart.IsZero() {
		s.firstStart = started
	}
	s.lastStart = started
	s.dispatchLag = append(s.dispatchLag, nonNegative(started.Sub(item.scheduled)))
}

func (s *Scheduler) recordTerminal(item arrival, finished time.Time, result Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastTerminal.Before(finished) {
		s.lastTerminal = finished
	}
	s.scheduledToTerminal = append(s.scheduledToTerminal, nonNegative(finished.Sub(item.scheduled)))
	if s.cfg.Collector != nil {
		s.cfg.Collector.Add(result)
	}
}

// finish summarizes the raw slices into the reportable Stats.
func (s *Scheduler) finish() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := s.stats
	stats.ActiveAtEnd = s.active
	stats.ArrivalLag = summarize(s.arrivalLag)
	stats.DispatchLag = summarize(s.dispatchLag)
	stats.ScheduledToTerminal = summarize(s.scheduledToTerminal)

	if !s.firstArrival.IsZero() && s.lastArrival.After(s.firstArrival) {
		stats.ArrivalWindowSeconds = s.lastArrival.Sub(s.firstArrival).Seconds()
	}
	if !s.firstStart.IsZero() && s.lastStart.After(s.firstStart) {
		stats.StartWindowSeconds = s.lastStart.Sub(s.firstStart).Seconds()
	}
	if !s.firstScheduled.IsZero() && s.lastTerminal.After(s.firstScheduled) {
		stats.RunSeconds = s.lastTerminal.Sub(s.firstScheduled).Seconds()
	}

	// The closed-loop arrival "plan" is the demand itself, so a rate is only
	// meaningful when the caller configured one.
	if stats.Scheduled > 1 && stats.ArrivalWindowSeconds > 0 {
		stats.ArrivalRatePerSecond = float64(stats.Scheduled-1) / stats.ArrivalWindowSeconds
	} else if stats.Scheduled > 0 && !s.firstArrival.IsZero() && !s.lastTerminal.IsZero() {
		if window := s.lastTerminal.Sub(s.firstArrival).Seconds(); window > 0 {
			stats.ArrivalRatePerSecond = float64(stats.Scheduled) / window
		}
	}
	// The achieved start rate is normalized by the offered-load window so it is
	// directly comparable with the planned rate: "how many streams did the
	// system start per second while that load was being offered". The window
	// between the first and last start is reported separately because it
	// collapses to almost nothing when the system is saturated.
	switch {
	case stats.ArrivalWindowSeconds > 0:
		stats.AchievedStartRatePerSecond = float64(stats.Started) / stats.ArrivalWindowSeconds
	case stats.RunSeconds > 0:
		stats.AchievedStartRatePerSecond = float64(stats.Started) / stats.RunSeconds
	}

	// Admitted arrivals that neither started nor were counted as drops can
	// only exist when the run was interrupted while the queue still held work.
	if remaining := stats.Scheduled - stats.Started - stats.LoadgenDropped; remaining > 0 {
		stats.Abandoned = remaining
	}
	return stats
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
