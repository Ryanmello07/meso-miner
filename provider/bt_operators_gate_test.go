package main

// Gates that Stage A adds to the v33 provider: the client identity refusal, the
// production 401 rejection path, and seed secrecy. Every fixture is local; no
// test contacts a real operator.

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/hotkeyauth"
)

func TestProviderClientIdentityGateRefusesAFreshDirectory(t *testing.T) {
	home := operatorTestHome(t)
	base := filepath.Join(home, ".urnetwork")
	opts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=https://api.example"})
	err := providerClientIdentityGate(opts)
	var refused *RegistrationRefusedError
	if !errors.As(err, &refused) || refused.Code != "provider_key_requires_explicit_recovery" {
		t.Fatalf("fresh identity gate = %v", err)
	}
	if want := "client registration requires recovery: provider_key_requires_explicit_recovery"; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if _, statErr := os.Stat(filepath.Join(base, ".provider.key")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused fresh identity created .provider.key: %v", statErr)
	}

	// The explicit flag allows creation, and an existing key is retained
	// without it.
	allowed := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=https://api.example", "--allow-client-registration"})
	if err := providerClientIdentityGate(allowed); err != nil {
		t.Fatalf("--allow-client-registration refused: %v", err)
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	retained := make([]byte, 32)
	retained[0] = 1
	if err := os.WriteFile(filepath.Join(base, ".provider.key"), retained, 0600); err != nil {
		t.Fatal(err)
	}
	if err := providerClientIdentityGate(opts); err != nil {
		t.Fatalf("an existing key was refused: %v", err)
	}
}

// A 401 from an operator must reach the rejection path through the production
// error chain: connect's typed HTTP status, networkCredentialRejection, and
// the set-aside file. Nothing here builds a rejection error by hand.
func TestProvideAuthTurnsAReal401IntoAQuarantinedRejection(t *testing.T) {
	home := operatorTestHome(t)
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")
	if err := os.MkdirAll(filepath.Dir(jwtPath), 0700); err != nil {
		t.Fatal(err)
	}
	token := createFakeJWT(time.Now().Add(24 * time.Hour).Unix())
	if err := os.WriteFile(jwtPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/network/auth-client" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte("the network JWT is required"))
	}))
	defer server.Close()

	opts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=" + server.URL, "--operator-child"})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_, _, _, err := provideAuth(ctx, connect.NewClientStrategyWithDefaults(ctx), server.URL, opts, "synthetic-node", "direct")
	if err == nil || !IsNetworkCredentialRejected(err) {
		t.Fatalf("401 from the operator = %v, rejected = %t", err, IsNetworkCredentialRejected(err))
	}

	quarantineOpts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=" + server.URL, "--quarantine-rejected-sign-in", "--operator-child"})
	quarantine, ok, quarantineErr := quarantineRejectedOperatorSignIn(quarantineOpts, err)
	if quarantineErr != nil || !ok {
		t.Fatalf("quarantine: ok=%t err=%v", ok, quarantineErr)
	}
	if _, statErr := os.Stat(jwtPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the rejected jwt was not set aside: %v", statErr)
	}
	if raw, readErr := os.ReadFile(quarantine.Path); readErr != nil || strings.TrimSpace(string(raw)) != token {
		t.Fatalf("set-aside token = %q, %v", raw, readErr)
	}
	latest, found, latestErr := LatestNetworkTokenQuarantine(jwtPath)
	if latestErr != nil || !found || latest.Consecutive != 1 || latest.Path != quarantine.Path {
		t.Fatalf("latest quarantine = %+v, %t, %v", latest, found, latestErr)
	}
}

// A plain node keeps the original 401 text: no re-typing, no quarantine. Only
// an operator child of --all-operators turns it into a rejection.
func TestPlainProvideKeepsItsOriginal401Error(t *testing.T) {
	home := operatorTestHome(t)
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")
	if err := os.MkdirAll(filepath.Dir(jwtPath), 0700); err != nil {
		t.Fatal(err)
	}
	token := createFakeJWT(time.Now().Add(24 * time.Hour).Unix())
	if err := os.WriteFile(jwtPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/network/auth-client" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte("the network JWT is required"))
	}))
	defer server.Close()

	opts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=" + server.URL})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_, _, _, err := provideAuth(ctx, connect.NewClientStrategyWithDefaults(ctx), server.URL, opts, "synthetic-node", "direct")
	if err == nil {
		t.Fatal("a 401 from the API did not fail the auth")
	}
	if IsNetworkCredentialRejected(err) {
		t.Fatalf("plain provide re-typed its 401: %v", err)
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "the network JWT is required") {
		t.Fatalf("plain provide lost its original error text: %v", err)
	}
}

// Without the internal flag a rejection is returned but nothing is moved:
// single-operator behavior is unchanged.
func TestARejectedSignInIsNotSetAsideWithoutTheFlag(t *testing.T) {
	home := operatorTestHome(t)
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")
	if err := os.MkdirAll(filepath.Dir(jwtPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jwtPath, []byte("synthetic-network-token"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=https://api.example"})
	rejected := networkCredentialRejection(&connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: []byte("nope")})
	if quarantine, ok, err := quarantineRejectedOperatorSignIn(opts, rejected); ok || err != nil {
		t.Fatalf("unflagged quarantine = %+v, %t, %v", quarantine, ok, err)
	}
	if _, err := os.Stat(jwtPath); err != nil {
		t.Fatalf("the jwt was moved without the flag: %v", err)
	}
}

// A sentinel seed never reaches a log line or an error string.
func TestHotkeySeedNeverAppearsInLogsOrErrors(t *testing.T) {
	home := operatorTestHome(t)
	sentinel := "sentinel-hotkey-seed-0123456789x"
	seed := []byte(sentinel)
	if len(seed) != 32 {
		t.Fatalf("sentinel seed is %d bytes", len(seed))
	}
	seedPath := filepath.Join(home, "hotkey.seed")
	if err := os.WriteFile(seedPath, seed, 0600); err != nil {
		t.Fatal(err)
	}
	opts := parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--auto-register", "--hotkey_seed_file=" + seedPath, "--operators-url=https://list.example/operators.yml"})
	settings, _, err := newOperatorSupervisorSettings(opts)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if settings.hotkey == nil {
		t.Fatal("hotkey not loaded")
	}
	for _, text := range []string{errString(err), settingsString(settings)} {
		assertNoSeed(t, text, sentinel, seed)
	}

	// The same seed through a failing sign-in: the supervisor's journal and
	// the sign-in error must not carry it either.
	fixture := newTestOperatorFixture(t)
	fixture.settings.autoRegister = true
	fixture.settings.hotkey = settings.hotkey
	fixture.signIn = func(context.Context, hotkeyauth.Settings) (*hotkeyauth.Network, error) {
		return nil, errors.New("synthetic operator refusal")
	}
	fixture.publish(testListedOperator("alpha.example"))
	fixture.run()
	at := fixture.journal.wait(t, 0, "log operator alpha.example: signing in with the hotkey")
	fixture.journal.waitPrefix(t, at, "log operator alpha.example: hotkey sign-in failed: ")
	assertNoSeed(t, fixture.journal.String(), sentinel, seed)
	fixture.stop(syscall.SIGTERM)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func settingsString(settings operatorSupervisorSettings) string {
	return strings.Join([]string{
		settings.baseStateDir,
		settings.executable,
		settings.operatorHotkeyDescription(),
	}, " ")
}

func (self operatorSupervisorSettings) operatorHotkeyDescription() string {
	if self.hotkey == nil {
		return "no hotkey"
	}
	return "hotkey loaded"
}

func assertNoSeed(t *testing.T, text string, seedText string, seed []byte) {
	t.Helper()
	if strings.Contains(text, seedText) || strings.Contains(text, hex.EncodeToString(seed)) {
		t.Fatalf("seed material reached output: %q", text)
	}
}

// Plain provide and auth-provide keep the v33 behavior: a missing provider
// key on an empty state directory is minted and persisted on first run, so
// existing single-network installs upgrade without a flag. Only an operator
// child of provide --all-operators is refused.
func TestPlainProvideStillMintsAProviderKey(t *testing.T) {
	home := operatorTestHome(t)
	base := filepath.Join(home, ".urnetwork")
	opts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=https://api.example"})
	if err := providerClientIdentityGateForOpts(opts); err != nil {
		t.Fatalf("plain provide was refused: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, ".provider.key")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the skipped gate created .provider.key: %v", statErr)
	}

	// The same empty directory as an operator child is the refused case.
	childOpts := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=https://api.example", "--operator-child"})
	var refused *RegistrationRefusedError
	if err := providerClientIdentityGateForOpts(childOpts); !errors.As(err, &refused) {
		t.Fatalf("operator child without the flag = %v", err)
	}

	// Plain provide's first run persists the seed it minted.
	seed := make([]byte, 32)
	seed[0] = 7
	if err := writeProviderClientKeySeed(seed); err != nil {
		t.Fatalf("persist provider key: %v", err)
	}
	got, err := readProviderClientKeySeed()
	if err != nil || string(got) != string(seed) {
		t.Fatalf("provider key read back = %x, %v", got, err)
	}
}
