package urnettools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// validateTag checks that a release tag contains only safe characters.
// Prevents path-traversal attacks via crafted --tag values like
// "../../other-org/other-repo/releases/latest".
func validateTag(tag string) error {
	if !regexp.MustCompile(`^[A-Za-z0-9._+-]+$`).MatchString(tag) {
		return fmt.Errorf("invalid tag %q: only alphanumerics, dots, underscores, hyphens, and plus signs allowed", tag)
	}
	return nil
}

// releaseInfo describes the latest release fetched from GitHub.
type releaseInfo struct {
	Tag string
	// ProviderDigest is the sha256 of the urnetwork-provider-<tag>.tar.gz
	// asset (hex). Named explicitly: the tool's OWN asset digest is separate
	// and resolved via digestForAsset(Assets, ...), never from this field.
	ProviderDigest string
	URL            string
	// Assets is the full asset list of the release. Tool self-update uses it
	// to look up the digest of the tool's OWN asset (urnet-tools-<os>-<arch>
	// or urnet-docker-<os>-<arch>), which is separate from the provider
	// tarball digest.
	Assets []releaseAsset
}

// releaseAsset is the subset of the GitHub release asset JSON we need.
type releaseAsset struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// releaseJSON is the subset of the GitHub release JSON we need.
type releaseJSON struct {
	TagName    string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

// repoAPIBase is the GitHub repository API root for this fork. It is a
// variable so tests can point every release lookup at a local fake server;
// production code never changes it.
var repoAPIBase = "https://api.github.com/repos/full-bars/urnetwork-3.23-fix"

const (
	// releasePageSize is the page size used when listing releases. 100 is
	// GitHub's maximum, so one page covers a normal release history.
	releasePageSize = 100
	// maxReleasePages bounds the listing walk. It only matters when the
	// repo has no eligible stable release at all (an error path); a normal
	// update resolves on page 1 in a single request.
	maxReleasePages = 10
)

// preReleaseTagRE matches a pre-release channel marker as a tag component:
// "-rc1", ".beta2", "-nightly", "_preview3". Matching is on component
// boundaries so a stable tag that merely contains the letters (for example
// "v3.23.0-fix.device.4") is not mistaken for a pre-release.
var preReleaseTagRE = regexp.MustCompile(`(?i)(^|[-._])(rc|alpha|beta|pre|preview|nightly|snapshot|dev)[0-9]*($|[-._])`)

// isPreReleaseTag reports whether a release tag names a pre-release channel.
// GitHub's release API already flags rc/beta releases created by the release
// workflow, but a release published by hand can carry an rc tag with the
// prerelease flag unset; the tag is then the only signal left, and an
// auto-updater must not treat it as stable.
func isPreReleaseTag(tag string) bool {
	return preReleaseTagRE.MatchString(tag)
}

// providerTarballName is the release asset name of the multi-arch provider
// tarball for a tag.
func providerTarballName(tag string) string {
	return "urnetwork-provider-" + tag + ".tar.gz"
}

// providerTarballURL is the GitHub download URL of that tarball.
func providerTarballURL(tag string) string {
	return fmt.Sprintf("https://github.com/full-bars/urnetwork-3.23-fix/releases/download/%s/%s",
		url.PathEscape(tag), providerTarballName(tag))
}

// githubAuthHeader returns an Authorization header value when GITHUB_TOKEN
// is set; otherwise ("", ""). Used to raise the unauthenticated rate limit
// from 60 to 5000 requests/hour so auto-update timers on a fleet don't hit 403.
func githubAuthHeader() (string, string) {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		return "Authorization", "Bearer " + tok
	}
	return "", ""
}

// releaseFromJSON builds the update target for one release object.
func releaseFromJSON(rj releaseJSON) *releaseInfo {
	return &releaseInfo{
		Tag:            rj.TagName,
		ProviderDigest: digestForAsset(rj.Assets, providerTarballName(rj.TagName)),
		URL:            providerTarballURL(rj.TagName),
		Assets:         rj.Assets,
	}
}

// skipReason explains why a release cannot serve the stable channel, or ""
// when it is a valid stable candidate.
func skipReason(rj releaseJSON) string {
	switch {
	case rj.TagName == "":
		return "missing tag_name"
	case rj.Draft:
		return "draft"
	case rj.Prerelease:
		return "marked pre-release"
	case isPreReleaseTag(rj.TagName):
		return "pre-release tag"
	case digestForAsset(rj.Assets, providerTarballName(rj.TagName)) == "":
		return "no " + providerTarballName(rj.TagName) + " with a sha256 digest"
	}
	return ""
}

// fetchLatestRelease resolves the stable update target: the newest release
// that is neither a draft nor a pre-release and that publishes the
// urnetwork-provider-<tag>.tar.gz asset with a sha256 digest.
//
// It walks the releases listing instead of trusting releases/latest. GitHub
// defines "latest" as the newest non-draft, non-prerelease release whether or
// not its assets are usable, so a docs-only or tool-only release published
// after a good one used to make every updater fail on that tag until an even
// newer release appeared. Paging past unusable releases keeps the fleet on
// the newest installable stable release instead.
func fetchLatestRelease() (*releaseInfo, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var skipped []string
	scanned := 0
	for page := 1; page <= maxReleasePages; page++ {
		releases, err := fetchReleasePage(client, page)
		if err != nil {
			return nil, fmt.Errorf("fetch latest release: %w", err)
		}
		for _, rj := range releases {
			scanned++
			if reason := skipReason(rj); reason != "" {
				if len(skipped) < 5 {
					skipped = append(skipped, fmt.Sprintf("%s (%s)", rj.TagName, reason))
				}
				continue
			}
			return releaseFromJSON(rj), nil
		}
		if len(releases) < releasePageSize {
			break
		}
	}
	detail := ""
	if len(skipped) > 0 {
		detail = "; skipped " + strings.Join(skipped, ", ")
		if scanned > len(skipped) {
			detail += fmt.Sprintf(" and %d more", scanned-len(skipped))
		}
	}
	if scanned == 0 {
		return nil, fmt.Errorf("no stable release with a verified provider tarball found: the release listing is empty")
	}
	return nil, fmt.Errorf("no stable release with a verified provider tarball found in the newest %d releases%s", scanned, detail)
}

// fetchReleasePage lists one page of releases, newest first. GitHub returns
// drafts only to authenticated repo owners; they are filtered by the caller
// either way.
func fetchReleasePage(client *http.Client, page int) ([]releaseJSON, error) {
	api := fmt.Sprintf("%s/releases?per_page=%d&page=%d", repoAPIBase, releasePageSize, page)
	req, err := http.NewRequest("GET", api, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch releases page %d: %w", page, err)
	}
	if k, v := githubAuthHeader(); k != "" {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch releases page %d: %w", page, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == 429 {
		return nil, fmt.Errorf("fetch releases page %d: rate-limited (status %d); set GITHUB_TOKEN for 5000 req/h quota", page, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch releases page %d: status %d", page, resp.StatusCode)
	}
	var releases []releaseJSON
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases page %d: %w", page, err)
	}
	return releases, nil
}

// digestForAsset finds the sha256 digest (hex, "sha256:" prefix stripped)
// for the named asset. Returns "" when the asset is absent or carries no
// digest — callers treat that as a hard refusal, never a silent skip.
func digestForAsset(assets []releaseAsset, wantName string) string {
	for _, a := range assets {
		if a.Name == wantName {
			return strings.TrimPrefix(a.Digest, "sha256:")
		}
	}
	return ""
}

// fetchReleaseByTag queries the GitHub releases/tags/<tag> endpoint and
// returns the tarball sha256 digest for the provider asset. Used when the
// user passes --tag without --digest so the update is always verified
// against the release API's recorded digest.
//
// An explicit tag is an operator override (canary, rollback): it is NOT
// filtered for pre-release names or flags here. Only automatic stable
// selection refuses pre-releases.
func fetchReleaseByTag(tag string) (*releaseInfo, error) {
	// M3 fix: validate tag format and escape for URL to prevent path traversal.
	if err := validateTag(tag); err != nil {
		return nil, err
	}
	api := fmt.Sprintf("%s/releases/tags/%s", repoAPIBase, url.PathEscape(tag))
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", api, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch release %s: %w", tag, err)
	}
	if k, v := githubAuthHeader(); k != "" {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == 429 {
		return nil, fmt.Errorf("fetch release %s: rate-limited (status %d); set GITHUB_TOKEN for 5000 req/h quota", tag, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch release %s: status %d", tag, resp.StatusCode)
	}
	var rj releaseJSON
	if err := json.NewDecoder(resp.Body).Decode(&rj); err != nil {
		return nil, fmt.Errorf("decode release %s: %w", tag, err)
	}
	info := releaseFromJSON(rj)
	// The tag endpoint echoes the requested tag; trust the path, not a
	// response field, so a malformed body cannot redirect the download URL.
	info.Tag = tag
	info.URL = providerTarballURL(tag)
	info.ProviderDigest = digestForAsset(rj.Assets, providerTarballName(tag))
	if info.ProviderDigest == "" {
		return nil, fmt.Errorf("release %s: asset %s has no sha256 digest; refusing unverified download", tag, providerTarballName(tag))
	}
	return info, nil
}

// releaseCacheTTL bounds how often we hit the GitHub API.
const releaseCacheTTL = 5 * time.Minute

// cachedLatest caches the latest release lookup so repeated invocations in
// a short window don't hammer the API. Mutex-guarded: the CLI is mostly
// sequential but tests and future concurrent callers (multi-provider batch
// update) can race on the two package vars.
var (
	cachedLatest     *releaseInfo
	cachedLatestTime time.Time
	latestReleaseMu  sync.Mutex
)

// latestRelease returns the latest stable release, using the short cache.
func latestRelease() (*releaseInfo, error) {
	latestReleaseMu.Lock()
	defer latestReleaseMu.Unlock()
	if cachedLatest != nil && time.Since(cachedLatestTime) < releaseCacheTTL {
		return cachedLatest, nil
	}
	info, err := fetchLatestRelease()
	if err != nil {
		return nil, err
	}
	cachedLatest = info
	cachedLatestTime = time.Now()
	return info, nil
}
