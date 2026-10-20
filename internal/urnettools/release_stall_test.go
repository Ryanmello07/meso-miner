package urnettools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// These tests pin the release SELECTION contract behind `urnet-tools update`
// (and reinstall / self-update / the upgrade notice), which resolves the
// newest release from the fork's GitHub releases API:
//
//   - a newer release without the expected urnetwork-provider-<tag>.tar.gz
//     asset (a docs-only or tool-only release) must not stall the updater on
//     that tag: the updater pages past it to the newest release that has a
//     verified provider tarball.
//   - a pre-release must never be selected as the stable channel, whether
//     GitHub flags it (prerelease: true) or only the tag names it (-rcN,
//     -betaN, ...) because someone published it with the flag unset.
//   - a normal stable newest release with the provider asset resolves exactly
//     as before (same tag, digest and download URL).
//
// Everything runs against a local fake releases API (no network).

// fakeAsset is one release asset in a fake API response.
type fakeAsset struct {
	Name   string
	Digest string
}

// fakeRelease is one release in a fake API response.
type fakeRelease struct {
	Tag        string
	Draft      bool
	Prerelease bool
	Assets     []fakeAsset
}

func (r fakeRelease) json() map[string]any {
	assets := make([]map[string]any, 0, len(r.Assets))
	for _, a := range r.Assets {
		assets = append(assets, map[string]any{"name": a.Name, "digest": a.Digest})
	}
	return map[string]any{
		"tag_name":   r.Tag,
		"draft":      r.Draft,
		"prerelease": r.Prerelease,
		"assets":     assets,
	}
}

// fakeReleasesAPI serves /releases (paged) and /releases/latest with GitHub's
// documented semantics: /latest is the most recent non-draft, non-prerelease
// release, regardless of whether its assets are usable.
type fakeReleasesAPI struct {
	releases []fakeRelease

	mu       sync.Mutex
	requests []string
}

func (f *fakeReleasesAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RequestURI())
	f.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/releases/latest"):
		for _, rel := range f.releases {
			if rel.Draft || rel.Prerelease {
				continue
			}
			writeJSON(w, rel.json())
			return
		}
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"message": "Not Found"})
	case strings.HasSuffix(r.URL.Path, "/releases"):
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if perPage <= 0 {
			perPage = 30 // GitHub's default page size
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page <= 0 {
			page = 1
		}
		start := (page - 1) * perPage
		if start > len(f.releases) {
			start = len(f.releases)
		}
		end := start + perPage
		if end > len(f.releases) {
			end = len(f.releases)
		}
		pageReleases := make([]map[string]any, 0, end-start)
		for _, rel := range f.releases[start:end] {
			pageReleases = append(pageReleases, rel.json())
		}
		writeJSON(w, pageReleases)
	default:
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"message": "Not Found"})
	}
}

func (f *fakeReleasesAPI) sawRequest(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if strings.Contains(req, substr) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newFakeReleasesServer starts the fake API and points repoAPIBase at it for
// the duration of the test.
func newFakeReleasesServer(t *testing.T, releases []fakeRelease) *fakeReleasesAPI {
	t.Helper()
	f := &fakeReleasesAPI{releases: releases}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	oldBase := repoAPIBase
	repoAPIBase = srv.URL + "/repos/urfoundation/meso-miner"
	t.Cleanup(func() { repoAPIBase = oldBase })
	return f
}

func providerAsset(tag string) fakeAsset {
	return fakeAsset{
		Name:   "urnetwork-provider-" + tag + ".tar.gz",
		Digest: "sha256:" + strings.Repeat("a", 64),
	}
}

func toolAsset() fakeAsset {
	return fakeAsset{
		Name:   "urnet-tools-linux-amd64",
		Digest: "sha256:" + strings.Repeat("b", 64),
	}
}

// TestFetchLatestReleaseSkipsAssetlessNewestRelease is the reproduction of
// the reported stall: the newest release carries no provider tarball (here a
// docs/tool-only release), so resolving the stable channel must page past it
// to the newest release that does carry one.
func TestFetchLatestReleaseSkipsAssetlessNewestRelease(t *testing.T) {
	stable := fakeRelease{
		Tag:    "v3.23.0-fix.32.10",
		Assets: []fakeAsset{providerAsset("v3.23.0-fix.32.10"), toolAsset()},
	}
	notesOnly := fakeRelease{
		Tag:    "v3.23.0-fix.33.1-notes",
		Assets: []fakeAsset{toolAsset()},
	}
	api := newFakeReleasesServer(t, []fakeRelease{notesOnly, stable})

	info, err := fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease stalled on %s (no provider asset) instead of falling back to %s: %v",
			notesOnly.Tag, stable.Tag, err)
	}
	if info.Tag != stable.Tag {
		t.Fatalf("fetchLatestRelease selected %q, want %q (skip the assetless release)", info.Tag, stable.Tag)
	}
	wantDigest := strings.Repeat("a", 64)
	if info.ProviderDigest != wantDigest {
		t.Errorf("ProviderDigest = %q, want %q", info.ProviderDigest, wantDigest)
	}
	wantURL := fmt.Sprintf("/download/%s/urnetwork-provider-%s.tar.gz", stable.Tag, stable.Tag)
	if !strings.HasSuffix(info.URL, wantURL) {
		t.Errorf("URL = %q, want suffix %q", info.URL, wantURL)
	}
	// The fallback must come from the paged list, not from /releases/latest.
	if !api.sawRequest("/releases?") {
		t.Errorf("expected the updater to list releases, requests: %v", api.requests)
	}
}

// TestFetchLatestReleaseNeverSelectsAPreRelease covers an rc published WITHOUT
// the GitHub prerelease flag: /releases/latest returns it, so the tag name is
// the only signal left. It must never become the stable channel.
func TestFetchLatestReleaseNeverSelectsAPreRelease(t *testing.T) {
	stable := fakeRelease{
		Tag:    "v3.23.0-fix.32.10",
		Assets: []fakeAsset{providerAsset("v3.23.0-fix.32.10")},
	}
	rc := fakeRelease{
		Tag:        "v3.23.0-fix.33.0-rc5",
		Prerelease: false, // published through the UI with the flag unset
		Assets:     []fakeAsset{providerAsset("v3.23.0-fix.33.0-rc5")},
	}
	api := newFakeReleasesServer(t, []fakeRelease{rc, stable})

	info, err := fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if info.Tag == rc.Tag {
		t.Fatalf("fetchLatestRelease selected pre-release %q as the stable channel", info.Tag)
	}
	if info.Tag != stable.Tag {
		t.Fatalf("fetchLatestRelease selected %q, want %q", info.Tag, stable.Tag)
	}
	if !api.sawRequest("/releases?") {
		t.Errorf("expected the updater to list releases, requests: %v", api.requests)
	}
}

// TestFetchLatestReleaseSkipsFlaggedPrerelease keeps the GitHub-flag path
// covered: a release marked prerelease:true is not a stable candidate.
func TestFetchLatestReleaseSkipsFlaggedPrerelease(t *testing.T) {
	stable := fakeRelease{
		Tag:    "v3.23.0-fix.32.10",
		Assets: []fakeAsset{providerAsset("v3.23.0-fix.32.10")},
	}
	flagged := fakeRelease{
		Tag:        "v3.23.0-fix.33.0-rc6",
		Prerelease: true,
		Assets:     []fakeAsset{providerAsset("v3.23.0-fix.33.0-rc6")},
	}
	newFakeReleasesServer(t, []fakeRelease{flagged, stable})

	info, err := fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if info.Tag != stable.Tag {
		t.Fatalf("fetchLatestRelease selected %q, want %q", info.Tag, stable.Tag)
	}
}

// TestFetchLatestReleaseUnchangedForNewestStableWithAsset pins the normal
// case: when the newest stable release carries the provider tarball, the
// resolved tag, digest and URL are exactly what the old single-release path
// returned.
func TestFetchLatestReleaseUnchangedForNewestStableWithAsset(t *testing.T) {
	newest := fakeRelease{
		Tag:    "v3.23.0-fix.32.11",
		Assets: []fakeAsset{providerAsset("v3.23.0-fix.32.11")},
	}
	older := fakeRelease{
		Tag:    "v3.23.0-fix.32.10",
		Assets: []fakeAsset{providerAsset("v3.23.0-fix.32.10")},
	}
	newFakeReleasesServer(t, []fakeRelease{newest, older})

	info, err := fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if info.Tag != newest.Tag {
		t.Fatalf("fetchLatestRelease selected %q, want %q", info.Tag, newest.Tag)
	}
	if info.ProviderDigest != strings.Repeat("a", 64) {
		t.Errorf("ProviderDigest = %q, want the newest release's digest", info.ProviderDigest)
	}
	wantURL := fmt.Sprintf("https://github.com/urfoundation/meso-miner/releases/download/%s/urnetwork-provider-%s.tar.gz", newest.Tag, newest.Tag)
	if info.URL != wantURL {
		t.Errorf("URL = %q, want %q", info.URL, wantURL)
	}
}

// TestFetchLatestReleasePaginatesPastAssetlessReleases forces the eligible
// release onto the second API page: a full first page of stable releases
// without provider assets must not end the scan.
func TestFetchLatestReleasePaginatesPastAssetlessReleases(t *testing.T) {
	const pageSize = 100
	junk := make([]fakeRelease, 0, pageSize)
	for i := 0; i < pageSize; i++ {
		tag := fmt.Sprintf("v3.23.0-fix.40.%d", i)
		junk = append(junk, fakeRelease{Tag: tag, Assets: []fakeAsset{toolAsset()}})
	}
	good := fakeRelease{
		Tag:    "v3.23.0-fix.32.10",
		Assets: []fakeAsset{providerAsset("v3.23.0-fix.32.10")},
	}
	releases := append(junk, good)
	api := newFakeReleasesServer(t, releases)

	info, err := fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease did not reach page 2: %v", err)
	}
	if info.Tag != good.Tag {
		t.Fatalf("fetchLatestRelease selected %q, want %q", info.Tag, good.Tag)
	}
	if !api.sawRequest("/releases?per_page=100&page=2") {
		t.Errorf("expected a request for release page 2, requests: %v", api.requests)
	}
}

// TestFetchLatestReleaseSkipsProviderAssetWithoutDigest: a release that ships
// the tarball but publishes no sha256 digest cannot be installed safely, so
// it is skipped for an older release that does publish one.
func TestFetchLatestReleaseSkipsProviderAssetWithoutDigest(t *testing.T) {
	noDigest := fakeRelease{
		Tag: "v3.23.0-fix.32.11",
		Assets: []fakeAsset{{
			Name:   "urnetwork-provider-v3.23.0-fix.32.11.tar.gz",
			Digest: "",
		}},
	}
	stable := fakeRelease{
		Tag:    "v3.23.0-fix.32.10",
		Assets: []fakeAsset{providerAsset("v3.23.0-fix.32.10")},
	}
	newFakeReleasesServer(t, []fakeRelease{noDigest, stable})

	info, err := fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if info.Tag != stable.Tag {
		t.Fatalf("fetchLatestRelease selected %q, want %q (skip the digestless release)", info.Tag, stable.Tag)
	}
}

// TestFetchLatestReleaseNoEligibleRelease: when nothing in the listing is a
// usable stable release the updater still fails closed with a clear reason.
func TestFetchLatestReleaseNoEligibleRelease(t *testing.T) {
	newFakeReleasesServer(t, []fakeRelease{
		{Tag: "v3.23.0-fix.33.1-notes", Assets: []fakeAsset{toolAsset()}},
		{Tag: "v3.23.0-fix.33.0-rc5", Assets: []fakeAsset{providerAsset("v3.23.0-fix.33.0-rc5")}},
	})

	_, err := fetchLatestRelease()
	if err == nil {
		t.Fatal("fetchLatestRelease must fail closed when no stable release has a provider tarball")
	}
	for _, want := range []string{"no stable release", "provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestFetchReleaseByTagStillAllowsExplicitPreRelease: an operator who names a
// tag explicitly (canary/rollback) keeps the explicit override; only
// automatic stable selection refuses pre-releases.
func TestFetchReleaseByTagStillAllowsExplicitPreRelease(t *testing.T) {
	const tag = "v3.23.0-fix.33.0-rc5"
	f := &fakeReleasesAPI{releases: []fakeRelease{{
		Tag:    tag,
		Assets: []fakeAsset{providerAsset(tag)},
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/tags/"+tag) {
			writeJSON(w, f.releases[0].json())
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	oldBase := repoAPIBase
	repoAPIBase = srv.URL
	t.Cleanup(func() { repoAPIBase = oldBase })

	info, err := fetchReleaseByTag(tag)
	if err != nil {
		t.Fatalf("fetchReleaseByTag(%s) = %v, want an explicit tag to resolve", tag, err)
	}
	if info.Tag != tag {
		t.Fatalf("fetchReleaseByTag selected %q, want %q", info.Tag, tag)
	}
}

// TestIsPreReleaseTag pins the tag-name predicate against the shapes this
// repo actually publishes.
func TestIsPreReleaseTag(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"v3.23.0-fix.32.10", false},
		{"v3.23.0-fix.33.0", false},
		{"v3.23.0-fix.33.0+build7", false},
		{"v3.23.0-fix.33.0-rc1", true},
		{"v3.23.0-fix.33.0-rc.5", true},
		{"v3.23.0-fix.33.0-RC5", true},
		{"v3.23.0-fix.33.0-beta2", true},
		{"v3.23.0-fix.33.0-alpha", true},
		{"v3.23.0-fix.33.0-preview3", true},
		{"v3.23.0-fix.33.0-nightly", true},
		{"v3.23.0-fix.33.0-dev", true},
		{"v3.23.0-fix.33.0-snapshot", true},
		// A stable tag that merely CONTAINS a pre-release word must not match.
		{"v3.23.0-fix.device.4", false},
		{"v3.23.0-fix.preflight.2", false},
	}
	for _, tc := range cases {
		if got := isPreReleaseTag(tc.tag); got != tc.want {
			t.Errorf("isPreReleaseTag(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}
