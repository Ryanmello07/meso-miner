package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

var (
	registrationTestNetworkId = connect.NewId()
	registrationTestUserId    = connect.NewId()
)

func registrationTestNetworkJWT(expiry time.Time) string {
	return createFakeJWTWithClaims(map[string]interface{}{
		"exp":        float64(expiry.Unix()),
		"network_id": registrationTestNetworkId.String(),
		"user_id":    registrationTestUserId.String(),
	})
}

// fakeRegistrationServer keeps the server's rules: one binding per scope that
// survives revocation, an exact replay answers the same identity, and any
// other request for a bound scope is refused.
type fakeRegistrationServer struct {
	t *testing.T

	mu       sync.Mutex
	bindings map[string]*fakeRegistrationBinding
	requests [][]byte
	allocs   int
	// loseReplies drops this many committed replies (the connection breaks)
	loseReplies int
	status      int
}

type fakeRegistrationBinding struct {
	request  registerNetworkClientArgs
	clientId connect.Id
	deviceId connect.Id
	revoked  bool
}

func newFakeRegistrationServer(t *testing.T) (*fakeRegistrationServer, *httptest.Server) {
	fake := &fakeRegistrationServer{t: t, bindings: map[string]*fakeRegistrationBinding{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	return fake, server
}

func (self *fakeRegistrationServer) serve(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/network/register-client-v1" {
		http.NotFound(writer, request)
		return
	}
	body, _ := io.ReadAll(request.Body)
	self.mu.Lock()
	defer self.mu.Unlock()
	self.requests = append(self.requests, body)
	if self.status != 0 {
		writer.WriteHeader(self.status)
		return
	}
	var args registerNetworkClientArgs
	if err := json.Unmarshal(body, &args); err != nil || args.Schema != networkClientRegistrationSchema {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	digest := sha256.Sum256(body)
	result := registerNetworkClientResult{Schema: networkClientRegistrationSchema, RegistrationId: args.RegistrationId, RequestSha256: hex.EncodeToString(digest[:])}
	binding := self.bindings[args.ScopeSha256]
	switch {
	case binding == nil:
		binding = &fakeRegistrationBinding{request: args, clientId: connect.NewId(), deviceId: connect.NewId()}
		self.bindings[args.ScopeSha256] = binding
		self.allocs++
	case binding.request != args:
		result.Error = &registerNetworkClientError{Code: "registration_conflict"}
	case binding.revoked:
		result.Error = &registerNetworkClientError{Code: "client_revoked"}
	}
	if result.Error == nil {
		clientId, deviceId := binding.clientId, binding.deviceId
		result.ClientId, result.DeviceId = &clientId, &deviceId
		result.ByClientJwt = createFakeJWTWithClaims(map[string]interface{}{
			"exp":        float64(time.Now().Add(24 * time.Hour).Unix()),
			"network_id": registrationTestNetworkId.String(),
			"user_id":    registrationTestUserId.String(),
			"client_id":  clientId.String(),
			"device_id":  deviceId.String(),
		})
	}
	if 0 < self.loseReplies {
		self.loseReplies--
		hijacker := writer.(http.Hijacker)
		conn, _, err := hijacker.Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	raw, _ := json.Marshal(result)
	writer.Write(raw)
}

func registrationTestState(t *testing.T) string {
	t.Helper()
	home := operatorTestHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0700); err != nil {
		t.Fatal(err)
	}
	return home
}

func registerForTest(t *testing.T, server *httptest.Server, networkJwt string, identityKey string) (string, connect.Id, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	return registerProviderClient(ctx, connect.NewClientStrategyWithDefaults(ctx), server.URL, networkJwt, identityKey, "node [v1]")
}

func TestProviderRegistrationAllocatesOnceAndReplays(t *testing.T) {
	home := registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))

	jwt, clientId, err := registerForTest(t, server, networkJwt, "direct")
	if err != nil || jwt == "" {
		t.Fatalf("register: %v", err)
	}
	// a restart replays the retained request and gets the same identity
	_, again, err := registerForTest(t, server, networkJwt, "direct")
	if err != nil || again != clientId {
		t.Fatalf("replay = %s, %v; want %s", again, err, clientId)
	}
	if fake.allocs != 1 || len(fake.requests) != 2 || string(fake.requests[0]) != string(fake.requests[1]) {
		t.Fatalf("allocs %d, requests %d, replay identical %t", fake.allocs, len(fake.requests), len(fake.requests) == 2 && string(fake.requests[0]) == string(fake.requests[1]))
	}
	// the request names the provider key that the state dir now holds
	seed, err := os.ReadFile(filepath.Join(home, ".urnetwork", ".provider.key"))
	if err != nil || len(seed) != 32 {
		t.Fatalf("provider key %d bytes, %v", len(seed), err)
	}
	records, err := readProviderRegistrations(filepath.Join(home, ".urnetwork", providerRegistrationStoreName))
	if err != nil || len(records) != 1 {
		t.Fatalf("records %d, %v", len(records), err)
	}
	for _, record := range records {
		if record.ClientId != clientId.String() || record.Scope.ClientSlot != "direct" || record.Scope.ClientRole != providerRegistrationRole ||
			record.Scope.Endpoint != server.URL+"/network/register-client-v1" || record.Request.DeviceDescription != "node [v1]" {
			t.Fatalf("record = %+v", record)
		}
	}
}

// A committed allocation whose reply is lost is recovered by replaying the
// same request; no second client is allocated.
func TestProviderRegistrationLostReplyReplaysTheSameRequest(t *testing.T) {
	registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	// the client strategy retries a broken connection over its other
	// transports itself (with the same body), so every attempt loses its reply
	fake.loseReplies = 1 << 20
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))

	_, _, err := registerForTest(t, server, networkJwt, "direct")
	var unavailable *ProviderRegistrationUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("lost reply = %v, want unavailable", err)
	}
	fake.mu.Lock()
	fake.loseReplies = 0
	fake.mu.Unlock()
	_, clientId, err := registerForTest(t, server, networkJwt, "direct")
	if err != nil {
		t.Fatal(err)
	}
	if fake.allocs != 1 || fake.bindings[fake.firstScope()].clientId != clientId {
		t.Fatalf("allocs %d after a lost reply", fake.allocs)
	}
}

func (self *fakeRegistrationServer) firstScope() string {
	for scope := range self.bindings {
		return scope
	}
	return ""
}

// Each proxy slot is its own scope: its own request and client.
func TestProviderRegistrationSlotsAreDistinct(t *testing.T) {
	registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))

	var wg sync.WaitGroup
	ids := make([]connect.Id, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ids[i], errs[i] = registerForTest(t, server, networkJwt, "socks5://proxy:1080|user"+string(rune('a'+i)))
		}()
	}
	wg.Wait()
	seen := map[connect.Id]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		seen[ids[i]] = true
	}
	if len(seen) != 8 || fake.allocs != 8 {
		t.Fatalf("%d distinct clients, %d allocations", len(seen), fake.allocs)
	}
	// concurrent first registrations shared one created key
	keys := map[string]bool{}
	for _, binding := range fake.bindings {
		keys[binding.request.ScopeSha256] = true
	}
	records, _ := readProviderRegistrations(mustRegistrationStorePath(t))
	clientKeys := map[string]bool{}
	for _, record := range records {
		clientKeys[record.Scope.ClientKey] = true
		if !strings.HasPrefix(record.Scope.ClientSlot, "proxy-sha256:") {
			t.Fatalf("slot %q", record.Scope.ClientSlot)
		}
	}
	if len(records) != 8 || len(clientKeys) != 1 {
		t.Fatalf("%d records over %d client keys", len(records), len(clientKeys))
	}
}

func mustRegistrationStorePath(t *testing.T) string {
	t.Helper()
	path, err := providerRegistrationStorePath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// A revoked identity is refused on replay and never recreated.
func TestProviderRegistrationRevokedSlotStaysRefused(t *testing.T) {
	registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))
	if _, _, err := registerForTest(t, server, networkJwt, "direct"); err != nil {
		t.Fatal(err)
	}
	fake.bindings[fake.firstScope()].revoked = true

	for range 2 {
		_, _, err := registerForTest(t, server, networkJwt, "direct")
		var refused *ProviderRegistrationRefusedError
		if !errors.As(err, &refused) || refused.Code != "client_revoked" {
			t.Fatalf("revoked slot = %v", err)
		}
	}
	if fake.allocs != 1 {
		t.Fatalf("a revoked slot was reallocated: %d", fake.allocs)
	}
}

// A server without the route is unsupported, with no legacy fallback; a 401
// keeps the server's status error for the caller.
func TestProviderRegistrationUnsupportedAndRejected(t *testing.T) {
	registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	networkJwt := registrationTestNetworkJWT(time.Now().Add(time.Hour))

	fake.status = http.StatusNotFound
	_, _, err := registerForTest(t, server, networkJwt, "direct")
	var unsupported *ProviderRegistrationUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("404 = %v", err)
	}
	fake.status = http.StatusUnauthorized
	_, _, err = registerForTest(t, server, networkJwt, "direct")
	var status *connect.HttpStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized {
		t.Fatalf("401 = %v", err)
	}
	for _, request := range fake.requests {
		var args registerNetworkClientArgs
		if json.Unmarshal(request, &args) != nil || args.Schema != networkClientRegistrationSchema {
			t.Fatalf("non-registration request %s", request)
		}
	}
}

// A sign-in for another principal never replays this network's operation,
// and a token without a network principal is refused before any request.
func TestProviderRegistrationPrincipalChecks(t *testing.T) {
	registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	if _, _, err := registerForTest(t, server, registrationTestNetworkJWT(time.Now().Add(time.Hour)), "direct"); err != nil {
		t.Fatal(err)
	}
	otherUser := createFakeJWTWithClaims(map[string]interface{}{
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"network_id": registrationTestNetworkId.String(),
		"user_id":    connect.NewId().String(),
	})
	if _, _, err := registerForTest(t, server, otherUser, "direct"); err == nil || !strings.Contains(err.Error(), "principal") {
		t.Fatalf("other principal = %v", err)
	}
	if _, _, err := registerForTest(t, server, createFakeJWT(time.Now().Add(time.Hour).Unix()), "direct"); err == nil {
		t.Fatal("a token without a network principal registered")
	}
	if len(fake.requests) != 1 {
		t.Fatalf("%d requests; checks must stop before sending", len(fake.requests))
	}
}

func TestProviderRegistrationRefusesAWrongSizedKey(t *testing.T) {
	home := registrationTestState(t)
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", ".provider.key"), []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	_, server := newFakeRegistrationServer(t)
	if _, _, err := registerForTest(t, server, registrationTestNetworkJWT(time.Now().Add(time.Hour)), "direct"); err == nil {
		t.Fatal("registered with a damaged provider key")
	}
}

// provideAuth mints through the registration route and stores the client JWT.
func TestProvideAuthRegistersThroughTheVersionedRoute(t *testing.T) {
	home := registrationTestState(t)
	fake, server := newFakeRegistrationServer(t)
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", "jwt"), []byte(registrationTestNetworkJWT(time.Now().Add(time.Hour))), 0600); err != nil {
		t.Fatal(err)
	}
	opts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=" + server.URL})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	jwt, clientId, reused, err := provideAuth(ctx, connect.NewClientStrategyWithDefaults(ctx), server.URL, opts, "synthetic-node", "direct-registration-test")
	if err != nil || reused || jwt == "" || fake.allocs != 1 || fake.bindings[fake.firstScope()].clientId != clientId {
		t.Fatalf("provideAuth = %s reused=%t err=%v allocs=%d", clientId, reused, err, fake.allocs)
	}
	if entry, ok := globalClientJWTStore.Get("direct-registration-test"); !ok || entry.ClientID != clientId.String() {
		t.Fatalf("client JWT not stored: %+v", entry)
	}
}
