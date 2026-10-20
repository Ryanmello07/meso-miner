package main

// The provider's network sign-in rejection and quarantine.
//
// Ported from the sn module's clientauth/network_token.go and
// clientauth/network_token_quarantine.go at 6608298e. The package cannot be
// imported here: clientauth, unlike hotkeyauth, imports
// github.com/urnetwork/sdk, which this module does not require (v33 provides
// through connect directly). Only the pieces Stage A needs are carried, and
// the behavior is kept line for line:
//
//   - NetworkCredentialRejectedError is the server's confirmed rejection (a
//     401 on the request that carried the network token). Only a new sign-in
//     resolves it.
//   - confirmedClientRefreshRejection is sdk's
//     ConfirmedClientRefreshRejection (api_client_control_json.go): a 401
//     only when every leaf of a joined error is one, including the single
//     Unwrap chain. It uses connect.HttpStatusError, the typed non-200 error
//     this line's HTTP helpers now return with unchanged text.
//   - The set-aside file is <token>.rejected-<unixnano>-<consecutive> beside
//     the token, newest three kept. The name carries the rejection time and
//     the run length, so the supervisor's limit on automatic sign-ins
//     survives its own restart.
//
// Adaptation: sn renames under the token's registration owner lock
// (registrationStore), because a renewal there can race an explicit sign-in.
// This line has no registration store and no network token renewal, so the
// rename is the only writer; the check that the file still holds the rejected
// token immediately precedes the rename.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// RegistrationRefusedError is sn clientauth.RegistrationRefusedError: a
// custody decision that refuses to invent an identity, with a stable code.
// The text is sn's.
type RegistrationRefusedError struct{ Code string }

func (self *RegistrationRefusedError) Error() string {
	return "client registration requires recovery: " + self.Code
}

// NetworkCredentialRejectedError is the server's confirmed rejection (401) of
// the network token that a registration or a renewal sent: the sign-in has
// expired, its credentials were rotated, or its network is gone. Only a new
// sign-in resolves it.
type NetworkCredentialRejectedError struct{ cause error }

func (self *NetworkCredentialRejectedError) Error() string {
	return "the network sign-in was rejected or has expired"
}

func (self *NetworkCredentialRejectedError) Unwrap() error { return self.cause }

// confirmedClientRefreshRejection is sdk.ConfirmedClientRefreshRejection: only
// a complete all-leaf HTTP 401 verdict may revoke a captured credential. A
// timeout or malformed response joined to a 401 is not confirmed revocation.
func confirmedClientRefreshRejection(err error) bool {
	if status, ok := err.(*connect.HttpStatusError); ok {
		return status.StatusCode == http.StatusUnauthorized
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !confirmedClientRefreshRejection(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return confirmedClientRefreshRejection(wrapped.Unwrap())
	}
	return false
}

// networkCredentialRejection types a confirmed rejection of the network token
// a request carried. Every other error is returned as it is.
func networkCredentialRejection(err error) error {
	if err != nil && confirmedClientRefreshRejection(err) {
		return &NetworkCredentialRejectedError{cause: err}
	}
	return err
}

// IsNetworkCredentialRejected reports whether err holds the server's
// confirmed rejection of a network token.
func IsNetworkCredentialRejected(err error) bool {
	var rejected *NetworkCredentialRejectedError
	return errors.As(err, &rejected)
}

// How many set-aside files are kept beside the token.
const maximumNetworkTokenQuarantine = 3

// Rejections closer together than this count as consecutive: a sign-in that
// does not hold for a day is rejected again in a row. Longer than the
// supervisor's largest wait between automatic sign-ins, so a chain at that
// wait stays consecutive.
const networkTokenQuarantineConsecutiveWindow = 25 * time.Hour

// NetworkTokenQuarantine is one set-aside rejected network token.
type NetworkTokenQuarantine struct {
	// the file the token was renamed to
	Path string
	Time time.Time
	// rejections in a row, this one included
	Consecutive int
}

func networkTokenQuarantinePrefix(name string) string {
	return name + ".rejected-"
}

func networkTokenQuarantineName(name string, at time.Time, consecutive int) string {
	return networkTokenQuarantinePrefix(name) + strconv.FormatInt(at.UnixNano(), 10) + "-" + strconv.Itoa(consecutive)
}

// parseNetworkTokenQuarantineName reads the time and the count from a
// set-aside file's name, or false for any other name.
func parseNetworkTokenQuarantineName(name string, leaf string) (time.Time, int, bool) {
	rest, ok := strings.CutPrefix(leaf, networkTokenQuarantinePrefix(name))
	if !ok {
		return time.Time{}, 0, false
	}
	nanos, count, ok := strings.Cut(rest, "-")
	if !ok {
		return time.Time{}, 0, false
	}
	at, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil || at <= 0 || strconv.FormatInt(at, 10) != nanos {
		return time.Time{}, 0, false
	}
	consecutive, err := strconv.Atoi(count)
	if err != nil || consecutive < 1 || strconv.Itoa(consecutive) != count {
		return time.Time{}, 0, false
	}
	return time.Unix(0, at).UTC(), consecutive, true
}

// networkTokenQuarantines lists the set-aside files of the token name among a
// directory's names, oldest first.
func networkTokenQuarantines(dir string, name string, names []string) []NetworkTokenQuarantine {
	var quarantines []NetworkTokenQuarantine
	for _, leaf := range names {
		if at, consecutive, ok := parseNetworkTokenQuarantineName(name, leaf); ok {
			quarantines = append(quarantines, NetworkTokenQuarantine{Path: filepath.Join(dir, leaf), Time: at, Consecutive: consecutive})
		}
	}
	slices.SortFunc(quarantines, func(a, b NetworkTokenQuarantine) int { return a.Time.Compare(b.Time) })
	return quarantines
}

// nextNetworkTokenQuarantine is the time and count of a quarantine at now
// after the given ones: one more in a row when the latest is within the
// consecutive window, else the first. Its time is strictly after the latest,
// so names never collide and order is kept.
func nextNetworkTokenQuarantine(quarantines []NetworkTokenQuarantine, now time.Time) (time.Time, int) {
	// the time as its name records it
	now = time.Unix(0, now.UnixNano()).UTC()
	if len(quarantines) == 0 {
		return now, 1
	}
	latest := quarantines[len(quarantines)-1]
	at := now
	if !at.After(latest.Time) {
		at = latest.Time.Add(time.Nanosecond)
	}
	if at.Sub(latest.Time) < networkTokenQuarantineConsecutiveWindow {
		return at, latest.Consecutive + 1
	}
	return at, 1
}

// LatestNetworkTokenQuarantine is the newest set-aside file of the token at
// path, or false when there is none.
func LatestNetworkTokenQuarantine(path string) (NetworkTokenQuarantine, bool, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return NetworkTokenQuarantine{}, false, nil
	}
	if err != nil {
		return NetworkTokenQuarantine{}, false, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	quarantines := networkTokenQuarantines(filepath.Dir(path), filepath.Base(path), names)
	if len(quarantines) == 0 {
		return NetworkTokenQuarantine{}, false, nil
	}
	return quarantines[len(quarantines)-1], true, nil
}

// QuarantineNetworkToken sets the rejected network token at path aside: it
// renames the file, keeping its mode and content, to
// <name>.rejected-<unixnano>-<consecutive> in the same directory. It acts only
// while the file still holds rejected: a sign-in that replaced it first is
// never set aside (false, nil). It keeps the newest
// maximumNetworkTokenQuarantine set-aside files, the one it writes always
// among them.
func QuarantineNetworkToken(path string, rejected string, now time.Time) (_ NetworkTokenQuarantine, _ bool, returnErr error) {
	rejected = strings.TrimSpace(rejected)
	if rejected == "" {
		return NetworkTokenQuarantine{}, false, errors.New("network token quarantine needs the rejected token")
	}
	current, err := readOperatorJwt(path)
	if errors.Is(err, os.ErrNotExist) {
		return NetworkTokenQuarantine{}, false, nil
	}
	if err != nil {
		return NetworkTokenQuarantine{}, false, err
	}
	if current != rejected {
		return NetworkTokenQuarantine{}, false, nil
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return NetworkTokenQuarantine{}, false, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	name := filepath.Base(path)
	quarantines := networkTokenQuarantines(filepath.Dir(path), name, names)
	at, consecutive := nextNetworkTokenQuarantine(quarantines, now)
	target := networkTokenQuarantineName(name, at, consecutive)
	// the check above and this rename are the only writers of the token file
	// on this line, so nothing can land between them; the read is repeated
	// immediately before the rename to keep that true if an auth command
	// replaces the file first
	if current, err = readOperatorJwt(path); err != nil || current != rejected {
		return NetworkTokenQuarantine{}, false, err
	}
	if err := os.Rename(path, filepath.Join(filepath.Dir(path), target)); err != nil {
		return NetworkTokenQuarantine{}, false, err
	}
	quarantine := NetworkTokenQuarantine{Path: filepath.Join(filepath.Dir(path), target), Time: at, Consecutive: consecutive}
	// the oldest beyond the bound go; the one just written is the newest
	kept := append(quarantines, quarantine)
	for _, old := range kept[:max(0, len(kept)-maximumNetworkTokenQuarantine)] {
		if err := os.Remove(old.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return quarantine, true, err
		}
	}
	return quarantine, true, nil
}

// The provider child of provide --all-operators --auto-register exits with
// this code after it sets a rejected network sign-in aside, so its supervisor
// signs in again with the hotkey. It matches sn's providerQuarantinedExitCode
// (EX_TEMPFAIL).
const providerQuarantinedExitCode = 75

// operatorAutomaticSignInAt is the earliest automatic sign-in after the latest
// set-aside network sign-in: at once after the first rejection in a while,
// else operatorAutomaticSignInMinimumDelay after the latest, doubling with
// each rejection in a row up to operatorAutomaticSignInMaximumDelay.
func operatorAutomaticSignInAt(quarantine NetworkTokenQuarantine) time.Time {
	if quarantine.Consecutive <= 1 {
		return quarantine.Time
	}
	delay := operatorAutomaticSignInMinimumDelay << min(quarantine.Consecutive-2, 5)
	return quarantine.Time.Add(min(delay, operatorAutomaticSignInMaximumDelay))
}

// operatorAutomaticSignInLimitedLog formats the one log line per changed
// limit, without ever printing the token.
func operatorAutomaticSignInLimitedLog(domain string, quarantine NetworkTokenQuarantine, allowedAt time.Time) string {
	return fmt.Sprintf("operator %s: automatic sign-in is limited after %d rejected network sign-ins in a row; the next is at %s. Run `provider auth --operator=%s` to sign in now", domain, quarantine.Consecutive, allowedAt.UTC().Format(time.RFC3339), domain)
}

// providerClientIdentityGate refuses to invent a provider client identity.
//
// sn gates client creation in clientauth.OpenProviderClientKey: a missing
// .provider.key is recovery work unless the caller explicitly allows creation.
// This line's provide path generated a seed and persisted it unconditionally;
// here the same decision is made before any client exists. An existing
// nonempty seed (including a legacy one) is retained as before.
func providerClientIdentityGate(opts docopt.Opts) error {
	if allow, _ := opts.Bool("--allow-client-registration"); allow {
		return nil
	}
	seed, err := readProviderClientKeySeed()
	if err != nil {
		return err
	}
	if 0 < len(seed) {
		// sn requires a nonzero 32-byte seed; anything else cannot be the
		// retained identity and must not be replaced silently
		nonzero := false
		for _, b := range seed {
			if b != 0 {
				nonzero = true
				break
			}
		}
		if len(seed) != 32 || !nonzero {
			return errors.New("provider client key must be an existing nonzero 32-byte seed")
		}
		return nil
	}
	return &RegistrationRefusedError{Code: "provider_key_requires_explicit_recovery"}
}

// providerClientIdentityGateForOpts applies the strict identity gate only to
// an operator child of provide --all-operators. Plain provide and auth-provide
// keep the v33 behavior: a missing .provider.key is minted and persisted on
// first run, so an existing single-network install (Docker, Pelican, the
// installer) upgrades without a flag.
func providerClientIdentityGateForOpts(opts docopt.Opts) error {
	if operatorChild, _ := opts.Bool("--operator-child"); !operatorChild {
		return nil
	}
	return providerClientIdentityGate(opts)
}

// providerStateJwtPath is the provider's own network token, the one its child
// process renews and the one a rejection sets aside.
func providerStateJwtPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "jwt"), nil
}

// quarantineRejectedOperatorSignIn sets the provider's network token aside
// when err is the server's confirmed rejection and the process was started
// with --quarantine-rejected-sign-in. It reports whether it quarantined; the
// caller then exits with providerQuarantinedExitCode so its supervisor signs
// in again.
func quarantineRejectedOperatorSignIn(opts docopt.Opts, err error) (NetworkTokenQuarantine, bool, error) {
	if !IsNetworkCredentialRejected(err) {
		return NetworkTokenQuarantine{}, false, nil
	}
	if quarantine, _ := opts.Bool("--quarantine-rejected-sign-in"); !quarantine {
		return NetworkTokenQuarantine{}, false, nil
	}
	jwtPath, err := providerStateJwtPath()
	if err != nil {
		return NetworkTokenQuarantine{}, false, err
	}
	token, err := readOperatorJwt(jwtPath)
	if err != nil {
		return NetworkTokenQuarantine{}, false, err
	}
	return QuarantineNetworkToken(jwtPath, token, time.Now())
}
