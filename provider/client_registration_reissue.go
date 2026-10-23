package main

// Operator reissue of one revoked provider slot.
//
// The server keeps a slot's registration binding after its client is deleted,
// so a revoked slot is refused forever (client_registration.go). Upstream
// allows a fresh registration only as an explicit operator assertion; this is
// that assertion for one slot. It moves the slot to its next generation, a
// scope the server has never bound, and drops the slot's saved client JWT.
// The next registration for the slot then creates a new client. Every other
// slot keeps its client, and the provider key does not change.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

const providerRegistrationGenerationsName = ".client_registration_generations.json"

// registrationGenerationKey normalizes the direct slot's identity key.
func registrationGenerationKey(identityKey string) string {
	if identityKey == "" {
		return "direct"
	}
	return identityKey
}

// readProviderRegistrationGenerations returns each reissued slot's generation,
// keyed by identity key. Slots never reissued are absent (generation 0).
func readProviderRegistrationGenerations() (map[string]uint64, error) {
	path, err := providerStatePath(providerRegistrationGenerationsName)
	if err != nil {
		return nil, err
	}
	generations := map[string]uint64{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return generations, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &generations); err != nil {
		// an unreadable file must not silently return reissued slots to
		// their revoked generation
		return nil, fmt.Errorf("%s is unreadable (%v); restore it from a backup", providerRegistrationGenerationsName, err)
	}
	return generations, nil
}

// reissueProviderSlot moves one slot to its next generation and drops its
// saved client JWT. Unless force, the slot must have a retained registration
// at its current generation, so a mistyped proxy changes nothing. It returns
// the new generation.
func reissueProviderSlot(identityKey string, force bool) (uint64, error) {
	identityKey = registrationGenerationKey(identityKey)
	storePath, err := providerRegistrationStorePath()
	if err != nil {
		return 0, err
	}
	generation, err := func() (uint64, error) {
		providerRegistrationLock.Lock()
		defer providerRegistrationLock.Unlock()
		release, err := acquireJWTStoreLock(storePath)
		if err != nil {
			return 0, err
		}
		defer release()
		generations, err := readProviderRegistrationGenerations()
		if err != nil {
			return 0, err
		}
		current := generations[identityKey]
		if !force {
			records, err := readProviderRegistrations(storePath)
			if err != nil {
				return 0, err
			}
			slot := providerRegistrationSlot(identityKey, current)
			found := false
			for _, record := range records {
				if record.Scope.ClientSlot == slot {
					found = true
					break
				}
			}
			if !found {
				return 0, errors.New("no registration is saved for that proxy; check the address and user, or pass --yes to reissue it anyway")
			}
		}
		generations[identityKey] = current + 1
		raw, err := json.MarshalIndent(generations, "", "  ")
		if err != nil {
			return 0, err
		}
		path, err := providerStatePath(providerRegistrationGenerationsName)
		if err != nil {
			return 0, err
		}
		if err := durableWriteFile(path, raw, 0600); err != nil {
			return 0, err
		}
		return current + 1, nil
	}()
	if err != nil {
		return 0, err
	}
	// the saved JWT names the revoked client; without it the next start
	// registers the slot instead of reusing it
	if err := globalClientJWTStore.Delete(identityKey); err != nil {
		return generation, fmt.Errorf("slot reissued, but its saved client JWT could not be removed: %w", err)
	}
	return generation, nil
}

// proxyReissueIdentityKey builds the identity key the provider uses for a
// proxy: its address, plus its user when it signs in (connect.ProxySettings.Key).
func proxyReissueIdentityKey(opts docopt.Opts) (string, error) {
	if direct, _ := opts.Bool("--direct"); direct {
		return "direct", nil
	}
	address, _ := opts.String("<address>")
	address = strings.TrimSpace(address)
	if address == "" {
		return "", errors.New("name the proxy address, or pass --direct")
	}
	settings := &connect.ProxySettings{Address: address}
	if user, _ := opts.String("--proxy-user"); user != "" {
		settings.Auth = &proxy.Auth{User: user}
	}
	return settings.Key(), nil
}

func proxyReissue(opts docopt.Opts) {
	identityKey, err := proxyReissueIdentityKey(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reissue: %v\n", err)
		os.Exit(2)
	}
	force, _ := opts.Bool("--yes")
	generation, err := reissueProviderSlot(identityKey, force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reissue: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Reissued %s (generation %d). Restart the provider; this proxy registers a new client on its next sign-in. Other proxies keep their clients.\n", describeIdentityKey(identityKey), generation)
}

// describeIdentityKey formats an identity key for operators (the raw key's
// separator must not be printed).
func describeIdentityKey(identityKey string) string {
	if identityKey == "direct" {
		return "the direct connection"
	}
	address, user := connect.SplitProxyKey(identityKey)
	if user == "" {
		return "proxy " + address
	}
	return fmt.Sprintf("proxy %s (user %s)", address, user)
}

// proxyReissueArgs is the reissue command's arguments for an identity key.
func proxyReissueArgs(identityKey string) string {
	if registrationGenerationKey(identityKey) == "direct" {
		return "--direct"
	}
	address, user := connect.SplitProxyKey(identityKey)
	if user == "" {
		return address
	}
	return fmt.Sprintf("%s --proxy-user=%s", address, user)
}
