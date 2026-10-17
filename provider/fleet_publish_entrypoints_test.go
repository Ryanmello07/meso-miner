//go:build linux || darwin

// Fleet publication checks roles at each command boundary: the pinned-runtime
// command path and the direct authority writer.
package main

import (
	"errors"
	"testing"

	snchain "github.com/urfoundation/sn/chain"
	"github.com/urfoundation/sn/crv4"
)

// The chain945 command has no mainnet authority document, but still refuses a
// validator before asking for a nonce or signing a fleet commitment.
func TestFleetPinnedPublishCommandRefusesValidatorHotkey(t *testing.T) {
	fixture := newFleetMainnetTestFixture(t)
	expected, _, _, _ := fleetRuntimeCurrentTestArtifactInputs(t)
	fixture.version, fixture.code = expected.Version, expected.CodeHash
	fixture.manifest.ChainID = 945
	fixture.opts["--mainnet-runtime-authority"] = ""
	fixture.opts["--mainnet-runtime-authority-sha256"] = ""
	fixture.registerValidatorRole(t, true)
	fixture.opts["--substrate"] = []string{fixture.nativeWebsocket(t, false)}
	err := fleetPublish(fixture.durable.Context, fixture.opts, fixture.manifest)
	if !errors.Is(err, ErrFleetPublishValidatorHotkey) || fixture.count("author_submitAndWatchExtrinsic") != 0 || fixture.count("system_accountNextIndex") != 0 {
		t.Fatalf("pinned command signed or sent from a validator hotkey: %v", err)
	}
}

// The direct authority writer applies the same check, rather than relying on
// the caller to have checked an earlier head.
func TestFleetAuthorityPublishRefusesValidatorHotkey(t *testing.T) {
	fixture := newFleetMainnetTestFixture(t)
	fixture.registerValidatorRole(t, true)
	endpoint := fixture.nativeWebsocket(t, false)
	chain, err := crv4.DialChainContext(t.Context(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFleetNative(chain)
	key, err := snchain.LoadKeypairFile(fleetOpt(fixture.opts, "--hotkey_seed_file"))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := fixture.manifest.CommitmentHash()
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.authority.publish(t.Context(), chain, key, fixture.manifest.Netuid, hash)
	if !errors.Is(err, ErrFleetPublishValidatorHotkey) || fixture.count("author_submitAndWatchExtrinsic") != 0 || fixture.count("system_accountNextIndex") != 0 {
		t.Fatalf("authority writer signed or sent from a validator hotkey: %v", err)
	}
}
