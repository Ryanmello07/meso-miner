package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Concurrent ip_version sets must leave the pin switches in step with the
// stack they read: two racing SetStack calls used to interleave the stack
// assignment with the pin switch-overs, so the last assignment could lose to
// the other call's switches and the pins would dial a family the stack
// excluded (or park one it wants). The seam holds one apply open after its
// assignment: a concurrent set that finishes while it is held was applied
// unsynchronized.
func TestFamilyGroupSetStackConcurrentSetsKeepPinsInStep(t *testing.T) {
	platform4 := newFamilyTestPlatform(t, "tcp4", "127.0.0.1:0")
	platform6 := newFamilyTestPlatform(t, "tcp6", "[::1]:0")
	if platform4 == nil || platform6 == nil || !connect.FamilySupported(6) {
		t.Skip("needs IPv4 and IPv6 loopback and a usable IPv6 stack")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategy := connect.NewClientStrategy(ctx, clientStrategySettings)
	defer clientStrategy.Close()

	settings := connect.DefaultPlatformTransportSettings()
	settings.PinnedReconnectMaxTimeout = 200 * time.Millisecond
	settings.EnableH3 = false
	groupSettings := connect.DefaultFamilyPlatformTransportGroupSettings()
	group := newProviderFamilyGroup(
		ctx, clientStrategySettings, clientStrategy, connect.NewRouteManager(ctx, "test-stack"),
		platform4.url(), platform4.url(), platform6.url(),
		&connect.ClientAuth{ByJwt: "testing", InstanceId: connect.NewId(), AppVersion: "testing"},
		settings, groupSettings,
	)
	defer group.Close()

	// hold the first apply open after its assignment; a concurrent set must
	// not be able to finish its own application while it is held
	assigned := make(chan struct{})
	release := make(chan struct{})
	var paused atomic.Bool
	previousPause := connect.FamilySetStackApplyPauseForTest(func() {
		if paused.CompareAndSwap(false, true) {
			close(assigned)
			<-release
		}
	})
	defer connect.FamilySetStackApplyPauseForTest(previousPause)

	firstDone := make(chan connect.FamilyStackMode, 1)
	go func() {
		firstDone <- group.SetStack(connect.FamilyStackDual)
	}()
	<-assigned

	secondDone := make(chan connect.FamilyStackMode, 1)
	go func() {
		secondDone <- group.SetStack(connect.FamilyStackIpv4)
	}()
	select {
	case <-secondDone:
		close(release)
		<-firstDone
		t.Fatal("a concurrent SetStack finished while another apply was held: the applications are not serialized")
	case <-time.After(300 * time.Millisecond):
		// serialized as required: the second set is waiting for the first
	}
	close(release)
	<-firstDone
	<-secondDone

	// the serialized end state is the last set's
	if stack := group.Stack(); stack != connect.FamilyStackIpv4 {
		t.Fatalf("final stack %s, want ipv4 (the last set)", stack)
	}
	ipv4, ipv6 := group.EnabledFamilies()
	if !ipv4 || ipv6 {
		t.Fatalf("final pins ipv4=%t ipv6=%t, want ipv4=true ipv6=false", ipv4, ipv6)
	}

	// and a sequential pair stays consistent too
	group.SetStack(connect.FamilyStackDual)
	if ipv4, ipv6 := group.EnabledFamilies(); !ipv4 || !ipv6 {
		t.Fatalf("dual after ipv4: pins ipv4=%t ipv6=%t", ipv4, ipv6)
	}
}
