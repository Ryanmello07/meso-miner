package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// An unreadable store must refuse, not start new operations: the requests it
// held may already have committed, and a new one would be refused for the
// same scope. The file is left for the operator to restore.
func TestProviderRegistrationRefusesAnUnreadableStore(t *testing.T) {
	home := registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))
	for _, name := range []string{providerRegistrationStoreName, providerRegistrationGenerationsName} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(home, ".urnetwork", name)
			if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			requestsBefore := len(fake.requests)
			if _, _, err := registerForTest(t, server, networkJwt, "direct"); err == nil || !strings.Contains(err.Error(), "unreadable") {
				t.Fatalf("register with an unreadable %s = %v", name, err)
			}
			if len(fake.requests) != requestsBefore {
				t.Fatalf("sent a registration with an unreadable %s", name)
			}
			if raw, _ := os.ReadFile(path); string(raw) != "{not json" {
				t.Fatalf("%s was rewritten", name)
			}
		})
	}
	// reissue reads both files too
	path := filepath.Join(home, ".urnetwork", providerRegistrationGenerationsName)
	if err := os.WriteFile(path, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(withGlobalStore(t, filepath.Join(home, ".urnetwork", ".client_jwts.json")))
	if _, err := reissueProviderSlot("direct", true); err == nil {
		t.Fatal("forced reissue over an unreadable generations file")
	}
}

// Proxies of one node start together. Concurrent first registrations of the
// same slot share one operation and one client; different slots get their own.
func TestProviderRegistrationConcurrentStartsShareOneOperation(t *testing.T) {
	registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))
	keys := []string{"direct", "10.0.0.1:1080", "10.0.0.2:1080"}
	const perKey = 4
	ids := make([][]connect.Id, len(keys))
	errs := make(chan error, len(keys)*perKey)
	var wait sync.WaitGroup
	var mu sync.Mutex
	for index, key := range keys {
		for range perKey {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, clientId, err := registerForTest(t, server, networkJwt, key)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				ids[index] = append(ids[index], clientId)
				mu.Unlock()
			}()
		}
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[connect.Id]string{}
	for index, key := range keys {
		for _, clientId := range ids[index] {
			if clientId != ids[index][0] {
				t.Fatalf("%s got clients %s and %s", key, ids[index][0], clientId)
			}
		}
		if other, ok := seen[ids[index][0]]; ok {
			t.Fatalf("%s and %s share client %s", key, other, ids[index][0])
		}
		seen[ids[index][0]] = key
	}
	if fake.allocs != len(keys) {
		t.Fatalf("%d allocations, want %d", fake.allocs, len(keys))
	}
}

// Each reissue moves the slot to a new generation, kept across restarts, and
// a later generation never falls back to an earlier, revoked one.
func TestProviderRegistrationGenerationsAdvanceAndPersist(t *testing.T) {
	home := registrationTestState(t)
	t.Cleanup(withGlobalStore(t, filepath.Join(home, ".urnetwork", ".client_jwts.json")))
	fake, server := newFakeRegistrationServer(t)
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))
	clientIds := map[connect.Id]bool{}
	for generation := uint64(0); generation < 3; generation++ {
		if 0 < generation {
			got, err := reissueProviderSlot("direct", false)
			if err != nil || got != generation {
				t.Fatalf("reissue = %d, %v; want %d", got, err, generation)
			}
		}
		_, clientId, err := registerForTest(t, server, networkJwt, "direct")
		if err != nil {
			t.Fatal(err)
		}
		if clientIds[clientId] {
			t.Fatalf("generation %d reused client %s", generation, clientId)
		}
		clientIds[clientId] = true
		// revoke it, as an operator removing the client would
		for _, binding := range fake.bindings {
			if binding.clientId == clientId {
				binding.revoked = true
			}
		}
		if _, _, err := registerForTest(t, server, networkJwt, "direct"); err == nil {
			t.Fatalf("generation %d: a revoked slot was not refused", generation)
		}
	}
	generations, err := readProviderRegistrationGenerations()
	if err != nil || generations[registrationGenerationKey("direct")] != 2 {
		t.Fatalf("saved generations = %v, %v", generations, err)
	}
	if providerRegistrationSlot("direct", 1) == providerRegistrationSlot("direct", 2) ||
		providerRegistrationSlot("direct", 0) == providerRegistrationSlot("direct", 1) {
		t.Fatal("generations share a slot")
	}
}
