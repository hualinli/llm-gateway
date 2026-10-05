package loadtest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSchedulerOpenKeepsScheduleWhenEverySlotIsBusy is the core T01B property:
// with all slots occupied for the whole arrival window, arrivals keep their
// planned monotonic timestamps and the excess becomes explicit drops instead of
// stalling the schedule (the historical behaviour).
func TestSchedulerOpenKeepsScheduleWhenEverySlotIsBusy(t *testing.T) {
	clock := newFakeClock()
	executor := newBlockingExecutor()
	collector := NewCollector(8)
	arrivals := make(chan int, 8)

	scheduler := NewScheduler(SchedulerConfig{
		Mode:        ModeOpen,
		Total:       5,
		Concurrency: 2,
		Rate:        1000, // one arrival per virtual millisecond
		QueueSize:   0,    // admit only when a slot is free right now
		Executor:    executor,
		Collector:   collector,
		Clock:       clock,
		OnArrival:   func(index int, at time.Time) { arrivals <- index },
	})

	done := make(chan Stats, 1)
	go func() {
		stats, err := scheduler.Run(context.Background())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- stats
	}()

	// Arrivals 0 and 1 find a free slot and occupy it; the executor blocks for
	// the whole window, so 2..4 must be dropped without delaying the schedule.
	for index := 0; index < 2; index++ {
		if got := awaitIndex(t, arrivals, "arrival"); got != index {
			t.Fatalf("arrival order = %d, want %d", got, index)
		}
		if got := awaitIndex(t, executor.starts, "executor start"); got != index {
			t.Fatalf("start order = %d, want %d", got, index)
		}
		clock.Advance(time.Millisecond)
	}
	for index := 2; index < 5; index++ {
		if got := awaitIndex(t, arrivals, "arrival"); got != index {
			t.Fatalf("arrival order = %d, want %d", got, index)
		}
		clock.Advance(time.Millisecond)
	}
	if executor.callCount() != 2 {
		t.Fatalf("executor calls = %d, want 2 (the schedule must not start more than c)", executor.callCount())
	}
	for i := 0; i < 2; i++ {
		executor.release <- struct{}{}
	}
	stats := <-done

	if stats.Scheduled != 5 || stats.Started != 2 || stats.LoadgenDropped != 3 {
		t.Fatalf("scheduled=%d started=%d dropped=%d, want 5/2/3", stats.Scheduled, stats.Started, stats.LoadgenDropped)
	}
	if stats.DroppedNoFreeSlot != 3 || stats.DroppedQueueFull != 0 {
		t.Fatalf("drop reasons no_slot=%d queue_full=%d, want 3/0", stats.DroppedNoFreeSlot, stats.DroppedQueueFull)
	}
	if stats.PeakActive != 2 {
		t.Fatalf("peak active = %d, want 2", stats.PeakActive)
	}
	if stats.Abandoned != 0 || stats.NotScheduled != 0 {
		t.Fatalf("abandoned=%d not_scheduled=%d, want 0/0", stats.Abandoned, stats.NotScheduled)
	}
	// The schedule itself was never stretched: five arrivals landed exactly on
	// their planned timestamps.
	if stats.ArrivalLag.Max != 0 || stats.ArrivalLag.Count != 5 {
		t.Fatalf("arrival lag = %+v, want 5 observations at 0", stats.ArrivalLag)
	}
	if got := stats.ArrivalWindowSeconds; got != 0.004 {
		t.Fatalf("arrival window = %vs, want 0.004s", got)
	}
	if got := stats.ArrivalRatePerSecond; got != 1000 {
		t.Fatalf("arrival rate = %v/s, want 1000/s (the offered load was preserved)", got)
	}
	// Dispatch was immediate for the two admitted arrivals.
	if stats.DispatchLag.Count != 2 || stats.DispatchLag.Max != 0 {
		t.Fatalf("dispatch lag = %+v, want 2 observations at 0", stats.DispatchLag)
	}
	if stats.ScheduledToTerminal.Count != 2 {
		t.Fatalf("scheduled-to-terminal = %+v, want 2 observations", stats.ScheduledToTerminal)
	}

	collector.SetStats(stats)
	report := collector.Report()
	if report.CapacityRoundValid {
		t.Fatalf("capacity validity = true, want false: %v", report.CapacityInvalidReasons)
	}
	if report.LoadgenDropped != 3 || report.Scheduled != 5 || report.Started != 2 || report.Completed != 2 {
		t.Fatalf("report accounting = %+v", report)
	}
	if len(report.CapacityInvalidReasons) == 0 || !strings.Contains(report.CapacityInvalidReasons[0], "loadgen_dropped") {
		t.Fatalf("invalid reasons = %v, want the drop rule", report.CapacityInvalidReasons)
	}
}

// TestSchedulerOpenQueueAdmitsThenDropsExplicitly covers the bounded admission
// queue: queue slots absorb short bursts, a full queue is a counted drop, and
// the queue wait shows up as dispatch lag rather than as network latency.
func TestSchedulerOpenQueueAdmitsThenDropsExplicitly(t *testing.T) {
	clock := newFakeClock()
	executor := newBlockingExecutor()
	collector := NewCollector(8)
	arrivals := make(chan int, 8)

	scheduler := NewScheduler(SchedulerConfig{
		Mode:        ModeOpen,
		Total:       5,
		Concurrency: 2,
		Rate:        1000,
		QueueSize:   2,
		Executor:    executor,
		Collector:   collector,
		Clock:       clock,
		OnArrival:   func(index int, at time.Time) { arrivals <- index },
	})
	done := make(chan Stats, 1)
	go func() {
		stats, _ := scheduler.Run(context.Background())
		done <- stats
	}()

	for index := 0; index < 2; index++ {
		awaitIndex(t, arrivals, "arrival")
		awaitIndex(t, executor.starts, "executor start")
		clock.Advance(time.Millisecond)
	}
	// Arrivals 2 and 3 fit in the queue; arrival 4 finds it full.
	for index := 2; index < 5; index++ {
		awaitIndex(t, arrivals, "arrival")
		clock.Advance(time.Millisecond)
	}
	for i := 0; i < 4; i++ {
		executor.release <- struct{}{}
	}
	stats := <-done

	if stats.Scheduled != 5 || stats.Started != 4 || stats.LoadgenDropped != 1 {
		t.Fatalf("scheduled=%d started=%d dropped=%d, want 5/4/1", stats.Scheduled, stats.Started, stats.LoadgenDropped)
	}
	if stats.DroppedQueueFull != 1 || stats.DroppedNoFreeSlot != 0 {
		t.Fatalf("drop reasons queue_full=%d no_slot=%d, want 1/0", stats.DroppedQueueFull, stats.DroppedNoFreeSlot)
	}
	if stats.PeakActive != 2 {
		t.Fatalf("peak active = %d, want 2", stats.PeakActive)
	}
	// Arrival 2 was scheduled at +2ms and started at +4ms; arrival 3 at +3ms
	// and +4ms.
	if stats.DispatchLag.Count != 4 || stats.DispatchLag.Max != 3*time.Millisecond {
		t.Fatalf("dispatch lag = %+v, want 4 observations with max 3ms", stats.DispatchLag)
	}
	if stats.ScheduledToTerminal.Count != 4 {
		t.Fatalf("scheduled-to-terminal = %+v, want 4 observations", stats.ScheduledToTerminal)
	}
	collector.SetStats(stats)
	report := collector.Report()
	if report.CapacityRoundValid {
		t.Fatalf("capacity validity = true, want false (1/5 dropped)")
	}
	if report.LoadgenDroppedPercent != 20 {
		t.Fatalf("dropped percent = %v, want 20", report.LoadgenDroppedPercent)
	}
}

// TestSchedulerOpenDoesNotBlockOnQueueAndAbandonsNothing is the no-stall guard
// for the queue path: even with a queue the arrival loop keeps its timestamps.
func TestSchedulerOpenDoesNotBlockOnQueueAndAbandonsNothing(t *testing.T) {
	clock := newFakeClock()
	executor := &instantExecutor{result: Result{Status: 200, Class: ClassCompleted, Events: 1, Chunks: 1}, starts: make(chan int, 16)}
	arrivals := make(chan int, 16)
	scheduler := NewScheduler(SchedulerConfig{
		Mode:        ModeOpen,
		Total:       8,
		Concurrency: 1,
		Rate:        100,
		QueueSize:   2,
		Executor:    executor,
		Collector:   NewCollector(8),
		Clock:       clock,
		OnArrival:   func(index int, at time.Time) { arrivals <- index },
	})
	done := make(chan Stats, 1)
	go func() {
		stats, _ := scheduler.Run(context.Background())
		done <- stats
	}()
	for index := 0; index < 8; index++ {
		awaitIndex(t, arrivals, "arrival")
		clock.Advance(10 * time.Millisecond)
	}
	stats := <-done
	if stats.Scheduled != 8 || stats.Started+stats.LoadgenDropped != 8 {
		t.Fatalf("scheduled=%d started=%d dropped=%d, want every arrival accounted", stats.Scheduled, stats.Started, stats.LoadgenDropped)
	}
	if stats.Abandoned != 0 {
		t.Fatalf("abandoned = %d, want 0 (the queue is drained before Run returns)", stats.Abandoned)
	}
	if stats.ArrivalLag.Max != 0 {
		t.Fatalf("arrival lag = %+v, want 0 (the queue never delayed an arrival)", stats.ArrivalLag)
	}
}

// TestSchedulerFixedStretchesScheduleInsteadOfDropping is the B00 A side: the
// historical blocking scheduler reports zero generator drops while quietly
// delivering arrivals far below the planned rate, and the report now flags that
// as invalid for capacity conclusions.
func TestSchedulerFixedStretchesScheduleInsteadOfDropping(t *testing.T) {
	clock := newFakeClock()
	executor := newBlockingExecutor()
	collector := NewCollector(8)
	arrivals := make(chan int, 8)

	scheduler := NewScheduler(SchedulerConfig{
		Mode:        ModeFixed,
		Total:       4,
		Concurrency: 2,
		Rate:        1000,
		Executor:    executor,
		Collector:   collector,
		Clock:       clock,
		OnArrival:   func(index int, at time.Time) { arrivals <- index },
	})
	done := make(chan Stats, 1)
	go func() {
		stats, _ := scheduler.Run(context.Background())
		done <- stats
	}()

	awaitIndex(t, arrivals, "arrival 0")
	awaitIndex(t, executor.starts, "start 0")
	clock.Advance(time.Millisecond)
	awaitIndex(t, arrivals, "arrival 1")
	awaitIndex(t, executor.starts, "start 1")
	clock.Advance(time.Millisecond)
	// Arrival 2 reaches its planned instant and then blocks on the semaphore.
	awaitIndex(t, arrivals, "arrival 2")
	clock.Advance(2 * time.Millisecond) // the loop is blocked; virtual time runs on
	executor.release <- struct{}{}      // one slot frees at +4ms
	awaitIndex(t, executor.starts, "start 2")
	// Arrival 3 was planned for +3ms but can only be recorded now.
	awaitIndex(t, arrivals, "arrival 3")
	for i := 0; i < 3; i++ {
		executor.release <- struct{}{}
	}
	stats := <-done

	if stats.Scheduled != 4 || stats.Started != 4 {
		t.Fatalf("scheduled=%d started=%d, want 4/4", stats.Scheduled, stats.Started)
	}
	if stats.LoadgenDropped != 0 {
		t.Fatalf("loadgen dropped = %d, want 0: the old scheduler hid saturation instead of counting it", stats.LoadgenDropped)
	}
	// The schedule stretched: arrival 3 was planned for +3ms and arrived at
	// +4ms, and arrival 2 waited 2ms for a slot.
	if stats.ArrivalLag.Max != time.Millisecond {
		t.Fatalf("arrival lag = %+v, want max 1ms (the schedule stretched)", stats.ArrivalLag)
	}
	if stats.DispatchLag.Max != 2*time.Millisecond {
		t.Fatalf("dispatch lag = %+v, want max 2ms (slot wait)", stats.DispatchLag)
	}
	if stats.ArrivalRatePerSecond >= 1000 {
		t.Fatalf("arrival rate = %v/s, want below the planned 1000/s", stats.ArrivalRatePerSecond)
	}

	collector.SetStats(stats)
	report := collector.Report()
	if report.LoadgenDropped != 0 {
		t.Fatalf("report dropped = %d, want 0", report.LoadgenDropped)
	}
	if report.CapacityRoundValid {
		t.Fatalf("capacity validity = true, want false: the stretched schedule must invalidate the round")
	}
	if !strings.Contains(strings.Join(report.CapacityInvalidReasons, "; "), "arrival rate") {
		t.Fatalf("invalid reasons = %v, want the arrival-rate rule", report.CapacityInvalidReasons)
	}
}

// TestSchedulerClosedLoopKeepsExactlyConcurrencyStreams covers the retained
// closed-loop mode: c streams in flight, no generator drops, no external rate.
func TestSchedulerClosedLoopKeepsExactlyConcurrencyStreams(t *testing.T) {
	clock := newFakeClock()
	executor := newBlockingExecutor()
	collector := NewCollector(8)
	arrivals := make(chan int, 8)

	scheduler := NewScheduler(SchedulerConfig{
		Mode:        ModeClosed,
		Total:       6,
		Concurrency: 2,
		Executor:    executor,
		Collector:   collector,
		Clock:       clock,
		OnArrival:   func(index int, at time.Time) { arrivals <- index },
	})
	done := make(chan Stats, 1)
	go func() {
		stats, _ := scheduler.Run(context.Background())
		done <- stats
	}()

	for round := 0; round < 3; round++ {
		for i := 0; i < 2; i++ {
			awaitIndex(t, arrivals, "arrival")
			awaitIndex(t, executor.starts, "start")
		}
		clock.Advance(time.Millisecond)
		executor.release <- struct{}{}
		executor.release <- struct{}{}
	}
	stats := <-done

	if stats.Scheduled != 6 || stats.Started != 6 || stats.LoadgenDropped != 0 {
		t.Fatalf("scheduled=%d started=%d dropped=%d, want 6/6/0", stats.Scheduled, stats.Started, stats.LoadgenDropped)
	}
	if stats.PeakActive != 2 {
		t.Fatalf("peak active = %d, want exactly 2", stats.PeakActive)
	}
	if stats.PlannedStartRatePerSecond != 0 {
		t.Fatalf("planned rate = %v, want 0 for closed loop", stats.PlannedStartRatePerSecond)
	}
	collector.SetStats(stats)
	report := collector.Report()
	if report.Mode != string(ModeClosed) {
		t.Fatalf("report mode = %q, want closed", report.Mode)
	}
	if !report.CapacityRoundValid || report.LoadgenDropped != 0 {
		t.Fatalf("closed-loop report = %+v", report)
	}
	if report.PeakActive != 2 || report.Started != 6 || report.Completed != 6 {
		t.Fatalf("closed-loop accounting = %+v", report)
	}
}

// TestSchedulerTimeoutStartsAtDispatch checks that queue waiting is not charged
// to the per-request timeout: the timeout starts when a slot picks the arrival
// up, and the queue wait is reported as dispatch lag instead.
func TestSchedulerTimeoutStartsAtDispatch(t *testing.T) {
	clock := newFakeClock()
	executor := newBlockingExecutor()
	arrivals := make(chan int, 4)
	scheduler := NewScheduler(SchedulerConfig{
		Mode:        ModeOpen,
		Total:       2,
		Concurrency: 1,
		Rate:        1000,
		QueueSize:   1,
		Timeout:     50 * time.Millisecond,
		Executor:    executor,
		Collector:   NewCollector(2),
		Clock:       clock,
		OnArrival:   func(index int, at time.Time) { arrivals <- index },
	})
	done := make(chan Stats, 1)
	go func() {
		stats, _ := scheduler.Run(context.Background())
		done <- stats
	}()

	awaitIndex(t, arrivals, "arrival 0")
	awaitIndex(t, executor.starts, "start 0")
	clock.Advance(time.Millisecond)
	awaitIndex(t, arrivals, "arrival 1") // no free slot: queued, not dropped
	clock.Advance(5 * time.Millisecond)
	executor.release <- struct{}{} // slot frees at +6ms and dispatches the queued arrival
	awaitIndex(t, executor.starts, "start 1")
	executor.release <- struct{}{}
	stats := <-done

	if stats.Started != 2 || stats.LoadgenDropped != 0 {
		t.Fatalf("started=%d dropped=%d, want 2/0 (queue size 1 fits the second arrival)", stats.Started, stats.LoadgenDropped)
	}
	// Arrival 1 was scheduled at +1ms and dispatched at +6ms.
	if stats.DispatchLag.Count != 2 || stats.DispatchLag.Max != 5*time.Millisecond {
		t.Fatalf("dispatch lag = %+v, want 2 observations with max 5ms", stats.DispatchLag)
	}
	for index, expired := range executor.expiredContexts() {
		if expired {
			t.Fatalf("call %d saw an already-expired context: queue waiting was charged to the request timeout", index)
		}
	}
}
