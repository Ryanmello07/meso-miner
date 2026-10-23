package connect

import (
	"testing"
	"time"
)

// Notify wakes a waiter holding the current channel and allocates nothing
// while nobody holds it.
func TestMonitorNotifyWakesHolderAndSkipsWhenUnheld(t *testing.T) {
	monitor := NewMonitor()
	if allocs := testing.AllocsPerRun(100, monitor.Notify); allocs != 0 {
		t.Fatalf("an unheld Notify allocated %v times", allocs)
	}
	held := monitor.NotifyChannel()
	monitor.Notify()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("Notify did not wake the holder")
	}
	// a channel handed out by NotifyAll is woken by the next Notify too
	next := monitor.NotifyAll()
	monitor.Notify()
	select {
	case <-next:
	case <-time.After(time.Second):
		t.Fatal("Notify did not wake a NotifyAll holder")
	}
	// after a wake, a new holder still gets woken
	again := monitor.NotifyChannel()
	monitor.Notify()
	select {
	case <-again:
	case <-time.After(time.Second):
		t.Fatal("Notify did not wake a later holder")
	}
}
