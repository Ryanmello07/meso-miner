package connect

import (
	"strings"
)

// net_http_doh_regional.go -- the regional dns resolver recommendations.
//
// Ported from connect v0.0.0-20261017075543-c8d8cdf536a8
// (net_http_doh_regional.go), adapted to this tree:
//   - The remote plain-dns lists carry each operator's v6 address beside its
//     v4 one, so a regional recommendation is walked by operator across both
//     families (knownDnsServerPairs, dualStackList) instead of being v4-only.
//     A region that publishes no v6 resolver keeps a v4-only recommendation,
//     and the resolver then walks the v4 list.
//   - This tree's RegionalDnsResolverSettings() also sets EnableLocalDns and
//     the 1.1.1.1 local list. That shape is kept (it is public API here); the
//     v6 remote list is added on top of it.
//   - The bootstrap control-DoH presets (RegionalControlDohServer and
//     RegionalControlDohUrls) are NOT ported: this tree has no control-DoH
//     path for them to feed.

// a well known regional dns server, associated to a country code. Ipv6 is
// empty where the operator publishes no v6 resolver (or none is known).
type RegionalDnsServer struct {
	CountryCode string
	Name        string
	Ipv4        string
	Ipv6        string
}

// the well known regional dns servers, associated to country codes.
// these are suggestions that clients can surface when connected to the region
var regionalDnsServers = []*RegionalDnsServer{
	{CountryCode: "cn", Name: "Alidns", Ipv4: "223.5.5.5", Ipv6: "2400:3200::1"},
	{CountryCode: "cn", Name: "DNSPod/Tencent", Ipv4: "119.29.29.29", Ipv6: "2402:4e00::"},
	{CountryCode: "ru", Name: "Yandex.DNS", Ipv4: "77.88.8.8", Ipv6: "2a02:6b8::feed:0ff"},
	{CountryCode: "ru", Name: "National DNS (NSDI)", Ipv4: "195.208.6.1"},
	{CountryCode: "ir", Name: "Shecan", Ipv4: "178.22.122.100"},
	{CountryCode: "ir", Name: "403.online", Ipv4: "10.202.10.10"},
	{CountryCode: "tr", Name: "TTNET", Ipv4: "195.175.39.39"},
	{CountryCode: "tr", Name: "Turkcell Superonline", Ipv4: "212.252.114.8"},
	{CountryCode: "kz", Name: "Google Public DNS", Ipv4: "8.8.8.8", Ipv6: "2001:4860:4860::8888"},
	{CountryCode: "tm", Name: "Turkmentelecom", Ipv4: "217.174.238.141"},
	{CountryCode: "tm", Name: "Google Public DNS", Ipv4: "8.8.4.4", Ipv6: "2001:4860:4860::8844"},
}

// RegionalDnsServers enumerates the well known regional dns servers
func RegionalDnsServers() []*RegionalDnsServer {
	return regionalDnsServers
}

// RegionalDnsServerIps returns the recommended dns server ipv4 addresses for a
// country, or nil when there is no recommendation
func RegionalDnsServerIps(countryCode string) []string {
	countryCode = strings.ToLower(countryCode)
	var ips []string
	for _, server := range regionalDnsServers {
		if server.CountryCode == countryCode {
			ips = append(ips, server.Ipv4)
		}
	}
	return ips
}

// RegionalDnsServerIpv6s returns the recommended dns server ipv6 addresses for
// a country, or nil when none of its servers publishes one. A region can have
// a v4 recommendation and no v6 one; the resolver then walks the v4 list.
func RegionalDnsServerIpv6s(countryCode string) []string {
	countryCode = strings.ToLower(countryCode)
	var ips []string
	for _, server := range regionalDnsServers {
		if server.CountryCode == countryCode && server.Ipv6 != "" {
			ips = append(ips, server.Ipv6)
		}
	}
	return ips
}

// regional dns resolver settings tune the dns resolver for regions
// if the result is nil, there is no regional specific setting and a universal resolver should be used
func RegionalDnsResolverSettings(countryCode string) *DnsResolverSettings {
	remoteDnsIpv4 := RegionalDnsServerIps(countryCode)
	if len(remoteDnsIpv4) == 0 {
		return nil
	}
	return &DnsResolverSettings{
		EnableRemoteDns: true,
		EnableLocalDns:  true,
		RemoteDnsIpv4:   remoteDnsIpv4,
		RemoteDnsIpv6:   RegionalDnsServerIpv6s(countryCode),
		LocalDnsIpv4: []string{
			"1.1.1.1",
		},
	}
}
