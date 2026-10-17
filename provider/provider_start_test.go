package main

// Start-up tests on real provider state directories. They run the actual
// main() in a child process, so a flag or gate change that would refuse an
// upgrade fails here rather than on a node. Every fixture and server is
// local; no test contacts an operator or a chain.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const providerStartWindow = 2 * time.Second

// hangServer accepts every request and never answers until the test ends, so
// the provider stays in its retry loop instead of reaching a real network.
func hangServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

// listServer serves one operator list naming one operator.
func listServer(t *testing.T, domain string, apiUrl string) *httptest.Server {
	t.Helper()
	body := fmt.Sprintf("schema: urnetwork-operators-v1\noperators:\n  - domain: %s\n    api_url: %s\n    connect_url: ws://127.0.0.1:1\n", domain, apiUrl)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/yaml")
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// rejectingServer answers every request with 401, like an operator that
// rejected the network sign-in.
func rejectingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "rejected", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	return server
}

type legacyState struct {
	home     string
	stateDir string
	keyPath  string
	key      []byte
}

// legacyStateDir lays out the files a provider of that release keeps: the
// network jwt, the client JWT store and, unless this is the fresh-install
// case, the client identity key.
func legacyStateDir(t *testing.T, version string, withKey bool) legacyState {
	t.Helper()
	home := t.TempDir()
	stateDir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte("legacy-network-jwt-"+version), 0600); err != nil {
		t.Fatal(err)
	}
	clientJwts := `{"direct":{"by_client_jwt":"legacy-client-jwt","client_id":"0011223344556677","network_id":"8899aabbccddeeff","minted_at":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(stateDir, ".client_jwts.json"), []byte(clientJwts), 0600); err != nil {
		t.Fatal(err)
	}
	self := legacyState{home: home, stateDir: stateDir}
	if withKey {
		self.key = bytes.Repeat([]byte{0x2a}, 32)
		self.key[0], self.key[31] = 0x11, 0x99
		self.keyPath = filepath.Join(stateDir, ".provider.key")
		if err := os.WriteFile(self.keyPath, self.key, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return self
}

type providerRun struct {
	output  string
	exited  bool
	exitErr error
}

// runProviderChild starts the test binary as the provider with this home and
// argv, and kills it after the window unless it exits first.
func runProviderChild(t *testing.T, home string, args []string, window time.Duration) providerRun {
	t.Helper()
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(),
		"HOME="+home,
		"USERPROFILE="+home,
		"URNETWORK_STATE_DIR=",
		providerTestExecEnv+"="+strings.Join(args, "\x1f"),
	)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatalf("start provider child: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return providerRun{output: output.String(), exited: true, exitErr: err}
	case <-time.After(window):
		_ = command.Process.Kill()
		<-done
		return providerRun{output: output.String()}
	}
}

// providerKeyFiles is every .provider.key below the state directory, by path,
// with its digest and mode: the whole identity surface a start-up may touch.
type providerKeyFile struct {
	digest string
	mode   fs.FileMode
}

func providerKeyFiles(t *testing.T, stateDir string) map[string]providerKeyFile {
	t.Helper()
	found := map[string]providerKeyFile{}
	err := filepath.WalkDir(stateDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != ".provider.key" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		found[path] = providerKeyFile{digest: hex.EncodeToString(sum[:]), mode: info.Mode().Perm()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func assertProviderKeysUnchanged(t *testing.T, before, after map[string]providerKeyFile) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("provider key files changed: before %v, after %v", before, after)
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Fatalf("%s disappeared", path)
		}
		if got.digest != want.digest {
			t.Fatalf("%s bytes changed", path)
		}
		if got.mode != want.mode {
			t.Fatalf("%s mode changed from %v to %v", path, want.mode, got.mode)
		}
	}
}

func assertNoGateRefusal(t *testing.T, run providerRun) {
	t.Helper()
	if strings.Contains(run.output, "client registration requires recovery") {
		t.Fatalf("the provider refused its existing identity: %s", run.output)
	}
	if run.exited {
		t.Fatalf("the provider did not stay up (exit %v): %s", run.exitErr, run.output)
	}
}

func assertNoOperatorDirectory(t *testing.T, stateDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(stateDir, "operators")); !os.IsNotExist(err) {
		t.Fatalf("a plain provide created an operator directory: %v", err)
	}
}

// A provider upgraded onto a state directory from 30.9 or 32.10, and a fresh
// install with no key, must start with no flags: no identity refusal, no
// operator directory, and the identity file untouched.
func TestProviderStartsOnLegacyStateDirs(t *testing.T) {
	hang := hangServer(t)
	for _, version := range []string{"30.9", "32.10"} {
		t.Run(version, func(t *testing.T) {
			fixture := legacyStateDir(t, version, true)
			before := providerKeyFiles(t, fixture.stateDir)
			run := runProviderChild(t, fixture.home, []string{
				"provide",
				"--api_url=" + hang.URL,
				"--connect_url=ws://127.0.0.1:1",
			}, providerStartWindow)
			assertNoGateRefusal(t, run)
			assertProviderKeysUnchanged(t, before, providerKeyFiles(t, fixture.stateDir))
			assertNoOperatorDirectory(t, fixture.stateDir)
		})
	}
	t.Run("no_provider_key", func(t *testing.T) {
		fixture := legacyStateDir(t, "fresh", false)
		run := runProviderChild(t, fixture.home, []string{
			"provide",
			"--api_url=" + hang.URL,
			"--connect_url=ws://127.0.0.1:1",
		}, providerStartWindow)
		assertNoGateRefusal(t, run)
		assertNoOperatorDirectory(t, fixture.stateDir)
	})
}

// Plain provide, auth-provide and --all-operators must leave an existing
// identity byte for byte, with the mode unchanged and no second key file.
func TestProviderIdentityFileUnchanged(t *testing.T) {
	hang := hangServer(t)
	list := listServer(t, "alpha.example", "https://api.alpha.example")
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{name: "provide", args: []string{"provide", "--api_url=" + hang.URL, "--connect_url=ws://127.0.0.1:1"}},
		{name: "auth_provide", args: []string{"auth-provide", "test-auth-code", "-f", "--api_url=" + hang.URL, "--connect_url=ws://127.0.0.1:1"}},
		{name: "all_operators", args: []string{"provide", "--all-operators", "--operators-url=" + list.URL}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := legacyStateDir(t, "32.10", true)
			before := providerKeyFiles(t, fixture.stateDir)
			run := runProviderChild(t, fixture.home, testCase.args, providerStartWindow)
			if testCase.name != "auth_provide" {
				assertNoGateRefusal(t, run)
			}
			assertProviderKeysUnchanged(t, before, providerKeyFiles(t, fixture.stateDir))
			if testCase.name == "provide" {
				assertNoOperatorDirectory(t, fixture.stateDir)
			}
		})
	}
}

// --all-operators must not even read the top-level identity: an unreadable
// key file cannot stop it, and its mode and bytes stay as they were.
func TestAllOperatorsDoesNotReadTopLevelProviderKey(t *testing.T) {
	list := listServer(t, "alpha.example", "https://api.alpha.example")
	fixture := legacyStateDir(t, "32.10", true)
	before := providerKeyFiles(t, fixture.stateDir)
	if err := os.Chmod(fixture.keyPath, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.keyPath, 0600) })

	run := runProviderChild(t, fixture.home, []string{
		"provide", "--all-operators", "--operators-url=" + list.URL,
	}, providerStartWindow)
	assertNoGateRefusal(t, run)
	if strings.Contains(run.output, ".provider.key") || strings.Contains(run.output, "permission denied") {
		t.Fatalf("--all-operators read the top-level provider key: %s", run.output)
	}
	if info, err := os.Stat(fixture.keyPath); err != nil || info.Mode().Perm() != 0000 {
		t.Fatalf("the unreadable fixture was touched: %v, %v", info, err)
	}
	// Restore readability only to compare the bytes the run left behind.
	if err := os.Chmod(fixture.keyPath, 0600); err != nil {
		t.Fatal(err)
	}
	assertProviderKeysUnchanged(t, before, providerKeyFiles(t, fixture.stateDir))
}

// Offline end to end: a fake operator list and an operator that rejects the
// hotkey sign-in with 401. The supervisor must report it per operator and keep
// running, and no child may start or mint a key.
func TestOfflineAllOperatorsAutoRegisterRejects401(t *testing.T) {
	rejected := rejectingServer(t)
	list := listServer(t, "e2e.example", rejected.URL)
	fixture := legacyStateDir(t, "32.10", true)
	before := providerKeyFiles(t, fixture.stateDir)
	seedPath := filepath.Join(fixture.home, "hotkey.seed")
	if err := os.WriteFile(seedPath, []byte(strings.Repeat("12", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	run := runProviderChild(t, fixture.home, []string{
		"provide", "--all-operators",
		"--operators-url=" + list.URL,
		"--auto-register",
		"--hotkey_seed_file=" + seedPath,
	}, 3*time.Second)
	assertNoGateRefusal(t, run)
	for _, want := range []string{"operator e2e.example", "hotkey sign-in failed", "401"} {
		if !strings.Contains(run.output, want) {
			t.Fatalf("offline auto-register did not report %q: %s", want, run.output)
		}
	}
	assertProviderKeysUnchanged(t, before, providerKeyFiles(t, fixture.stateDir))
	assertNoOperatorDirectory(t, fixture.stateDir)
}
