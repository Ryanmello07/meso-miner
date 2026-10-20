package main

// The operators command tests, ported from the sn miner's
// operators_cmd_test.go (upstream 4ca8316e) and adapted to the v33 grammar:
// no --auto-register, --hotkey_seed_file, --port, --max-memory or
// --allow-client-registration, and no `auth --operator`, so the settings
// tests cover the list options and the state layout only.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/operatorlist"
)

// The digest `provider operators` prints is sha256 over the fetched bytes:
// pin the published shape with a golden so the printed value cannot drift
// silently. (Nothing in this family signs bytes; this digest is the only
// hash on the surface.)
const goldenOperatorListDigest = "sha256:9126a28abaf9dfd32058b82ac986d43d6b3de1c6d1409cd3faebaa6181c068a4"

func TestOperatorsListDigestIsGolden(t *testing.T) {
	raw := testOperatorListYaml(t, testListedOperator("alpha.example"))
	sum := sha256.Sum256([]byte(raw))
	if digest := "sha256:" + hex.EncodeToString(sum[:]); digest != goldenOperatorListDigest {
		t.Fatalf("digest = %s, want %s", digest, goldenOperatorListDigest)
	}
}

func TestMainUsageOperatorCommands(t *testing.T) {
	provide := parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--operators-url=https://list.example/operators.yml", "--operators-refresh=2h", "-vv"})
	for _, flag := range []string{"provide", "--all-operators"} {
		if value, err := provide.Bool(flag); err != nil || !value {
			t.Fatalf("all-operators provide lost %s: %v, %v", flag, value, err)
		}
	}
	for option, want := range map[string]string{
		"--operators-url":     "https://list.example/operators.yml",
		"--operators-refresh": "2h",
	} {
		if value, err := provide.String(option); err != nil || value != want {
			t.Fatalf("all-operators provide %s = %q, %v; want %q", option, value, err, want)
		}
	}
	if verbosity, ok := provide["-v"].(int); !ok || verbosity != 2 {
		t.Fatalf("all-operators provide verbosity = %#v", provide["-v"])
	}
	operators := parseOperatorsArgsForTest(t, []string{"operators", "--operators-url=https://list.example/operators.yml", "-v"})
	if selected, _ := operators.Bool("operators"); !selected {
		t.Fatal("operators command not selected")
	}
	if allOperators, _ := parseOperatorsArgsForTest(t, []string{"provide", "--api_url=https://api.example"}).Bool("--all-operators"); allOperators {
		t.Fatal("plain provide selected all operators")
	}
}

// The v33 grammar has one refuse-by-name case the sn answers with
// operatorArgsError: --api_url inside --all-operators, where every
// operator's urls come from the operator list.
func TestAllOperatorsRefusesAMeaninglessApiUrl(t *testing.T) {
	withApiUrl := parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--api_url=https://api.example"})
	err := allOperatorsArgsError(withApiUrl)
	if err == nil || !strings.Contains(err.Error(), "provide --all-operators excludes --api_url") {
		t.Fatalf("--api_url with --all-operators: %v", err)
	}
	withoutApiUrl := parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--operators-refresh=2h"})
	if err := allOperatorsArgsError(withoutApiUrl); err != nil {
		t.Fatalf("plain --all-operators refused: %v", err)
	}
}

func TestOperatorSupervisorSettingsReadTheListOptions(t *testing.T) {
	home := operatorTestHome(t)
	base := filepath.Join(home, ".urnetwork")
	opts := parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--operators-url=https://list.example/operators.yml", "--operators-refresh=2h", "-v"})
	settings, listSettings, err := newOperatorSupervisorSettings(opts)
	if err != nil {
		t.Fatal(err)
	}
	if settings.baseStateDir != base || settings.verbosity != 1 || settings.executable == "" {
		t.Fatalf("settings = %+v", settings)
	}
	if listSettings.Url != "https://list.example/operators.yml" || listSettings.Interval != 2*time.Hour || listSettings.CachePath != filepath.Join(base, "operators.yml") {
		t.Fatalf("list settings = %+v", listSettings)
	}
	if _, listSettings, err = newOperatorSupervisorSettings(parseOperatorsArgsForTest(t, []string{"provide", "--all-operators"})); err != nil || listSettings.Interval != time.Hour {
		t.Fatalf("default refresh = %v, %v", listSettings.Interval, err)
	}
	for _, refresh := range []string{"30s", "soon"} {
		if _, _, err := newOperatorSupervisorSettings(parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--operators-refresh=" + refresh})); err == nil {
			t.Errorf("--operators-refresh=%s accepted", refresh)
		}
	}
}

func TestPrintOperatorsShowsCredentialStateSourceAndDigest(t *testing.T) {
	home := operatorTestHome(t)
	base := filepath.Join(home, ".urnetwork")
	operator := newTestOperatorListServer(t)
	digest := operator.setList(t, testListedOperator("alpha.example"), testListedOperator("beta.example"))
	writeOperatorJwt(t, base, "alpha.example", "synthetic-jwt")
	opts := parseOperatorsArgsForTest(t, []string{"operators", "--operators-url=" + operator.listUrl()})
	var out bytes.Buffer
	if err := printOperators(t.Context(), opts, &out); err != nil {
		t.Fatal(err)
	}
	table := "" +
		"alpha.example  jwt present    https://api.alpha.example  wss://connect.alpha.example\n" +
		"beta.example   awaiting auth  https://api.beta.example   wss://connect.beta.example\n" +
		"authenticate an operator awaiting auth with: provider auth --operator=<domain>\n"
	if want := "source: fetched " + operator.listUrl() + "\ndigest: " + digest + "\n" + table; out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestPrintOperatorsFallsBackToTheCache(t *testing.T) {
	home := operatorTestHome(t)
	base := filepath.Join(home, ".urnetwork")
	operator := newTestOperatorListServer(t)
	digest := operator.setList(t, testListedOperator("alpha.example"), testListedOperator("beta.example"))
	writeOperatorJwt(t, base, "alpha.example", "synthetic-jwt")
	opts := parseOperatorsArgsForTest(t, []string{"operators", "--operators-url=" + operator.listUrl()})
	if err := printOperators(t.Context(), opts, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	// unreachable, the list comes from the cache with the same digest
	operator.setListOnline(false)
	var out bytes.Buffer
	if err := printOperators(t.Context(), opts, &out); err != nil {
		t.Fatal(err)
	}
	source, rest, _ := strings.Cut(out.String(), "\n")
	fetchedAt, ok := strings.CutPrefix(source, "source: cached "+operatorListCachePath(base)+", fetched ")
	if _, err := time.Parse(time.RFC3339, fetchedAt); !ok || err != nil {
		t.Fatalf("cached source line = %q", source)
	}
	if !strings.Contains(rest, "digest: "+digest) {
		t.Fatalf("cached output:\n%s", out.String())
	}
	if _, err := operatorlist.Parse(mustReadOperatorFile(t, operatorListCachePath(base))); err != nil {
		t.Fatalf("operator list not cached: %v", err)
	}
}

// The operator options Stage A adds: the memory share, the status port, the
// client registration permission and the hotkey seed.
func TestOperatorSupervisorSettingsReadTheOperatorOptions(t *testing.T) {
	home := operatorTestHome(t)
	seedPath := filepath.Join(home, "hotkey.seed")
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	if err := os.WriteFile(seedPath, seed, 0600); err != nil {
		t.Fatal(err)
	}
	opts := parseOperatorsArgsForTest(t, []string{
		"provide", "--all-operators",
		"--operators-url=https://list.example/operators.yml",
		"--max-memory=3gib", "--port=9090", "--allow-client-registration",
		"--hotkey_seed_file=" + seedPath,
	})
	settings, _, err := newOperatorSupervisorSettings(opts)
	if err != nil {
		t.Fatal(err)
	}
	if settings.maxMemory != 3*1024*1024*1024 || settings.port != 9090 || !settings.allowClientRegistration || settings.autoRegister || settings.hotkey == nil {
		t.Fatalf("settings = %+v", settings)
	}

	// --auto-register needs the seed, and a bad seed or memory value is
	// refused before any child runs.
	if _, _, err := newOperatorSupervisorSettings(parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--auto-register", "--hotkey_seed_file=" + seedPath})); err != nil {
		t.Fatalf("--auto-register with a seed refused: %v", err)
	}
	if _, _, err := newOperatorSupervisorSettings(parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--max-memory=plenty"})); err == nil {
		t.Fatal("bad --max-memory accepted")
	}
	if _, _, err := newOperatorSupervisorSettings(parseOperatorsArgsForTest(t, []string{"provide", "--all-operators", "--hotkey_seed_file=" + filepath.Join(home, "missing.seed")})); err == nil || !strings.Contains(err.Error(), "missing.seed") {
		t.Fatalf("missing seed file: %v", err)
	}
}

func TestOperatorStatusServerIsOffByDefault(t *testing.T) {
	server, err := newOperatorSupervisor(operatorSupervisorSettings{}, defaultOperatorSupervisorHooks(), io.Discard).serveStatus(0)
	if err != nil || server != nil {
		t.Fatalf("port 0 status server = %v, %v", server, err)
	}
}

// auth --operator is its own grammar line and refuses a domain the list does
// not carry before any sign-in is attempted.
func TestOperatorAuthRefusesADomainNotInTheList(t *testing.T) {
	home := operatorTestHome(t)
	operator := newTestOperatorListServer(t)
	operator.setList(t, testListedOperator("alpha.example"))
	opts := parseOperatorsArgsForTest(t, []string{"auth", "--operator=beta.example", "--operators-url=" + operator.listUrl()})
	if selected, _ := opts.Bool("auth"); !selected {
		t.Fatal("auth not selected")
	}
	if domain, _ := opts.String("--operator"); domain != "beta.example" {
		t.Fatalf("--operator = %q", domain)
	}
	err := operatorAuth(t.Context(), opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is not in the operator list") {
		t.Fatalf("unknown operator = %v", err)
	}
	if _, statErr := os.Stat(operatorJwtPath(filepath.Join(home, ".urnetwork"), "beta.example")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("an unknown operator wrote a jwt: %v", statErr)
	}
}
