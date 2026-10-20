package connect

// Exact-admission budget tests, ported from the pinned v2026 engine.
//
// Source: github.com/full-bars/connect@v0.0.0-20261017075543-c8d8cdf536a8
// `transfer_memory_budget_test.go`, the budget-only subset (the queue,
// handoff and receive-window tests in that file belong to later stages and
// are deliberately not carried here). `transfer_memory_budget_hierarchy_test.go`
// is carried whole alongside this file.
//
// Only the imports the carried tests use are declared; the pinned file's
// `context`/`fmt`/`runtime`/`strings`/`protocol` imports belong to the
// deferred tests. No test body was rewritten.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// essential test 1a: budget accounting semantics
func TestTransferMemoryBudgetAccounting(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(64))

	AssertEqual(t, budget.TotalByteCount(), kib(64))
	AssertEqual(t, budget.Available(), kib(64))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	budget.Reserve(kib(16))
	AssertEqual(t, budget.Available(), kib(48))
	AssertEqual(t, budget.UsedByteCount(), kib(16))

	// reserve always succeeds; available floors at zero on overdraft
	budget.Reserve(kib(64))
	AssertEqual(t, budget.Available(), ByteCount(0))
	AssertEqual(t, budget.UsedByteCount(), kib(80))

	budget.Release(kib(80))
	AssertEqual(t, budget.Available(), kib(64))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	reserved, released := budget.Counts()
	AssertEqual(t, reserved, kib(80))
	AssertEqual(t, released, kib(80))
}

// essential test 1a: concurrent reserve/release keeps exact balance (run
// under -race in the suite)
func TestTransferMemoryBudgetConcurrent(t *testing.T) {
	budget := NewTransferMemoryBudget(mib(1))

	var wg sync.WaitGroup
	for g := 0; g < 8; g += 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10000; i += 1 {
				budget.Reserve(1000)
				budget.Available()
				budget.Release(1000)
			}
		}()
	}
	wg.Wait()

	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))
	reserved, released := budget.Counts()
	AssertEqual(t, reserved, ByteCount(8*10000*1000))
	AssertEqual(t, reserved, released)
}

func TestTransferMemoryBudgetTryReserveExactConcurrentCeiling(t *testing.T) {
	const reservationCount = 2
	reservationSize := kib(8)
	budget := NewTransferMemoryBudget(reservationCount * reservationSize)

	start := make(chan struct{})
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if budget.TryReserve(reservationSize) {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	AssertEqual(t, admitted.Load(), int64(reservationCount))
	AssertEqual(t, budget.UsedByteCount(), reservationCount*reservationSize)
	AssertEqual(t, budget.TryReserve(1), false)

	notify := budget.CapacityNotify()
	budget.Release(reservationSize)
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("release did not notify an admission waiter")
	}
	AssertEqual(t, budget.TryReserve(reservationSize), true)
	budget.Release(reservationCount * reservationSize)
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
}

func TestTransferMemoryBudgetCapacityNotificationIsLazy(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(64))
	if budget.notify.Load() != nil {
		t.Fatal("budget eagerly allocated a capacity notification")
	}

	for range 10_000 {
		budget.Reserve(1)
		budget.Release(1)
	}
	if budget.notify.Load() != nil {
		t.Fatal("release allocated a notification without a waiter")
	}

	notify := budget.CapacityNotify()
	if notify == nil {
		t.Fatal("capacity waiter did not receive a notification channel")
	}
	budget.Reserve(1)
	budget.Release(1)
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("subscribed capacity waiter was not notified")
	}
	if budget.notify.Load() != nil {
		t.Fatal("release eagerly allocated the next notification generation")
	}
	if next := budget.CapacityNotify(); next == notify {
		t.Fatal("new waiter reused a closed notification generation")
	}
}

func TestTransferMemoryBudgetCapacityNotificationWakesConcurrentSubscribers(t *testing.T) {
	const waiterCount = 64
	budget := NewTransferMemoryBudget(1)
	budget.Reserve(1)

	start := make(chan struct{})
	notifies := make(chan (<-chan struct{}), waiterCount)
	var wg sync.WaitGroup
	for range waiterCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			notifies <- budget.CapacityNotify()
		}()
	}
	close(start)
	wg.Wait()
	close(notifies)

	var shared <-chan struct{}
	for notify := range notifies {
		if shared == nil {
			shared = notify
		} else if notify != shared {
			t.Fatal("concurrent subscribers observed different notification generations")
		}
	}
	budget.Release(1)
	select {
	case <-shared:
	case <-time.After(time.Second):
		t.Fatal("release did not wake concurrent capacity subscribers")
	}
}

func TestTransferMemoryBudgetCapacityNotificationHasNoReleaseRace(t *testing.T) {
	budget := NewTransferMemoryBudget(1)
	AssertEqual(t, true, budget.TryReserve(1))
	deadline := time.After(10 * time.Second)

	for range 10_000 {
		notify := budget.CapacityNotify()
		releaseDone := make(chan struct{})
		go func() {
			budget.Release(1)
			close(releaseDone)
		}()

		if !budget.TryReserve(1) {
			select {
			case <-notify:
			case <-deadline:
				t.Fatal("release raced past its capacity subscriber")
			}
			if !budget.TryReserve(1) {
				t.Fatal("capacity remained unavailable after release notification")
			}
		}
		<-releaseDone
	}
	budget.Release(1)
}

func TestTransferMemoryBudgetAdmissionReleaseWakesOnlyCapacityFit(t *testing.T) {
	const waiterCount = 64
	budget := NewTransferMemoryBudget(1)
	budget.Reserve(1)

	waiters := make([]*transferMemoryBudgetWaiter, waiterCount)
	notifies := make([]<-chan struct{}, waiterCount)
	for i := range waiterCount {
		waiters[i] = newTransferMemoryBudgetWaiter()
		notifies[i] = waiters[i].subscribe(budget, 1)
	}
	defer func() {
		for _, waiter := range waiters {
			waiter.reset()
		}
	}()

	budget.Release(1)
	wokenCount := 0
	for _, notify := range notifies {
		select {
		case <-notify:
			wokenCount += 1
		default:
		}
	}
	AssertEqual(t, wokenCount, 1)
	AssertEqual(t, budget.capacityWaiterCount.Load(), int64(waiterCount-1))

	if !budget.TryReserve(1) {
		t.Fatal("the single notified admission could not reserve released capacity")
	}
	budget.Release(1)
	wokenCount = 0
	for _, notify := range notifies {
		select {
		case <-notify:
			wokenCount += 1
		default:
		}
	}
	AssertEqual(t, wokenCount, 1)
	AssertEqual(t, budget.capacityWaiterCount.Load(), int64(waiterCount-2))
}

func TestTransferMemoryBudgetAdmissionReleaseSkipsIneligibleHead(t *testing.T) {
	budget := NewTransferMemoryBudget(10)
	budget.Reserve(10)
	largeWaiter := newTransferMemoryBudgetWaiter()
	smallWaiter := newTransferMemoryBudgetWaiter()
	defer largeWaiter.reset()
	defer smallWaiter.reset()

	largeNotify := largeWaiter.subscribe(budget, 8)
	smallNotify := smallWaiter.subscribe(budget, 3)
	budget.Release(3)
	select {
	case <-largeNotify:
		t.Fatal("partial capacity woke an ineligible large admission")
	default:
	}
	select {
	case <-smallNotify:
	default:
		t.Fatal("eligible admission behind a large waiter was not woken")
	}

	budget.Release(5)
	select {
	case <-largeNotify:
	default:
		t.Fatal("large admission was not woken when enough capacity accumulated")
	}
}

func TestTransferMemoryBudgetAdmissionResizeWakesOnlyNewCapacityFit(t *testing.T) {
	budget := NewTransferMemoryBudget(1)
	budget.Reserve(1)
	waiters := make([]*transferMemoryBudgetWaiter, 4)
	notifies := make([]<-chan struct{}, len(waiters))
	for i := range waiters {
		waiters[i] = newTransferMemoryBudgetWaiter()
		notifies[i] = waiters[i].subscribe(budget, 2)
	}
	defer func() {
		for _, waiter := range waiters {
			waiter.reset()
		}
	}()

	budget.SetTotalByteCount(5)
	wokenCount := 0
	for _, notify := range notifies {
		select {
		case <-notify:
			wokenCount += 1
		default:
		}
	}
	AssertEqual(t, wokenCount, 2)
	AssertEqual(t, budget.capacityWaiterCount.Load(), int64(2))
}

func TestTransferMemoryBudgetCanceledAdmissionDoesNotConsumeWake(t *testing.T) {
	budget := NewTransferMemoryBudget(1)
	budget.Reserve(1)
	canceledWaiter := newTransferMemoryBudgetWaiter()
	liveWaiter := newTransferMemoryBudgetWaiter()
	canceledWaiter.subscribe(budget, 1)
	canceledWaiter.reset()
	liveNotify := liveWaiter.subscribe(budget, 1)
	defer liveWaiter.reset()

	budget.Release(1)
	select {
	case <-liveNotify:
	default:
		t.Fatal("canceled admission prevented the live waiter from waking")
	}
}

func TestTransferMemoryBudgetAdmissionSubscribeResetDoesNotAllocate(t *testing.T) {
	budget := NewTransferMemoryBudget(1)
	budget.Reserve(1)
	waiter := newTransferMemoryBudgetWaiter()
	allocCount := testing.AllocsPerRun(1_000, func() {
		waiter.subscribe(budget, 1)
		waiter.reset()
	})
	AssertEqual(t, allocCount, float64(0))
}

func BenchmarkTransferMemoryBudgetReserveReleaseNoWaiter(b *testing.B) {
	budget := NewTransferMemoryBudget(mib(1))
	b.ReportAllocs()
	for range b.N {
		budget.Reserve(1)
		budget.Release(1)
	}
}

func BenchmarkTransferMemoryBudgetConcurrentReserveReleaseNoWaiter(b *testing.B) {
	budget := NewTransferMemoryBudget(mib(1))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			budget.Reserve(1)
			budget.Release(1)
		}
	})
}

func BenchmarkTransferMemoryBudgetAdmissionWakeOneOf64(b *testing.B) {
	const waiterCount = 64
	budget := NewTransferMemoryBudget(1)
	budget.Reserve(1)
	waiters := make([]*transferMemoryBudgetWaiter, waiterCount)
	for i := range waiters {
		waiters[i] = newTransferMemoryBudgetWaiter()
		waiters[i].subscribe(budget, 1)
	}
	defer func() {
		for _, waiter := range waiters {
			waiter.reset()
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		waiter := waiters[i%waiterCount]
		budget.Release(1)
		<-waiter.notify
		if !budget.TryReserve(1) {
			b.Fatal("notified admission could not reserve")
		}
		waiter.subscribe(budget, 1)
	}
}

func TestTransferMemoryBudgetResize(t *testing.T) {
	budget := NewTransferMemoryBudget(kib(64))
	budget.Reserve(kib(48))

	// Shrink is non-evicting and exposes no new headroom until usage drains.
	budget.SetTotalByteCount(kib(32))
	AssertEqual(t, budget.TotalByteCount(), kib(32))
	AssertEqual(t, budget.UsedByteCount(), kib(48))
	AssertEqual(t, budget.Available(), ByteCount(0))

	budget.Release(kib(32))
	AssertEqual(t, budget.UsedByteCount(), kib(16))
	AssertEqual(t, budget.Available(), kib(16))

	// Growth takes effect immediately.
	budget.SetTotalByteCount(kib(128))
	AssertEqual(t, budget.Available(), kib(112))
	budget.Release(kib(16))
	AssertEqual(t, budget.UsedByteCount(), ByteCount(0))

	reserved, released := budget.Counts()
	AssertEqual(t, reserved, released)
}
