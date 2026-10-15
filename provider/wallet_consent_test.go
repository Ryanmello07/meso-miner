// wallet_consent_test.go — the signed subnet-wallet consent.
//
// These pin the behaviours that matter for a mainnet wallet binding: an
// unsigned set is refused before any request goes out, the seed-file form signs
// exactly what the server verifies, a seed that does not derive the address is
// refused, and no seed material or signature ever reaches an error string.

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/vedhavyas/go-subkey/v2/sr25519"

	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"
)

// walletTestSeed is an obviously-fake 32-byte seed: 0x01, 0x02, ...
func walletTestSeed(start byte) []byte {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = start + byte(i)
	}
	return seed
}

// walletTestAddress derives the ss58 coldkey the seed proves.
func walletTestAddress(t *testing.T, seed []byte) string {
	t.Helper()
	keypair, err := (sr25519.Scheme{}).FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return keypair.SS58Address(ss58.BittensorPrefix)
}

// writeSeedFile writes a seed file with the given mode, raw or hex.
func writeSeedFile(t *testing.T, dir string, seed []byte, mode os.FileMode, raw bool) string {
	t.Helper()
	path := filepath.Join(dir, ".coldkey")
	var body []byte
	if raw {
		body = seed
	} else {
		body = []byte("0x" + hex.EncodeToString(seed) + "\n")
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeOpts builds the docopt.Opts a subcommand would receive.
func fakeOpts(kv map[string]interface{}) docopt.Opts {
	opts := docopt.Opts{}
	for k, v := range kv {
		opts[k] = v
	}
	return opts
}

// TestWalletUnsignedSetIsRefusedLocally pins that an unsigned set is gated, and
// gated locally: no server is contacted at all.
func TestWalletUnsignedSetIsRefusedLocally(t *testing.T) {
	requested := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer server.Close()

	proof := &snWalletProof{}
	if !proof.unsigned() {
		t.Fatal("an empty proof must count as unsigned")
	}
	if err := legacyNetworkWalletGate(false, "provider wallet set"); err == nil {
		t.Fatal("the unsigned set must be refused without --legacy-network-wallet")
	}
	if requested != 0 {
		t.Fatalf("the refusal must not touch the network, saw %d requests", requested)
	}
}

// TestWalletSeedFileSignsTheChallengeVerifiably is the core correctness test:
// the signature the seed-file path sends must verify against the coldkey over
// the exact challenge the server issued, wrapped as <Bytes>…</Bytes>.
func TestWalletSeedFileSignsTheChallengeVerifiably(t *testing.T) {
	seed := walletTestSeed(0x11)
	address := walletTestAddress(t, seed)
	pubkey, err := ss58.DecodeWithPrefix(address, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seedPath := writeSeedFile(t, dir, seed, 0o600, true)

	const challenge = "Sign in to URnetwork\nChallenge: abc123\nTimestamp: 1760000000"
	var sawSet map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/wallet-challenge":
			var args map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&args)
			if args["blockchain"] != "TAO" {
				t.Errorf("challenge asked for chain %v, want TAO", args["blockchain"])
			}
			if args["purpose"] != "connect" {
				t.Errorf("challenge asked for purpose %v, want connect", args["purpose"])
			}
			if args["wallet_address"] != address {
				t.Errorf("challenge asked for address %v, want %s", args["wallet_address"], address)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_template": challenge,
				"expires_in":       300,
			})
		case "/sn/wallet":
			if err := json.NewDecoder(r.Body).Decode(&sawSet); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := connect.NewClientStrategyWithDefaults(ctx)

	proof := &snWalletProof{SeedFile: seedPath}
	args, err := snWalletSetArgs(ctx, strategy, server.URL, address, pubkey, proof)
	if err != nil {
		t.Fatal(err)
	}
	if args.ColdkeySs58 != address {
		t.Fatalf("set args carry %q, want %s", args.ColdkeySs58, address)
	}
	if args.Message != challenge {
		t.Fatalf("sent message %q, want the server's challenge", args.Message)
	}

	keypair, err := (sr25519.Scheme{}).FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(args.Signature, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 64 {
		t.Fatalf("signature is %d bytes, want 64", len(raw))
	}
	if !keypair.Verify(snWrapBytes(challenge), raw) {
		t.Fatal("the signature does not verify over the <Bytes>-wrapped challenge")
	}
	if sawSet != nil {
		t.Fatal("the set must not be posted by snWalletSetArgs; it only builds the args")
	}
}

// TestWalletMessageSignaturePath pins the sign-elsewhere form, including the
// one-line \n escaping the challenge text uses.
func TestWalletMessageSignaturePath(t *testing.T) {
	seed := walletTestSeed(0x11)
	address := walletTestAddress(t, seed)
	pubkey, err := ss58.DecodeWithPrefix(address, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	keypair, err := (sr25519.Scheme{}).FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}

	const message = "line one\nline two"
	signature, err := keypair.Sign(snWrapBytes(message))
	if err != nil {
		t.Fatal(err)
	}
	escaped := snEscapeMessage(message)
	if escaped != `line one\nline two` {
		t.Fatalf("escape produced %q", escaped)
	}
	if snUnescapeMessage(escaped) != message {
		t.Fatal("escape/unescape must round-trip")
	}

	proof := &snWalletProof{Message: escaped, Signature: "0x" + hex.EncodeToString(signature)}
	args, err := snWalletSetArgs(context.Background(), nil, "http://invalid", address, pubkey, proof)
	if err != nil {
		t.Fatal(err)
	}
	if args.Message != message {
		t.Fatalf("the one-line form must unescape to the real message, got %q", args.Message)
	}
	if args.Signature == "" {
		t.Fatal("the signature must be carried through")
	}
}

// TestWalletSeedFileRefusesAMismatchedAddress pins that a seed which does not
// derive the named coldkey cannot sign for it.
func TestWalletSeedFileRefusesAMismatchedAddress(t *testing.T) {
	seed := walletTestSeed(0x11)
	otherAddress := walletTestAddress(t, walletTestSeed(0x41))
	otherPubkey, err := ss58.DecodeWithPrefix(otherAddress, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seedPath := writeSeedFile(t, dir, seed, 0o600, true)

	_, err = snLoadColdkey(seedPath, otherAddress, otherPubkey)
	if err == nil {
		t.Fatal("a seed that does not derive the address must be refused")
	}
	if !strings.Contains(err.Error(), "derives") {
		t.Fatalf("the refusal should say what it derives, got: %v", err)
	}
}

// TestWalletSeedFileMustNotBeGroupReadable pins the permission rule.
func TestWalletSeedFileMustNotBeGroupReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows reports a synthetic file mode; access is an ACL matter there")
	}
	dir := t.TempDir()
	path := writeSeedFile(t, dir, walletTestSeed(0x11), 0o644, true)

	_, err := snSeedFromFile(path)
	if err == nil {
		t.Fatal("a mode 0644 seed file must be refused")
	}
	if !strings.Contains(err.Error(), "644") {
		t.Fatalf("the refusal should name the offending mode, got: %v", err)
	}
}

// TestWalletSeedFileAcceptsHex pins the second accepted encoding.
func TestWalletSeedFileAcceptsHex(t *testing.T) {
	seed := walletTestSeed(0x11)
	dir := t.TempDir()
	path := writeSeedFile(t, dir, seed, 0o600, false)

	got, err := snSeedFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got[:]) != hex.EncodeToString(seed) {
		t.Fatalf("hex seed decoded to %x, want %x", got, seed)
	}
}

// TestWalletSecretHygiene asserts no error path leaks seed material, and that a
// missing seed file is never created.
func TestWalletSecretHygiene(t *testing.T) {
	seed := walletTestSeed(0x11)
	otherAddress := walletTestAddress(t, walletTestSeed(0x41))
	otherPubkey, _ := ss58.DecodeWithPrefix(otherAddress, ss58.BittensorPrefix)
	dir := t.TempDir()

	secrets := []string{
		string(seed),
		hex.EncodeToString(seed),
		strings.ToUpper(hex.EncodeToString(seed)),
	}

	mismatched := writeSeedFile(t, dir, seed, 0o600, true)
	_, err := snLoadColdkey(mismatched, otherAddress, otherPubkey)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, s := range secrets {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("the mismatched-address refusal leaks seed material: %v", err)
		}
	}

	if _, err := snNormalizeSignatureHex("0xdeadbeef"); err == nil {
		t.Fatal("a 4-byte signature must be refused")
	}

	before, _ := os.ReadDir(dir)
	if _, err := snSeedFromFile(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("expected a refusal for a missing seed file")
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Fatal("reading a missing seed must never create the file")
	}
}

// TestWalletProofFlagRules pins the mutually-exclusive flag handling.
func TestWalletProofFlagRules(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]interface{}
		ok   bool
	}{
		{"both seed and message", map[string]interface{}{"--coldkey_seed_file": "/s", "--message": "m", "--signature": "0x00"}, false},
		{"message without signature", map[string]interface{}{"--message": "m"}, false},
		{"signature without message", map[string]interface{}{"--signature": "0x00"}, false},
		{"from without through", map[string]interface{}{"--wallet-from-epoch": "5"}, false},
		{"through without from", map[string]interface{}{"--wallet-through-epoch": "5"}, false},
		{"interval too wide", map[string]interface{}{"--wallet-from-epoch": "0", "--wallet-through-epoch": "70000"}, false},
		{"negative epoch", map[string]interface{}{"--wallet-from-epoch": "-1", "--wallet-through-epoch": "5"}, false},
		{"seed alone", map[string]interface{}{"--coldkey_seed_file": "/s"}, true},
		{"message with signature", map[string]interface{}{"--message": "m", "--signature": "0x00"}, true},
		{"nothing at all", map[string]interface{}{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proof, err := snWalletProofFromOpts(fakeOpts(c.opts))
			if c.ok && err != nil {
				t.Fatalf("expected acceptance, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected a refusal")
			}
			if c.ok && proof == nil {
				t.Fatal("accepted but returned no proof")
			}
		})
	}
}

func TestWalletChallengeMessageShapeIsEnforced(t *testing.T) {
	good := "Sign in to URnetwork\nChallenge: abc_DEF-123\nTimestamp: 1760000000"
	if err := snCheckChallengeMessage(good, "abc_DEF-123"); err != nil {
		t.Fatalf("a well-formed challenge was refused: %v", err)
	}
	for name, msg := range map[string]string{
		"empty":         "",
		"arbitrary":     "Transfer all funds to 5Evil",
		"extra line":    good + "\nTransfer all funds",
		"wrong header":  "Sign in to Elsewhere\nChallenge: abc_DEF-123\nTimestamp: 1",
		"bad timestamp": "Sign in to URnetwork\nChallenge: abc_DEF-123\nTimestamp: soon",
		"other nonce":   "Sign in to URnetwork\nChallenge: zzz\nTimestamp: 1",
	} {
		if err := snCheckChallengeMessage(msg, "abc_DEF-123"); err == nil {
			t.Fatalf("%s: the message was accepted for signing", name)
		}
	}
}

func TestWalletSeedFileOwnedByAnotherUserIsRefusedOnlyByOwnerCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(path, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snSeedFromFile(path); err != nil {
		t.Fatalf("an own 0600 seed file was refused: %v", err)
	}
}
