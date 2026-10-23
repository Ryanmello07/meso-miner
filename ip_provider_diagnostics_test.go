package connect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
)

// securityPolicyRulesPreimage is upstream's preimage for the rules pin: the
// default settings (MaxFlows scales with memory, so it is zeroed) and the
// hand-maintained exception tables. The feed-generated tables are left out.
func securityPolicyRulesPreimage(t *testing.T) []byte {
	t.Helper()
	dmca := DefaultDmcaSecurityPolicySettings()
	dmca.MaxFlows = 0
	settings, err := json.Marshal(struct {
		Cfaa *CfaaSecurityPolicySettings `json:"cfaa"`
		Dmca *DmcaSecurityPolicySettings `json:"dmca"`
		Web  *WebStandardSettings        `json:"web"`
	}{
		Cfaa: DefaultCfaaSecurityPolicySettings(),
		Dmca: dmca,
		Web:  DefaultWebStandardSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var preimage bytes.Buffer
	preimage.Write(settings)
	fmt.Fprintf(
		&preimage,
		"\x00steam %v\x00telegram %v %d",
		steamValveNetworkPrefixes,
		telegramCallReflectorIpv4Ranges,
		telegramCallV12TcpFallbackIpv4,
	)
	return preimage.Bytes()
}

// Upstream's pins (connect ip_provider_diagnostics_test.go). Meso reports a
// generation only when its rules hash to that generation's pin.
var securityPolicyRulesPins = map[uint64]string{
	1: "265961a0f2b445d75fd5a851ec49198f580e3778f4bbe27315af7f3290c7e8c9",
	2: "2798a1cef586738de34759ff274f084e950cbd09b9c17b35a3baeba57d80141f",
}

// A change to meso's default settings or a hand-maintained exception table
// breaks the claim that meso enforces upstream's generation. Either port the
// matching upstream generation or stop reporting one.
func TestSecurityPolicyRulesGenerationPin(t *testing.T) {
	digest := sha256.Sum256(securityPolicyRulesPreimage(t))
	got := hex.EncodeToString(digest[:])
	if pin := securityPolicyRulesPins[SecurityPolicyRulesGeneration]; got != pin {
		t.Fatalf("meso's built-in rules digest %s does not match upstream's pin %q for generation %d", got, pin, SecurityPolicyRulesGeneration)
	}
}

func TestSecurityPolicyGenerationByPolicy(t *testing.T) {
	if g := SecurityPolicyGeneration(DefaultEgressSecurityPolicy()); g != SecurityPolicyRulesGeneration {
		t.Fatalf("built-in exit policy generation %d", g)
	}
	if g := SecurityPolicyGeneration(DisableSecurityPolicy()); g != 0 {
		t.Fatalf("disabled policy generation %d, want unknown", g)
	}
	if g := SecurityPolicyGeneration(DefaultIngressSecurityPolicy()); g != 0 {
		t.Fatalf("ingress policy generation %d, want unknown", g)
	}
	if g := SecurityPolicyGeneration(&smtpSeamPolicy{}); g != 0 {
		t.Fatalf("custom policy generation %d, want unknown", g)
	}
}

// The feed-generated tables are identified by the hash, so a release that
// refreshed them reports a different hash.
func TestSecurityPolicyHashIdentifiesFeedTables(t *testing.T) {
	var identity bytes.Buffer
	writeSecurityPolicyIdentity(&identity, DefaultEgressSecurityPolicy())
	if !bytes.Contains(identity.Bytes(), []byte(cfaaBlockedPrefixData)) ||
		!bytes.Contains(identity.Bytes(), []byte(cfaaBlockedPrefix6Data)) {
		t.Fatal("identity does not cover the CFAA tables")
	}
	for _, prefix := range metaNetworkPrefixes {
		if !bytes.Contains(identity.Bytes(), []byte(prefix.String()+"\n")) {
			t.Fatalf("identity does not cover the Meta prefix %s", prefix)
		}
	}
	if SecurityPolicyHash(DefaultEgressSecurityPolicy()) != SecurityPolicyHash(DefaultEgressSecurityPolicy()) {
		t.Fatal("hash is not stable")
	}
	if SecurityPolicyHash(DefaultEgressSecurityPolicy()) == SecurityPolicyHash(DisableSecurityPolicy()) {
		t.Fatal("disabled policy shares the built-in hash")
	}
}

func TestProviderDiagnosticsPublishesIdentityOnceThenBlocks(t *testing.T) {
	provider := &RemoteUserNatProvider{
		securityPolicyHash:       "hash",
		securityPolicyGeneration: SecurityPolicyRulesGeneration,
	}
	SetBuildVersion("v-test")
	t.Cleanup(func() { SetBuildVersion("") })
	source := NewId()

	first := provider.providerDiagnosticsMessage(source)
	if first == nil || first.Sequence != 1 || first.GetSecurityPolicyGeneration() != SecurityPolicyRulesGeneration ||
		first.SecurityPolicyHash != "hash" || first.BuildVersion != "v-test" {
		t.Fatalf("identity message = %v", first)
	}
	// unpublished: offered again
	if again := provider.providerDiagnosticsMessage(source); again == nil || again.Sequence != 1 {
		t.Fatal("an unsent identity was not offered again")
	}
	provider.markProviderDiagnosticsPublished(source, first.Sequence)
	if provider.providerDiagnosticsMessage(source) != nil {
		t.Fatal("a published state was republished")
	}

	provider.recordProviderBlock(source, 2, 100)
	blocked := provider.providerDiagnosticsMessage(source)
	if blocked == nil || blocked.Sequence != 2 || blocked.BlockIngressPacketCount != 2 || blocked.BlockIngressByteCount != 100 {
		t.Fatalf("block message = %v", blocked)
	}
	provider.markProviderDiagnosticsPublished(source, blocked.Sequence)
	// a stale mark never rewinds publication
	provider.markProviderDiagnosticsPublished(source, 1)
	if provider.providerDiagnosticsMessage(source) != nil {
		t.Fatal("a stale mark rewound publication")
	}

	decoded := providerDiagnosticsFromProtocol(blocked)
	if decoded.SecurityPolicyGeneration != SecurityPolicyRulesGeneration || decoded.BlockIngressPacketCount != 2 {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestProviderDiagnosticsUnknownGenerationStaysAbsent(t *testing.T) {
	provider := &RemoteUserNatProvider{}
	message := provider.providerDiagnosticsMessage(NewId())
	if message == nil || message.SecurityPolicyGeneration != nil {
		t.Fatalf("unknown generation was sent: %v", message)
	}
}

func TestProviderDiagnosticsSourcesAreBounded(t *testing.T) {
	provider := &RemoteUserNatProvider{}
	for i := 0; i < providerDiagnosticsMaxSources+100; i++ {
		provider.recordProviderBlock(NewId(), 1, 1)
	}
	if n := len(provider.sourceDiagnostics); n > providerDiagnosticsMaxSources {
		t.Fatalf("%d source records, bound %d", n, providerDiagnosticsMaxSources)
	}
}

// A packet the exit policy drops is counted against its source.
func TestProviderDiagnosticsCountsPolicyDrops(t *testing.T) {
	policy := &smtpSeamPolicy{stats: DefaultSecurityPolicyStatsCollector(), result: SecurityPolicyResultDrop}
	provider := newSmtpSeamProvider(t, policy)
	source := SourceId(NewId())
	packet := smtpSeamTcp4Packet(40000, 443, byte(tcpFlagSyn), 0, 0, nil)
	sendSmtpSeamProviderPacket(t, provider, source, packet)

	provider.diagnosticsLock.Lock()
	state := provider.sourceDiagnostics[source.SourceId]
	provider.diagnosticsLock.Unlock()
	if state == nil || state.blockIngressPacketCount != 1 || state.blockIngressByteCount != uint64(len(packet)) {
		t.Fatalf("drop not counted: %+v", state)
	}
	if calls := policy.calls.Load(); calls != 1 {
		t.Fatalf("policy calls %d", calls)
	}
}

// An admitted packet is not a block; the source still gets an identity record.
func TestProviderDiagnosticsAllowIsNotABlock(t *testing.T) {
	policy := &smtpSeamPolicy{stats: DefaultSecurityPolicyStatsCollector(), result: SecurityPolicyResultAllow}
	provider := newSmtpSeamProvider(t, policy)
	source := SourceId(NewId())
	sendSmtpSeamProviderPacket(t, provider, source, smtpSeamTcp4Packet(40000, 443, byte(tcpFlagSyn), 0, 0, nil))

	provider.diagnosticsLock.Lock()
	defer provider.diagnosticsLock.Unlock()
	state := provider.sourceDiagnostics[source.SourceId]
	if state == nil || state.blockIngressPacketCount != 0 {
		t.Fatalf("allowed packet state: %+v", state)
	}
}
