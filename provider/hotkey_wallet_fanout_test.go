package main

// Fixtures for the wallet hotkey fan-out:
//
//   - targets come from the operator list snapshot, never from scanning the
//     operators directory;
//   - a pre-operator install (no operators directory) keeps its single network
//     target, while operator state with zero authenticated operators never
//     falls back to the legacy network;
//   - one operator's failure is reported per operator and never stops the
//     others; partial failure returns an error keyed off failed operators, and
//     status with awaiting operators still exits zero;
//   - every request carries only the credential of the operator it is sent to.
//
// Every server here is a local httptest fixture. No test reaches a real
// operator or the published list.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/operatorlist"
	"github.com/urfoundation/sn/protocol"
)

// A synthetic network JWT with a marker that keeps every operator's token
// distinct. The marker never contains a seed or other secret.
func fanoutTestJwtAt(t *testing.T, marker string, expiresAt time.Time) string {
	t.Helper()
	claims := gojwt.MapClaims{
		"network_id":   fakeTestNetworkId.String(),
		"user_id":      fakeTestUserId.String(),
		"network_name": "synthetic-fanout-" + marker,
		"roles":        []string{"provider"},
		"exp":          expiresAt.Unix(),
	}
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func fanoutTestJwt(t *testing.T, marker string) string {
	t.Helper()
	return fanoutTestJwtAt(t, marker, time.Now().Add(24*time.Hour))
}

// An expired synthetic jwt: an operator rejects it with 401, and the command
// must report that without failing the whole fan-out.
func fanoutExpiredTestJwt(t *testing.T, marker string) string {
	t.Helper()
	return fanoutTestJwtAt(t, marker, time.Now().Add(-time.Hour))
}

// A fake operator that accepts exactly one synthetic token.
func newFanoutOperator(t *testing.T, token string) *fakeOperatorServer {
	t.Helper()
	op := newFakeOperator(t)
	op.mu.Lock()
	op.byJwt = token
	op.mu.Unlock()
	return op
}

// The published-list entry for a fake operator, with a valid loopback pair.
func fanoutListOperator(domain string, op *fakeOperatorServer) operatorlist.Operator {
	return operatorlist.Operator{
		Domain:     domain,
		ApiUrl:     op.server.URL,
		ConnectUrl: strings.Replace(op.server.URL, "http://", "ws://", 1),
	}
}

// Owner-only hotkey and coldkey seed files and the coldkey address.
func fanoutSeeds(t *testing.T, base string) (hotkeySeed string, coldkeySeed string, coldkeySs58 string) {
	t.Helper()
	hotkeySeed = filepath.Join(base, "hotkey.seed")
	if err := os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	coldkeySeed = filepath.Join(base, "coldkey.seed")
	if err := os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := crv4.LoadSeedFile(coldkeySeed)
	if err != nil {
		t.Fatal(err)
	}
	keypair, err := crv4.KeypairFromSeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	return hotkeySeed, coldkeySeed, keypair.Address()
}

func fanoutAuthSeen(op *fakeOperatorServer) []string {
	op.mu.Lock()
	defer op.mu.Unlock()
	return slices.Clone(op.authSeen)
}

// Targets come from the list snapshot: only listed operators with a jwt are
// targets, each with its own api url and jwt, and a listed operator without a
// jwt is awaiting. An unlisted operator directory with a jwt is never scanned.
func TestHotkeyWalletFanout_TargetsComeFromTheOperatorList(t *testing.T) {
	base := hotkeyWalletTestStateDir(t)
	list := newTestOperatorListServer(t)

	tokenAlpha := fanoutTestJwt(t, "alpha")
	tokenBeta := fanoutTestJwt(t, "beta")
	tokenStray := fanoutTestJwt(t, "stray")
	alpha := newFanoutOperator(t, tokenAlpha)
	beta := newFanoutOperator(t, tokenBeta)
	gamma := newFanoutOperator(t, fanoutTestJwt(t, "gamma"))

	list.setList(t,
		fanoutListOperator("alpha.example", alpha),
		fanoutListOperator("beta.example", beta),
		fanoutListOperator("gamma.example", gamma),
	)
	writeOperatorJwt(t, base, "alpha.example", tokenAlpha)
	writeOperatorJwt(t, base, "beta.example", tokenBeta)
	// A delisted operator with a jwt exists in the state directory: it must not
	// become a target.
	writeOperatorJwt(t, base, "stray.example", tokenStray)
	if _, err := readOperatorJwt(operatorJwtPath(base, "stray.example")); err != nil {
		t.Fatalf("the stray operator jwt fixture is missing: %v", err)
	}

	opts := docopt.Opts{
		"hotkey":          true,
		"challenge":       true,
		"--operators-url": list.listUrl(),
	}
	targets, awaiting, err := hotkeyWalletTargetsFromOpts(context.Background(), opts, base)
	if err != nil {
		t.Fatalf("resolve targets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 authenticated targets, got %d", len(targets))
	}
	for index, expected := range []struct {
		domain string
		apiUrl string
		byJwt  string
	}{
		{domain: "alpha.example", apiUrl: alpha.server.URL, byJwt: tokenAlpha},
		{domain: "beta.example", apiUrl: beta.server.URL, byJwt: tokenBeta},
	} {
		target := targets[index]
		if target.domain != expected.domain || target.apiUrl != expected.apiUrl || target.byJwt != expected.byJwt {
			t.Fatalf("target %d is not the listed authenticated operator %s", index, expected.domain)
		}
	}
	if !slices.Equal(awaiting, []string{"gamma.example"}) {
		t.Fatalf("expected gamma.example awaiting auth, got %v", awaiting)
	}
	for _, target := range targets {
		if target.domain == "stray.example" {
			t.Fatal("the operators directory was scanned: a delisted operator became a target")
		}
	}
}

// A pre-operator install has no operators directory and one jwt at the top of
// the state directory: it keeps exactly today's single target.
func TestHotkeyWalletFanout_LegacySingleNetworkFallback(t *testing.T) {
	base := hotkeyWalletTestStateDir(t)
	token := fanoutTestJwt(t, "legacy")
	if err := os.WriteFile(filepath.Join(base, "jwt"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("default_api_url", func(t *testing.T) {
		targets, awaiting, err := hotkeyWalletTargetsFromOpts(context.Background(), docopt.Opts{
			"hotkey":    true,
			"challenge": true,
		}, base)
		if err != nil {
			t.Fatalf("resolve the legacy target: %v", err)
		}
		if len(targets) != 1 || len(awaiting) != 0 {
			t.Fatalf("expected exactly one legacy target and no awaiting operator, got %d targets and %v", len(targets), awaiting)
		}
		if targets[0].apiUrl != DefaultApiUrl {
			t.Fatalf("expected the default api url %s, got %s", DefaultApiUrl, targets[0].apiUrl)
		}
		if targets[0].byJwt != token {
			t.Fatal("the legacy target did not carry the state directory's jwt")
		}
	})

	t.Run("explicit_api_url", func(t *testing.T) {
		targets, awaiting, err := hotkeyWalletTargetsFromOpts(context.Background(), docopt.Opts{
			"hotkey":    true,
			"challenge": true,
			"--api_url": "https://legacy.example",
		}, base)
		if err != nil {
			t.Fatalf("resolve the legacy target: %v", err)
		}
		if len(targets) != 1 || len(awaiting) != 0 {
			t.Fatalf("expected exactly one legacy target and no awaiting operator, got %d targets and %v", len(targets), awaiting)
		}
		if targets[0].apiUrl != "https://legacy.example" {
			t.Fatalf("expected the explicit api url, got %s", targets[0].apiUrl)
		}
	})
}

// Operator state with zero authenticated operators must not fall back to the
// legacy network: challenge and set refuse, and status prints every awaiting
// operator and still exits zero.
func TestHotkeyWalletFanout_NoFallbackWhenOperatorStateHasNoAuthenticatedOperator(t *testing.T) {
	requireSeedFileCustody(t)
	base := hotkeyWalletTestStateDir(t)
	list := newTestOperatorListServer(t)

	domains := []string{"alpha.example", "beta.example", "gamma.example"}
	list.setList(t, testListedOperator(domains[0]), testListedOperator(domains[1]), testListedOperator(domains[2]))
	// Operator state exists: the operators directory is there, with one
	// operator's directory, but no operator has a jwt.
	if err := os.MkdirAll(filepath.Dir(operatorJwtPath(base, domains[0])), 0700); err != nil {
		t.Fatal(err)
	}
	legacyToken := fanoutTestJwt(t, "legacy")
	if err := os.WriteFile(filepath.Join(base, "jwt"), []byte(legacyToken), 0600); err != nil {
		t.Fatal(err)
	}

	opts := docopt.Opts{
		"hotkey":          true,
		"--operators-url": list.listUrl(),
	}
	targets, awaiting, err := hotkeyWalletTargetsFromOpts(context.Background(), opts, base)
	if err != nil {
		t.Fatalf("resolve targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("operator state with zero authenticated operators produced %d targets", len(targets))
	}
	if !slices.Equal(awaiting, domains) {
		t.Fatalf("expected every listed operator awaiting auth, got %v", awaiting)
	}

	hotkeySeed, coldkeySeed, coldkeySs58 := fanoutSeeds(t, base)
	challengeOpts := docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeySs58,
		"--hotkey_seed_file": hotkeySeed,
		"--operators-url":    list.listUrl(),
	}
	var challengeOut bytes.Buffer
	if err := hotkeyWalletChallenge(context.Background(), challengeOpts, &challengeOut); err == nil || !strings.Contains(err.Error(), "no authenticated operator") {
		t.Fatalf("challenge with zero authenticated operators must refuse, got %v", err)
	}

	setOpts := docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeySs58,
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--operators-url":     list.listUrl(),
	}
	var setOut bytes.Buffer
	if err := hotkeyWalletSet(context.Background(), setOpts, &setOut); err == nil || !strings.Contains(err.Error(), "no authenticated operator") {
		t.Fatalf("set with zero authenticated operators must refuse, got %v", err)
	}

	var statusOut bytes.Buffer
	if err := hotkeyWalletStatus(context.Background(), docopt.Opts{
		"hotkey":          true,
		"status":          true,
		"--operators-url": list.listUrl(),
	}, &statusOut); err != nil {
		t.Fatalf("status with awaiting operators must exit zero, got %v", err)
	}
	for _, domain := range domains {
		if !strings.Contains(statusOut.String(), "operator "+domain+": awaiting auth") {
			t.Fatalf("status did not report %s awaiting auth:\n%s", domain, statusOut.String())
		}
	}
	if !strings.Contains(statusOut.String(), "operators: --operators-url='"+list.listUrl()+"'") {
		t.Fatalf("status did not print the operator list it used:\n%s", statusOut.String())
	}
}

// Fan-out set: every authenticated operator is attempted, one operator's 401
// is reported without stopping the others, the partial failure is non-zero,
// and each operator sees only its own credential.
func TestHotkeyWalletFanout_SetContinuesAfterOneOperatorRejects(t *testing.T) {
	requireSeedFileCustody(t)
	base := hotkeyWalletTestStateDir(t)
	list := newTestOperatorListServer(t)

	tokenAlpha := fanoutTestJwt(t, "alpha")
	tokenBeta := fanoutTestJwt(t, "beta")
	// gamma's jwt is expired, and the operator rejects it with 401.
	tokenGamma := fanoutExpiredTestJwt(t, "gamma")
	gammaExpected := fanoutTestJwt(t, "gamma-expected")
	alpha := newFanoutOperator(t, tokenAlpha)
	beta := newFanoutOperator(t, tokenBeta)
	// The operator expects another token, so every request carrying the jwt
	// from gamma's state directory gets a 401.
	gamma := newFanoutOperator(t, gammaExpected)

	list.setList(t,
		fanoutListOperator("alpha.example", alpha),
		fanoutListOperator("beta.example", beta),
		fanoutListOperator("gamma.example", gamma),
	)
	writeOperatorJwt(t, base, "alpha.example", tokenAlpha)
	writeOperatorJwt(t, base, "beta.example", tokenBeta)
	writeOperatorJwt(t, base, "gamma.example", tokenGamma)

	hotkeySeed, coldkeySeed, coldkeySs58 := fanoutSeeds(t, base)
	var challengeOut bytes.Buffer
	if err := hotkeyWalletChallenge(context.Background(), docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeySs58,
		"--hotkey_seed_file": hotkeySeed,
		"--operators-url":    list.listUrl(),
	}, &challengeOut); err != nil {
		t.Fatalf("challenge failed: %v\n%s", err, challengeOut.String())
	}
	if !strings.Contains(challengeOut.String(), "--operators-url='"+list.listUrl()+"'") {
		t.Fatalf("challenge's set hint did not carry --operators-url:\n%s", challengeOut.String())
	}

	var setOut bytes.Buffer
	err := hotkeyWalletSet(context.Background(), docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeySs58,
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--operators-url":     list.listUrl(),
	}, &setOut)
	if err == nil {
		t.Fatalf("a set with one failing operator must return an error\n%s", setOut.String())
	}
	if !strings.Contains(err.Error(), "1 of 3 authenticated operators do not adopt the head yet") {
		t.Fatalf("the partial failure did not name the failed operator count: %v", err)
	}
	output := setOut.String()
	if !strings.Contains(output, "operator gamma.example: failed:") {
		t.Fatalf("the failing operator was not reported per operator:\n%s", output)
	}
	if !strings.Contains(output, "operator alpha.example: chain stored;") || !strings.Contains(output, "operator beta.example: chain stored;") {
		t.Fatalf("the two healthy operators were not reported as updated:\n%s", output)
	}

	alpha.mu.Lock()
	alphaChains, alphaDelegations := len(alpha.consents), len(alpha.delegations)
	alpha.mu.Unlock()
	beta.mu.Lock()
	betaChains, betaDelegations := len(beta.consents), len(beta.delegations)
	beta.mu.Unlock()
	gamma.mu.Lock()
	gammaChains, gammaDelegations := len(gamma.consents), len(gamma.delegations)
	gamma.mu.Unlock()
	if alphaChains != 1 || alphaDelegations != 1 {
		t.Fatalf("alpha.example did not store the chain and delegate: %d chains, %d delegations", alphaChains, alphaDelegations)
	}
	if betaChains != 1 || betaDelegations != 1 {
		t.Fatalf("beta.example did not store the chain and delegate: %d chains, %d delegations", betaChains, betaDelegations)
	}
	if gammaChains != 0 || gammaDelegations != 0 {
		t.Fatalf("the rejecting operator stored state: %d chains, %d delegations", gammaChains, gammaDelegations)
	}

	// Credential isolation: every request to an operator carried that
	// operator's own jwt, never another operator's.
	for _, check := range []struct {
		domain string
		op     *fakeOperatorServer
		own    string
		others []string
	}{
		{domain: "alpha.example", op: alpha, own: tokenAlpha, others: []string{tokenBeta, tokenGamma}},
		{domain: "beta.example", op: beta, own: tokenBeta, others: []string{tokenAlpha, tokenGamma}},
		{domain: "gamma.example", op: gamma, own: tokenGamma, others: []string{tokenAlpha, tokenBeta}},
	} {
		seen := fanoutAuthSeen(check.op)
		if len(seen) == 0 {
			t.Fatalf("%s never received a request", check.domain)
		}
		for _, header := range seen {
			if header != "Bearer "+check.own {
				t.Fatalf("%s received a credential other than its own", check.domain)
			}
			for _, other := range check.others {
				if strings.Contains(header, other) {
					t.Fatalf("%s received another operator's credential", check.domain)
				}
			}
		}
	}
}

// Set also reports the listed operators that have no jwt yet, without making
// them a target.
func TestHotkeyWalletFanout_SetReportsAwaitingOperator(t *testing.T) {
	requireSeedFileCustody(t)
	base := hotkeyWalletTestStateDir(t)
	list := newTestOperatorListServer(t)

	tokenAlpha := fanoutTestJwt(t, "alpha")
	alpha := newFanoutOperator(t, tokenAlpha)
	list.setList(t,
		fanoutListOperator("alpha.example", alpha),
		testListedOperator("beta.example"),
	)
	writeOperatorJwt(t, base, "alpha.example", tokenAlpha)

	hotkeySeed, coldkeySeed, coldkeySs58 := fanoutSeeds(t, base)
	var challengeOut bytes.Buffer
	if err := hotkeyWalletChallenge(context.Background(), docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeySs58,
		"--hotkey_seed_file": hotkeySeed,
		"--operators-url":    list.listUrl(),
	}, &challengeOut); err != nil {
		t.Fatalf("challenge failed: %v\n%s", err, challengeOut.String())
	}

	var setOut bytes.Buffer
	if err := hotkeyWalletSet(context.Background(), docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeySs58,
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--operators-url":     list.listUrl(),
	}, &setOut); err != nil {
		t.Fatalf("set with one authenticated operator failed: %v\n%s", err, setOut.String())
	}
	if !strings.Contains(setOut.String(), "operator beta.example: skipped, awaiting auth: provider auth --operator=beta.example") {
		t.Fatalf("set did not report the awaiting operator:\n%s", setOut.String())
	}
	alpha.mu.Lock()
	delegations := len(alpha.delegations)
	alpha.mu.Unlock()
	if delegations != 1 {
		t.Fatalf("the authenticated operator was not delegated to: %d delegations", delegations)
	}
}

// Status fans out too: the authenticated operator, the awaiting one and the
// one whose jwt the operator rejects all get a line, and status exits zero.
func TestHotkeyWalletFanout_StatusReportsEveryOperator(t *testing.T) {
	base := hotkeyWalletTestStateDir(t)
	list := newTestOperatorListServer(t)

	tokenAlpha := fanoutTestJwt(t, "alpha")
	// gamma's jwt is expired, so the operator rejects it with 401.
	tokenGamma := fanoutExpiredTestJwt(t, "gamma")
	alpha := newFanoutOperator(t, tokenAlpha)
	beta := newFanoutOperator(t, fanoutTestJwt(t, "beta"))
	gamma := newFanoutOperator(t, fanoutTestJwt(t, "gamma-expected"))

	list.setList(t,
		fanoutListOperator("alpha.example", alpha),
		fanoutListOperator("beta.example", beta),
		fanoutListOperator("gamma.example", gamma),
	)
	writeOperatorJwt(t, base, "alpha.example", tokenAlpha)
	// beta.example has no jwt: it is awaiting auth.
	writeOperatorJwt(t, base, "gamma.example", tokenGamma)

	var out bytes.Buffer
	if err := hotkeyWalletStatus(context.Background(), docopt.Opts{
		"hotkey":          true,
		"status":          true,
		"--operators-url": list.listUrl(),
	}, &out); err != nil {
		t.Fatalf("status must exit zero with awaiting and unreachable operators, got %v", err)
	}
	output := out.String()
	for _, expected := range []string{
		"operator alpha.example: no hotkey delegation",
		"operator beta.example: awaiting auth",
		"operator gamma.example: unavailable:",
		"operators: --operators-url='" + list.listUrl() + "'",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("status did not print %q:\n%s", expected, output)
		}
	}
}

func TestHotkeyWalletFanout_SubnetDisagreementAndUnreachableOperator(t *testing.T) {
	ctx := context.Background()
	coldkeySs58 := "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"

	epochServer := func(subnet protocol.HotkeyWalletMappingSubnet) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/sn/epoch" {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"epoch":        10,
				"chain_id":     subnet.ChainID,
				"genesis_hash": "0x" + hex.EncodeToString(subnet.GenesisHash[:]),
				"netuid":       subnet.Netuid,
			})
		}))
		t.Cleanup(server.Close)
		return server
	}

	t.Run("operators_disagree", func(t *testing.T) {
		first := epochServer(protocol.HotkeyWalletMappingSubnet{ChainID: 7, GenesisHash: [32]byte{3}, Netuid: 25})
		second := epochServer(protocol.HotkeyWalletMappingSubnet{ChainID: 7, GenesisHash: [32]byte{3}, Netuid: 26})
		targets := []hotkeyWalletTarget{
			{domain: "alpha.example", apiUrl: first.URL, byJwt: "synthetic-alpha"},
			{domain: "beta.example", apiUrl: second.URL, byJwt: "synthetic-beta"},
		}
		var out bytes.Buffer
		if _, err := hotkeyWalletSubnet(ctx, targets, coldkeySs58, &out); err == nil || !strings.Contains(err.Error(), "operators disagree") {
			t.Fatalf("expected the subnet disagreement to abort, got %v", err)
		}
	})

	t.Run("unreachable_skipped", func(t *testing.T) {
		expected := protocol.HotkeyWalletMappingSubnet{ChainID: 7, GenesisHash: [32]byte{3}, Netuid: 25}
		good := epochServer(expected)
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadUrl := dead.URL
		dead.Close()

		targets := []hotkeyWalletTarget{
			{domain: "dead.example", apiUrl: deadUrl, byJwt: "synthetic-dead"},
			{domain: "alpha.example", apiUrl: good.URL, byJwt: "synthetic-alpha"},
		}
		var out bytes.Buffer
		subnet, err := hotkeyWalletSubnet(ctx, targets, coldkeySs58, &out)
		if err != nil {
			t.Fatalf("an unreachable operator must be skipped, got %v", err)
		}
		if subnet != expected {
			t.Fatalf("expected the reachable operator's subnet, got chain %d netuid %d", subnet.ChainID, subnet.Netuid)
		}
		if !strings.Contains(out.String(), "operator dead.example: its subnet is unavailable") {
			t.Fatalf("the unreachable operator was not reported:\n%s", out.String())
		}
	})
}

// Challenge and set need one reachable operator to state the subnet. When no
// authenticated operator can state it, both fail before storing anything.
func TestHotkeyWalletFanout_NoSubnetFailsChallengeAndSet(t *testing.T) {
	requireSeedFileCustody(t)
	base := hotkeyWalletTestStateDir(t)
	list := newTestOperatorListServer(t)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadUrl := dead.URL
	dead.Close()
	list.setList(t, operatorlist.Operator{
		Domain:     "alpha.example",
		ApiUrl:     deadUrl,
		ConnectUrl: strings.Replace(deadUrl, "http://", "ws://", 1),
	})
	writeOperatorJwt(t, base, "alpha.example", fanoutTestJwt(t, "alpha"))

	hotkeySeed, coldkeySeed, coldkeySs58 := fanoutSeeds(t, base)
	var challengeOut bytes.Buffer
	err := hotkeyWalletChallenge(context.Background(), docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeySs58,
		"--hotkey_seed_file": hotkeySeed,
		"--operators-url":    list.listUrl(),
	}, &challengeOut)
	if err == nil || !strings.Contains(err.Error(), "no authenticated operator stated its subnet") {
		t.Fatalf("challenge without a reachable subnet must fail, got %v", err)
	}

	var setOut bytes.Buffer
	err = hotkeyWalletSet(context.Background(), docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeySs58,
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--operators-url":     list.listUrl(),
	}, &setOut)
	if err == nil || !strings.Contains(err.Error(), "no authenticated operator stated its subnet") {
		t.Fatalf("set without a reachable subnet must fail, got %v", err)
	}
}
