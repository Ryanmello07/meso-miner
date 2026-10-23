package main

// Durable provider client registration (POST /network/register-client-v1).
//
// Ported in intent from urnetwork/sdk api_client_registration.go and
// urfoundation/sn clientauth/registration.go. The legacy /network/auth-client
// creation route is not idempotent: when its reply is lost (a proxy drops the
// connection after the server committed), a retry allocates a SECOND client
// for the same proxy, and on a bulk node that leaks client slots against the
// network's limit. The versioned route binds one network principal and one
// exact, retained request to one server-issued client/device:
//
//   - The request (an opaque random registration id plus the SHA-256 of a
//     stable scope) is written durably BEFORE the first send. Every retry,
//     including after a crash or restart, replays those exact bytes, and the
//     server answers with the identity it already allocated.
//   - The scope is the provider role, the exact endpoint, the provider's
//     long-lived client key (.provider.key) and the proxy slot. The server
//     keeps that binding even after the client is deleted, so a revoked
//     identity is never recreated by a replay or a new request for the same
//     scope. A slot whose identity was revoked stays refused.
//   - There is no fallback to the legacy route: the server answers an older
//     client's missing route as unsupported, and upstream forbids falling back
//     because the fallback reintroduces the duplicate allocation.
//
// Meso adaptations: one JSON store (.client_registrations.json) for every
// slot of a state dir, under the same inter-process lock helper as the client
// JWT store, keyed by network and scope so an account switch or a key change
// starts its own operation instead of replaying a foreign one. The proxy slot
// is the SHA-256 of meso's proxy identity key (address plus user, no
// password), because meso runs several accounts at one address as distinct
// identities. Renewal of an existing identity is unchanged (auth-client with
// the stored client id); only creation moved to this route.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
)

const (
	networkClientRegistrationSchema  = "urnetwork-client-registration-v1"
	providerRegistrationRecordSchema = "urnetwork-durable-client-registration-v1"
	providerRegistrationStoreName    = ".client_registrations.json"
	providerRegistrationRole         = "provider-v1"
	// the server route's encoded request bound
	networkClientRegistrationMaxRequestBytes = 16 * 1024
)

// clientRegistrationScope matches sn clientauth.RegistrationScope field for
// field, so the canonical JSON and its hash have upstream's shape.
type clientRegistrationScope struct {
	Endpoint     string `json:"endpoint"`
	DeploymentId string `json:"deployment_id"`
	ChainId      uint64 `json:"chain_id"`
	GenesisHash  string `json:"genesis_hash"`
	Netuid       uint16 `json:"netuid"`
	ValidatorId  uint64 `json:"validator_id"`
	OperatorNoId uint64 `json:"operator_no_id"`
	ClientKey    string `json:"client_key"`
	ClientRole   string `json:"client_role,omitempty"`
	ClientSlot   string `json:"client_slot,omitempty"`
}

// Field order is the canonical request-hash format shared with the server.
type registerNetworkClientArgs struct {
	Schema            string `json:"schema"`
	RegistrationId    string `json:"registration_id"`
	ScopeSha256       string `json:"scope_sha256"`
	DeviceDescription string `json:"description"`
	DeviceSpec        string `json:"device_spec"`
}

type registerNetworkClientResult struct {
	Schema         string                      `json:"schema"`
	RegistrationId string                      `json:"registration_id"`
	RequestSha256  string                      `json:"request_sha256"`
	ClientId       *connect.Id                 `json:"client_id,omitempty"`
	DeviceId       *connect.Id                 `json:"device_id,omitempty"`
	ByClientJwt    string                      `json:"by_client_jwt,omitempty"`
	Error          *registerNetworkClientError `json:"error,omitempty"`
}

type registerNetworkClientError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable principal fields: no token timestamps, bearer bytes or builds.
type clientRegistrationPrincipal struct {
	NetworkId string   `json:"network_id"`
	UserId    string   `json:"user_id"`
	Roles     []string `json:"roles"`
	Principal string   `json:"principal"`
}

func (self clientRegistrationPrincipal) equal(other clientRegistrationPrincipal) bool {
	return self.NetworkId == other.NetworkId && self.UserId == other.UserId && self.Principal == other.Principal && slices.Equal(self.Roles, other.Roles)
}

type clientRegistrationRecord struct {
	Schema    string                      `json:"schema"`
	Scope     clientRegistrationScope     `json:"scope"`
	Principal clientRegistrationPrincipal `json:"principal"`
	Request   registerNetworkClientArgs   `json:"request"`
	ClientId  string                      `json:"client_id,omitempty"`
	DeviceId  string                      `json:"device_id,omitempty"`
}

// ProviderRegistrationRefusedError is the server's refusal of the retained
// request. It is not stored: like upstream, the next attempt replays the same
// request, so a refusal the server later lifts (a client limit) clears itself,
// while a completed one (a revoked identity) is answered again.
type ProviderRegistrationRefusedError struct{ Code string }

func (self *ProviderRegistrationRefusedError) Error() string {
	return "provider client registration refused: " + self.Code
}

// ProviderRegistrationUnavailableError is a request whose reply is unknown.
// The server may have committed it; the only safe retry is the same request.
type ProviderRegistrationUnavailableError struct{ cause error }

func (self *ProviderRegistrationUnavailableError) Error() string {
	return fmt.Sprintf("provider client registration reply unavailable (the retained request will be replayed): %v", self.cause)
}

func (self *ProviderRegistrationUnavailableError) Unwrap() error { return self.cause }

// ProviderRegistrationUnsupportedError is a server without the versioned
// route. There is no fallback to legacy creation.
type ProviderRegistrationUnsupportedError struct{ Status int }

func (self *ProviderRegistrationUnsupportedError) Error() string {
	return fmt.Sprintf("server does not support versioned client registration (HTTP %d)", self.Status)
}

// providerRegistrationLock serializes registrations within this process; the
// file lock serializes them with a hot-swap partner process.
var providerRegistrationLock sync.Mutex

// providerRegistrationSlot is "direct" or "proxy-sha256:" plus the SHA-256 of
// the proxy identity key. A slot the operator reissued (generation N > 0) is
// "proxy-sha256:" plus the SHA-256 of the identity key and its generation, a
// scope the server has never bound.
func providerRegistrationSlot(identityKey string, generation uint64) string {
	if identityKey == "" {
		identityKey = "direct"
	}
	if generation == 0 {
		if identityKey == "direct" {
			return "direct"
		}
		sum := sha256.Sum256([]byte(identityKey))
		return "proxy-sha256:" + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00reissue\x00%d", identityKey, generation))
	return "proxy-sha256:" + hex.EncodeToString(sum[:])
}

func networkClientRegistrationEndpoint(apiUrl string) string {
	return strings.TrimRight(apiUrl, "/") + "/network/register-client-v1"
}

// registrationClaims reads the identity claims of a JWT. The API remains the
// authority on signatures; these claims only select local ownership.
func registrationClaims(token string) (principal clientRegistrationPrincipal, clientId string, deviceId string, err error) {
	claims := gojwt.MapClaims{}
	if _, _, parseErr := gojwt.NewParser().ParseUnverified(token, claims); parseErr != nil {
		err = errors.New("registration credential is malformed")
		return
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"network_id", &principal.NetworkId}, {"user_id", &principal.UserId}, {"principal", &principal.Principal},
		{"client_id", &clientId}, {"device_id", &deviceId},
	} {
		if raw, ok := claims[field.name]; ok && raw != nil {
			value, ok := raw.(string)
			if !ok {
				err = fmt.Errorf("registration claim %s has an invalid type", field.name)
				return
			}
			*field.value = value
		}
	}
	if raw, ok := claims["roles"]; ok && raw != nil {
		roles, ok := raw.([]any)
		if !ok {
			err = errors.New("registration roles have an invalid type")
			return
		}
		for _, raw := range roles {
			role, ok := raw.(string)
			if !ok || role == "" {
				err = errors.New("registration role is invalid")
				return
			}
			principal.Roles = append(principal.Roles, role)
		}
		slices.Sort(principal.Roles)
		principal.Roles = slices.Compact(principal.Roles)
	}
	return
}

func validRegistrationId(value string) bool {
	id, err := connect.ParseId(value)
	return err == nil && id != (connect.Id{}) && id.String() == value
}

// ensureProviderClientKeySeed returns the provider's long-lived client key
// seed, creating and durably publishing it first when the state dir has none.
// The registration scope names the key, so it must exist before any request.
func ensureProviderClientKeySeed() ([]byte, error) {
	seed, err := readProviderClientKeySeed()
	if err != nil {
		return nil, err
	}
	if len(seed) == ed25519.SeedSize {
		return seed, nil
	}
	if 0 < len(seed) {
		return nil, fmt.Errorf(".provider.key has %d bytes, want %d; restore it from a backup", len(seed), ed25519.SeedSize)
	}
	seed = make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	path, err := providerStatePath(".provider.key")
	if err != nil {
		return nil, err
	}
	if err := durableWriteFile(path, seed, 0600); err != nil {
		return nil, err
	}
	return seed, nil
}

// durableWriteFile writes via temp file, fsync and rename, then fsyncs the
// directory, so a crash leaves either the old or the new complete file.
func durableWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// providerRegistrationStorePath also creates the state dir, since the store's
// lock file lives beside it and is opened before anything is written.
func providerRegistrationStorePath() (string, error) {
	path, err := providerStatePath(providerRegistrationStoreName)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	return path, nil
}

func readProviderRegistrations(path string) (map[string]*clientRegistrationRecord, error) {
	records := map[string]*clientRegistrationRecord{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		// Never treat an unreadable store as empty: that would start new
		// operations for slots whose requests may already have committed.
		return nil, fmt.Errorf("%s is unreadable (%v); restore it from a backup", providerRegistrationStoreName, err)
	}
	return records, nil
}

func writeProviderRegistrations(path string, records map[string]*clientRegistrationRecord) error {
	raw, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return durableWriteFile(path, raw, 0600)
}

// registrationRecordKey keys a retained operation by network and scope.
func registrationRecordKey(networkId string, scopeSha256 string) string {
	return networkId + "/" + scopeSha256
}

// providerRegistrationPost is the HTTP seam for tests.
var providerRegistrationPost = func(ctx context.Context, clientStrategy *connect.ClientStrategy, endpoint string, body []byte, byJwt string) ([]byte, error) {
	return connect.HttpPostWithStrategyRaw(ctx, clientStrategy, endpoint, body, byJwt)
}

// registerProviderClient returns the client JWT and id for one provider slot
// through the versioned route, replaying the slot's retained request when it
// has one. networkJwt is the network sign-in. description is used only when the
// operation is first created: a replay sends the retained bytes, and renewal
// keeps the dashboard description current afterwards.
func registerProviderClient(
	ctx context.Context,
	clientStrategy *connect.ClientStrategy,
	apiUrl string,
	networkJwt string,
	identityKey string,
	description string,
) (string, connect.Id, error) {
	principal, clientClaim, deviceClaim, err := registrationClaims(networkJwt)
	if err != nil {
		return "", connect.Id{}, err
	}
	if !validRegistrationId(principal.NetworkId) || !validRegistrationId(principal.UserId) || clientClaim != "" || deviceClaim != "" {
		return "", connect.Id{}, errors.New("the network sign-in does not name a network principal")
	}

	path, err := providerRegistrationStorePath()
	if err != nil {
		return "", connect.Id{}, err
	}
	// Every slot of a fresh install reaches here at once; under the locks
	// exactly one creates the seed and the rest read it.
	var generation uint64
	seed, err := func() ([]byte, error) {
		providerRegistrationLock.Lock()
		defer providerRegistrationLock.Unlock()
		release, err := acquireJWTStoreLock(path)
		if err != nil {
			return nil, err
		}
		defer release()
		generations, err := readProviderRegistrationGenerations()
		if err != nil {
			return nil, err
		}
		generation = generations[registrationGenerationKey(identityKey)]
		return ensureProviderClientKeySeed()
	}()
	if err != nil {
		return "", connect.Id{}, fmt.Errorf("provider client key: %w", err)
	}
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	scope := clientRegistrationScope{
		Endpoint:   networkClientRegistrationEndpoint(apiUrl),
		ClientKey:  "0x" + hex.EncodeToString(public),
		ClientRole: providerRegistrationRole,
		ClientSlot: providerRegistrationSlot(identityKey, generation),
	}
	scopeRaw, err := json.Marshal(scope)
	if err != nil {
		return "", connect.Id{}, err
	}
	scopeDigest := sha256.Sum256(scopeRaw)
	scopeSha256 := hex.EncodeToString(scopeDigest[:])
	key := registrationRecordKey(principal.NetworkId, scopeSha256)

	// Retain (or load) the operation under both locks, then send without
	// them: a slow reply must not hold every other slot's registration.
	record, err := func() (*clientRegistrationRecord, error) {
		providerRegistrationLock.Lock()
		defer providerRegistrationLock.Unlock()
		release, err := acquireJWTStoreLock(path)
		if err != nil {
			return nil, err
		}
		defer release()
		records, err := readProviderRegistrations(path)
		if err != nil {
			return nil, err
		}
		if record := records[key]; record != nil {
			if record.Schema != providerRegistrationRecordSchema || record.Scope != scope || record.Request.ScopeSha256 != scopeSha256 {
				return nil, errors.New("retained registration differs from its scope; restore .client_registrations.json from a backup")
			}
			if !record.Principal.equal(principal) {
				return nil, errors.New("the network sign-in differs from the retained registration's principal")
			}
			return record, nil
		}
		var requestId [32]byte
		if _, err := rand.Read(requestId[:]); err != nil {
			return nil, err
		}
		record := &clientRegistrationRecord{
			Schema:    providerRegistrationRecordSchema,
			Scope:     scope,
			Principal: principal,
			Request: registerNetworkClientArgs{
				Schema:            networkClientRegistrationSchema,
				RegistrationId:    hex.EncodeToString(requestId[:]),
				ScopeSha256:       scopeSha256,
				DeviceDescription: description,
			},
		}
		records[key] = record
		// durable before the first send
		if err := writeProviderRegistrations(path, records); err != nil {
			return nil, err
		}
		return record, nil
	}()
	if err != nil {
		return "", connect.Id{}, err
	}
	body, err := json.Marshal(record.Request)
	if err != nil {
		return "", connect.Id{}, err
	}
	if networkClientRegistrationMaxRequestBytes < len(body) {
		return "", connect.Id{}, errors.New("registration request exceeds the server route bound")
	}
	raw, err := providerRegistrationPost(ctx, clientStrategy, scope.Endpoint, body, networkJwt)
	if err != nil {
		var status *connect.HttpStatusError
		if errors.As(err, &status) {
			switch {
			case status.StatusCode == http.StatusUnauthorized:
				// the network sign-in was rejected; the caller decides
				// whether to re-type it (operator children only)
				return "", connect.Id{}, err
			case status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusMethodNotAllowed || status.StatusCode == http.StatusNotImplemented:
				return "", connect.Id{}, &ProviderRegistrationUnsupportedError{Status: status.StatusCode}
			}
		}
		if ctx.Err() != nil {
			return "", connect.Id{}, errors.Join(err, ctx.Err())
		}
		return "", connect.Id{}, &ProviderRegistrationUnavailableError{cause: err}
	}

	var result *registerNetworkClientResult
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return "", connect.Id{}, errors.New("registration reply is malformed")
	}
	bodyDigest := sha256.Sum256(body)
	if result.Schema != networkClientRegistrationSchema || result.RegistrationId != record.Request.RegistrationId || result.RequestSha256 != hex.EncodeToString(bodyDigest[:]) {
		return "", connect.Id{}, errors.New("registration reply differs from the retained request")
	}
	if result.Error != nil {
		if result.Error.Code == "" || result.ClientId != nil || result.DeviceId != nil || result.ByClientJwt != "" {
			return "", connect.Id{}, errors.New("registration reply mixed a refusal with a client identity")
		}
		return "", connect.Id{}, &ProviderRegistrationRefusedError{Code: result.Error.Code}
	}
	if result.ClientId == nil || result.DeviceId == nil || *result.ClientId == (connect.Id{}) || *result.DeviceId == (connect.Id{}) || result.ByClientJwt == "" {
		return "", connect.Id{}, errors.New("registration reply omitted its server-issued identity")
	}
	clientId, deviceId := result.ClientId.String(), result.DeviceId.String()
	tokenPrincipal, tokenClientId, tokenDeviceId, err := registrationClaims(result.ByClientJwt)
	if err != nil || tokenClientId != clientId || tokenDeviceId != deviceId || !tokenPrincipal.equal(principal) ||
		(record.ClientId != "" && (record.ClientId != clientId || record.DeviceId != deviceId)) {
		return "", connect.Id{}, errors.New("registered credential differs from its principal or retained client/device binding")
	}
	if record.ClientId == "" {
		if err := updateProviderRegistration(path, key, func(r *clientRegistrationRecord) {
			r.ClientId, r.DeviceId = clientId, deviceId
		}); err != nil {
			return "", connect.Id{}, err
		}
	}
	return result.ByClientJwt, *result.ClientId, nil
}

func updateProviderRegistration(path string, key string, update func(*clientRegistrationRecord)) error {
	providerRegistrationLock.Lock()
	defer providerRegistrationLock.Unlock()
	release, err := acquireJWTStoreLock(path)
	if err != nil {
		return err
	}
	defer release()
	records, err := readProviderRegistrations(path)
	if err != nil {
		return err
	}
	record := records[key]
	if record == nil {
		return errors.New("retained registration disappeared")
	}
	update(record)
	return writeProviderRegistrations(path, records)
}
