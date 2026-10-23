package connect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"sync/atomic"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Provider diagnostics, ported from urnetwork/connect ip_provider_diagnostics.go
// (main, 2026-10-22).
//
// An exit reports its build, a digest of its security policy and the policy's
// rules generation to each client it serves, then the cumulative count of
// that client's packets its policy dropped whenever the count rises. Upstream
// clients use it: after a provider with an older or unknown generation drops
// their traffic, they demote every provider that is not current for a while.
// A provider that reports nothing counts as unknown, so before this port every
// meso exit was demoted whenever that preference was armed.
//
// The generation is upstream's ordering of the built-in rules. Meso's default
// settings and hand-maintained exception tables hash to upstream's pin for
// generation 2 (TestSecurityPolicyRulesGenerationPin). Meso's detectors are
// that generation plus one stricter check (BitTorrent markers at any payload
// offset), so it never admits what a generation 2 client's rules drop.

// Upstream's built-in rules generation that meso's built-in policy enforces.
// Raise it only together with upstream, after porting that generation's rule
// changes, and pin the new digest in ip_provider_diagnostics_test.go.
const SecurityPolicyRulesGeneration uint64 = 2

// The rules generation a policy enforces: SecurityPolicyRulesGeneration for
// the built-in exit policy, and 0 (unknown) for any other policy.
func SecurityPolicyGeneration(policy SecurityPolicy) uint64 {
	switch policy.(type) {
	case *egressSecurityPolicy:
		// the exit's policy; meso's ingress policy (the client's return
		// direction, CFAA only) is not upstream's built-in policy
		return SecurityPolicyRulesGeneration
	default:
		return 0
	}
}

// SecurityPolicyIdentity lets a custom provider policy publish an identity
// for its effective rules. The value should change whenever enforcement
// changes and should not contain secrets.
type SecurityPolicyIdentity interface {
	SecurityPolicyHash() string
}

// SecurityPolicyHash returns a stable digest for the effective policy: every
// built-in setting and the feed-generated tables, or for an opaque policy its
// type and build.
func SecurityPolicyHash(policy SecurityPolicy) string {
	if identity, ok := policy.(SecurityPolicyIdentity); ok {
		if value := identity.SecurityPolicyHash(); value != "" {
			return value
		}
	}
	digest := sha256.New()
	writeSecurityPolicyIdentity(digest, policy)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeSecurityPolicyIdentity(digest io.Writer, policy SecurityPolicy) {
	io.WriteString(digest, "urnetwork-security-policy-v1\x00")
	writeBuiltin := func(direction string, cfaa *cfaaDetector, dmca *dmcaDetector) {
		io.WriteString(digest, "builtin-meso-"+direction+"\x00")
		configuration := struct {
			Cfaa *CfaaSecurityPolicySettings `json:"cfaa"`
			Dmca *DmcaSecurityPolicySettings `json:"dmca,omitempty"`
			Web  *WebStandardSettings        `json:"web,omitempty"`
		}{
			Cfaa: cfaa.settings,
		}
		if dmca != nil {
			configuration.Dmca = dmca.settings
			configuration.Web = dmca.web.settings
		}
		encoded, err := json.Marshal(configuration)
		if err != nil {
			panic(err)
		}
		digest.Write(encoded)
		io.WriteString(digest, "\x00cfaa4\x00")
		io.WriteString(digest, cfaaBlockedPrefixData)
		io.WriteString(digest, "\x00cfaa6\x00")
		io.WriteString(digest, cfaaBlockedPrefix6Data)
		io.WriteString(digest, "\x00meta\x00")
		for _, prefix := range metaNetworkPrefixes {
			io.WriteString(digest, prefix.String())
			io.WriteString(digest, "\n")
		}
	}
	switch concrete := policy.(type) {
	case *egressSecurityPolicy:
		writeBuiltin("egress", concrete.cfaa, concrete.dmca)
	case *ingressSecurityPolicy:
		writeBuiltin("ingress", concrete.cfaa, nil)
	case *disableSecurityPolicy:
		io.WriteString(digest, "disabled\x00")
	default:
		io.WriteString(digest, "opaque\x00")
		io.WriteString(digest, reflect.TypeOf(policy).String())
		io.WriteString(digest, "\x00")
		io.WriteString(digest, BuildVersion())
	}
}

// buildVersion is the host's build identifier, reported in diagnostics. The
// provider sets it at startup; unset, diagnostics carry an empty build.
var buildVersion atomic.Pointer[string]

// SetBuildVersion records the host's build identifier.
func SetBuildVersion(version string) {
	if version == "" {
		buildVersion.Store(nil)
		return
	}
	buildVersion.Store(&version)
}

// BuildVersion reports the host's build identifier, or "" when unset.
func BuildVersion() string {
	if version := buildVersion.Load(); version != nil {
		return *version
	}
	return ""
}

// ProviderDiagnostics is the latest identity and source-scoped security
// enforcement telemetry published by one provider exit.
type ProviderDiagnostics struct {
	BuildVersion       string
	SecurityPolicyHash string
	// 0 is unknown: the provider predates the field or runs a policy that is
	// not the built-in one
	SecurityPolicyGeneration uint64
	// The provider counts the remote client's outbound packets as its
	// ingress, so BlockIngress* are this client's packets the provider policy
	// dropped on the way out, and BlockEgress* are return packets dropped on
	// the way back (meso does not drop return packets, so these stay zero).
	BlockIngressPacketCount int64
	BlockIngressByteCount   int64
	BlockEgressPacketCount  int64
	BlockEgressByteCount    int64
	Sequence                int64
}

// providerDiagnosticsMaxSources bounds the per-source records one provider
// keeps. Records are advisory; evicting one only re-publishes identity to
// that source later.
const providerDiagnosticsMaxSources = 4096

type providerSourceDiagnostics struct {
	blockIngressPacketCount uint64
	blockIngressByteCount   uint64
	sequence                uint64
	publishedSequence       uint64
}

func (self *RemoteUserNatProvider) ensureSourceDiagnosticsWithLock(sourceId Id) *providerSourceDiagnostics {
	if self.sourceDiagnostics == nil {
		self.sourceDiagnostics = map[Id]*providerSourceDiagnostics{}
	}
	state := self.sourceDiagnostics[sourceId]
	if state == nil {
		if len(self.sourceDiagnostics) >= providerDiagnosticsMaxSources {
			for id := range self.sourceDiagnostics {
				delete(self.sourceDiagnostics, id)
				break
			}
		}
		// Sequence 1 publishes provider identity before any block occurs.
		state = &providerSourceDiagnostics{sequence: 1}
		self.sourceDiagnostics[sourceId] = state
	}
	return state
}

// recordProviderBlock counts packets of the source's outbound traffic that the
// provider policy dropped.
func (self *RemoteUserNatProvider) recordProviderBlock(sourceId Id, packetCount int, byteCount int) {
	if packetCount <= 0 || byteCount < 0 {
		return
	}
	self.diagnosticsLock.Lock()
	defer self.diagnosticsLock.Unlock()
	state := self.ensureSourceDiagnosticsWithLock(sourceId)
	state.blockIngressPacketCount += uint64(packetCount)
	state.blockIngressByteCount += uint64(byteCount)
	state.sequence++
}

func (self *RemoteUserNatProvider) providerDiagnosticsMessage(sourceId Id) *protocol.IpProviderDiagnostics {
	self.diagnosticsLock.Lock()
	defer self.diagnosticsLock.Unlock()
	state := self.ensureSourceDiagnosticsWithLock(sourceId)
	if state.publishedSequence == state.sequence {
		return nil
	}
	message := &protocol.IpProviderDiagnostics{
		BuildVersion:            BuildVersion(),
		SecurityPolicyHash:      self.securityPolicyHash,
		BlockIngressPacketCount: state.blockIngressPacketCount,
		BlockIngressByteCount:   state.blockIngressByteCount,
		Sequence:                state.sequence,
	}
	// an unknown generation stays absent, as from a provider that predates it
	if self.securityPolicyGeneration != 0 {
		message.SecurityPolicyGeneration = proto.Uint64(self.securityPolicyGeneration)
	}
	return message
}

func (self *RemoteUserNatProvider) markProviderDiagnosticsPublished(sourceId Id, sequence uint64) {
	self.diagnosticsLock.Lock()
	defer self.diagnosticsLock.Unlock()
	if state := self.sourceDiagnostics[sourceId]; state != nil && state.publishedSequence < sequence {
		state.publishedSequence = sequence
	}
}

// publishProviderDiagnostics is nonblocking. Identity publishes once per
// source; counters republish only after a block changes them. It rides the
// provider's ordinary return path: the reversed source path, stream id kept,
// over a companion contract, like the packet returns in Receive.
//
// Records are keyed by source client id and bounded; past the bound an
// evicted source is offered identity again from sequence 1 (upstream bounds
// its records the same way).
func (self *RemoteUserNatProvider) publishProviderDiagnostics(source TransferPath) {
	message := self.providerDiagnosticsMessage(source.SourceId)
	if message == nil {
		return
	}
	// Rare lifecycle telemetry, not packet data: an ordinary GC-owned buffer.
	messageBytes, err := proto.Marshal(message)
	if err != nil {
		return
	}
	frame := &protocol.Frame{
		MessageType:  protocol.MessageType_IpIpProviderDiagnostics,
		MessageBytes: messageBytes,
	}
	if self.client.SendWithTimeout(
		frame,
		source.Reverse(),
		func(error) {},
		0,
		CompanionContract(),
	) {
		self.markProviderDiagnosticsPublished(source.SourceId, message.Sequence)
	}
}

func providerDiagnosticsFromProtocol(message *protocol.IpProviderDiagnostics) *ProviderDiagnostics {
	if message == nil {
		return nil
	}
	return &ProviderDiagnostics{
		BuildVersion:             message.BuildVersion,
		SecurityPolicyHash:       message.SecurityPolicyHash,
		SecurityPolicyGeneration: message.GetSecurityPolicyGeneration(),
		BlockIngressPacketCount:  int64(message.BlockIngressPacketCount),
		BlockIngressByteCount:    int64(message.BlockIngressByteCount),
		BlockEgressPacketCount:   int64(message.BlockEgressPacketCount),
		BlockEgressByteCount:     int64(message.BlockEgressByteCount),
		Sequence:                 int64(message.Sequence),
	}
}
