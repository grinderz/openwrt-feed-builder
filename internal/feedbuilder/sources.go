package feedbuilder

// Resolve configured sources into concrete package (.ipk / .apk) download URLs.
//
// Supported source types:
//
//   - ipk        : explicit list of package URLs (.ipk or .apk)
//   - feed       : an existing opkg or apk feed; its Packages[.gz] or
//                  packages.adb index is parsed and the referenced package
//                  files are mirrored (include/exclude globs)
//   - github     : package assets attached to a GitHub release (latest or a tag)
//   - github_dir : a directory inside a GitHub repo (Contents API)
//   - html       : an HTML autoindex / directory listing scraped for package links
//
// File-name patterns default to the formats the carried branches use
// (defaultPkgPattern): "*.ipk" for opkg-only layouts, "*.apk" for apk-only,
// "*.[ai]pk" for both.
//   - binary     : raw binaries / archives repacked into locally built .ipk
//                  files (handled in binary.go, not here)
//   - sdk        : packages compiled from source with the OpenWrt SDK via an
//                  openwrt-buildroot checkout (handled in sdk.go, not here)
//
// Each resolver only yields URLs; downloading is handled by the cache so all
// source types share one retrying, cached fetch path.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var hrefRE = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)

// wildcard holds the glob metacharacters that make a pattern out of a name.
const wildcard = "*?["

// remoteFile is one resolvable package file: its URL plus whatever integrity
// metadata the source exposes (feed indexes carry size + sha256, the GitHub
// APIs carry size, plain URL/html sources carry nothing). The cache uses the
// metadata to validate an existing download instead of trusting the URL alone.
type remoteFile struct {
	url    string
	size   int64  // expected byte size, 0 = unknown
	sha256 string // expected hex sha256, "" = unknown
	arch   string // package architecture when the source knows it (feed index), "" = unknown
}

func urlsOnly(urls []string) []remoteFile {
	out := make([]remoteFile, 0, len(urls))
	for _, u := range urls {
		out = append(out, remoteFile{url: u})
	}

	return out
}

func hasWildcard(s string) bool {
	return strings.ContainsAny(s, wildcard)
}

// fnmatch reports whether name matches the shell-glob pattern. Patterns here are
// simple (*, ?, [..]) and operate on a single path segment, so path.Match fits.
func fnmatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// resolveURL joins ref against base, like urllib.parse.urljoin.
func resolveURL(base, ref string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return ref
	}

	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}

	return parsed.ResolveReference(r).String()
}

// expandIPKURL expands a (possibly wildcarded) .ipk URL.
//
// Without a wildcard the URL is returned as-is. With a wildcard in the file name
// (e.g. .../bar_*_all.ipk) the containing directory is listed, the matching
// files are grouped by package name, and only the latest version of each is
// returned.
func expandIPKURL(ctx context.Context, client *Client, raw string) ([]string, error) {
	split, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}

	dirname, filename := path.Split(split.Path)
	if !hasWildcard(filename) {
		return []string{raw}, nil
	}

	base := fmt.Sprintf("%s://%s%s", split.Scheme, split.Host, dirname)

	html, err := client.fetchText(ctx, base)
	if err != nil {
		return nil, err
	}

	type match struct{ name, full string }

	var matches []match

	seen := map[string]bool{}

	for _, m := range hrefRE.FindAllStringSubmatch(html, -1) {
		href := m[1]

		name := lastPathPart(stripQuery(href))
		if name != "" && !seen[name] && fnmatch(filename, name) {
			seen[name] = true
			matches = append(matches, match{name, resolveURL(base, href)})
		}
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: no files matched %q at %s", errSource, filename, base)
	}

	// Pick the highest version per package name.
	type best struct{ version, url string }

	bestByKey := map[string]best{}

	for _, candidate := range matches {
		var key, ver string
		if name, version, arch, ok := ipkMatch(candidate.name); ok {
			key = name + "_" + arch
			ver = version
		} else { // unparseable name: treat each distinct name as its own group
			key, ver = candidate.name, "0"
		}

		cur, exists := bestByKey[key]
		if !exists || compareVersion(ver, cur.version) > 0 {
			bestByKey[key] = best{ver, candidate.full}
		}
	}

	out := make([]string, 0, len(bestByKey))
	for _, b := range bestByKey {
		out = append(out, b.url)
	}

	sort.Strings(out) // mirror Python's sort by URL

	return out, nil
}

func stripQuery(s string) string {
	if before, _, ok := strings.Cut(s, "?"); ok {
		return before
	}

	return s
}

func lastPathPart(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}

	return s
}

func matchesInclExcl(name string, includes, excludes []string) bool {
	if len(includes) == 0 {
		includes = []string{"*"}
	}

	included := false

	for _, pat := range includes {
		if fnmatch(pat, name) {
			included = true
			break
		}
	}

	if !included {
		return false
	}

	for _, pat := range excludes {
		if fnmatch(pat, name) {
			return false
		}
	}

	return true
}

// defaultPkgPattern is the package file glob sources use when none is
// configured: the extensions of the formats the carried branches use.
func defaultPkgPattern(layout Layout) string {
	formats := toSet(layout.formats())
	switch {
	case formats[formatIPK] && formats[formatAPK]:
		return "*.[ai]pk"
	case formats[formatAPK]:
		return "*.apk"
	}

	return "*.ipk"
}

// feedURLs reads an opkg (Packages.gz / Packages) or apk (packages.adb) feed
// index and returns the package files it references. The url is the feed dir;
// a url pointing at the index file itself works too.
func feedURLs(ctx context.Context, client *Client, src Source) ([]remoteFile, error) {
	base := strings.TrimRight(src.strOr("url", ""), "/")
	for _, index := range []string{"/" + opkgIndexName + ".gz", "/" + opkgIndexName, "/" + apkIndexName} {
		base = strings.TrimSuffix(base, index)
	}

	var (
		stanzas []map[string]string
		text    string
		lastErr error
	)

	got := false

	for _, candidate := range []string{"/" + opkgIndexName + ".gz", "/" + opkgIndexName, "/" + apkIndexName} {
		if candidate == "/"+apkIndexName {
			raw, err := client.fetchBytes(ctx, base+candidate)
			if err != nil {
				lastErr = err
				continue
			}

			if stanzas, err = parseADBIndex(raw); err != nil {
				lastErr = err
				continue
			}

			got = true

			break
		}

		if strings.HasSuffix(candidate, ".gz") {
			raw, err := client.fetchBytes(ctx, base+candidate)
			if err != nil {
				lastErr = err
				continue
			}

			gzr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				lastErr = err
				continue
			}

			dec, err := readAllGzip(gzr)
			if err != nil {
				lastErr = err
				continue
			}

			text = dec
			got = true

			break
		}

		body, err := client.fetchText(ctx, base+candidate)
		if err != nil {
			lastErr = err
			continue
		}

		text = body
		got = true

		break
	}

	if !got {
		return nil, fmt.Errorf("could not read package index from %s: %w", base, lastErr)
	}

	if stanzas == nil {
		stanzas = parseIndex(text)
	}

	includes := src.strSlice("include")
	excludes := src.strSlice("exclude")

	var out []remoteFile

	for _, stanza := range stanzas {
		filename := stanza["Filename"]
		pkg := stanza["Package"]

		if filename == "" {
			continue
		}

		if !matchesInclExcl(pkg, includes, excludes) {
			continue
		}

		size, _ := strconv.ParseInt(strings.TrimSpace(stanza["Size"]), 10, 64) // 0 = unknown
		out = append(out, remoteFile{
			url:    base + "/" + strings.TrimLeft(filename, "/"),
			size:   size,
			sha256: strings.ToLower(strings.TrimSpace(stanza["SHA256sum"])),
			arch:   stanza["Architecture"],
		})
	}

	return out, nil
}

func readAllGzip(gz *gzip.Reader) (string, error) {
	defer closeQuietly(gz)

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(gz); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}

	return buf.String(), nil
}

var ghTreeRE = regexp.MustCompile(
	`^https?://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/tree/(?P<ref>[^/]+)/(?P<rest>.+)$`)

func githubHeaders() map[string]string {
	h := map[string]string{"Accept": "application/vnd.github+json"}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		h["Authorization"] = "Bearer " + token
	}

	return h
}

// ghGet GETs a GitHub API URL, turning common failures into helpful messages.
func ghGet(ctx context.Context, client *Client, apiURL string) ([]byte, error) {
	resp, err := client.do(ctx, apiURL, githubHeaders())
	if err != nil {
		return nil, err
	}
	defer closeQuietly(resp.Body)

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, fmt.Errorf("GitHub API %s: %w", apiURL, err)
	}

	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-Ratelimit-Remaining") == "0" {
		return nil, fmt.Errorf("%w rate limit exceeded. Set GITHUB_TOKEN to raise "+
			"the limit (export GITHUB_TOKEN=ghp_...). Unauthenticated requests are limited "+
			"to 60/hour per IP", errGitHub)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: resource not found (check repo / tag / path): %s", errGitHub, apiURL)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w %s: %w %d", errGitHub, apiURL, errHTTPStatus, resp.StatusCode)
	}

	return buf.Bytes(), nil
}

type ghContent struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

type ghAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	ID      int64  `json:"id"`
	TagName string `json:"tag_name"`
}

// parseGithubTreeURL parses https://github.com/<owner>/<repo>/tree/<ref>/<path>[/<glob>].
// Without a trailing glob the pattern is "" (the caller's default applies).
// ghTreeRef is a parsed GitHub tree URL: repo, git ref, dir path and an
// optional file glob ("" = the caller's default).
type ghTreeRef struct {
	repo, ref, path, pattern string
}

func parseGithubTreeURL(raw string) (ghTreeRef, error) {
	match := ghTreeRE.FindStringSubmatch(raw)
	if match == nil {
		return ghTreeRef{}, fmt.Errorf("%w: not a GitHub tree URL: %s", errSource, raw)
	}

	ref := ghTreeRef{repo: match[1] + "/" + match[2], ref: match[3]}
	rest := strings.TrimRight(match[4], "/")
	segs := strings.Split(rest, "/")

	last := segs[len(segs)-1]
	if hasWildcard(last) {
		ref.pattern = last
		ref.path = strings.Join(segs[:len(segs)-1], "/")
	} else {
		ref.path = rest
	}

	return ref, nil
}

func githubDirURLs(ctx context.Context, client *Client, src Source, defPattern string) ([]remoteFile, error) {
	var repo, ref, dirPath, pattern string

	if u := src.strOr("url", ""); u != "" {
		tree, err := parseGithubTreeURL(u)
		if err != nil {
			return nil, err
		}

		repo, ref, dirPath, pattern = tree.repo, tree.ref, tree.path, tree.pattern

		pattern = src.strOr("pattern", orDefault(pattern, defPattern))
		ref = src.strOr("ref", ref)
	} else {
		repo = src.strOr("repo", "")
		ref = src.strOr("ref", src.strOr("branch", ""))
		dirPath = strings.Trim(src.strOr("path", ""), "/")
		pattern = src.strOr("pattern", defPattern)
	}

	recursive := src.boolOr("recursive", false)

	var out []remoteFile

	stack := []string{dirPath}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		api := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", repo, current)
		if ref != "" {
			api += "?ref=" + ref
		}

		body, err := ghGet(ctx, client, api)
		if err != nil {
			return nil, err
		}

		entries, err := decodeContents(body)
		if err != nil {
			return nil, err
		}

		for _, entry := range entries {
			if entry.Type == "dir" {
				if recursive {
					stack = append(stack, entry.Path)
				}

				continue
			}

			if entry.DownloadURL != "" && fnmatch(pattern, entry.Name) {
				out = append(out, remoteFile{url: entry.DownloadURL, size: entry.Size})
			}
		}
	}

	return out, nil
}

// decodeContents handles the Contents API returning either an array (directory)
// or a single object (a single file).
func decodeContents(body []byte) ([]ghContent, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var entries []ghContent
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, fmt.Errorf("decode JSON: %w", err)
		}

		return entries, nil
	}

	var single ghContent
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}

	return []ghContent{single}, nil
}

// githubURLs resolves a github release source. Besides the asset URLs it
// returns the resolved tag name (meaningful when tag is "latest"), which the
// caller may use to derive a per-source kmod version.
func githubURLs(ctx context.Context, client *Client, src Source, defPattern string) ([]remoteFile, string, error) {
	repo := src.strOr("repo", "")
	tag := src.strOr("tag", "latest")

	var api string
	if tag == "" || tag == "latest" {
		api = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	} else {
		api = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	}

	body, err := ghGet(ctx, client, api)
	if err != nil {
		return nil, "", err
	}

	release, err := decodeRelease(body)
	if err != nil {
		return nil, "", err
	}

	pattern := src.strOr("asset_match", defPattern)

	assets, err := githubReleaseAssets(ctx, client, repo, release.ID)
	if err != nil {
		return nil, "", err
	}

	var out []remoteFile

	for _, a := range assets {
		if fnmatch(pattern, a.Name) {
			out = append(out, remoteFile{url: a.BrowserDownloadURL, size: a.Size})
		}
	}

	return out, orDefault(release.TagName, tag), nil
}

func decodeRelease(body []byte) (ghRelease, error) {
	var release ghRelease

	if err := json.Unmarshal(body, &release); err != nil {
		return release, fmt.Errorf("decode JSON: %w", err)
	}

	return release, nil
}

// githubReleaseAssets lists every asset of a release. The release object only
// embeds the first page of assets (~30), so this pages through the dedicated
// assets endpoint to get them all.
func githubReleaseAssets(ctx context.Context, client *Client, repo string, releaseID int64) ([]ghAsset, error) {
	const perPage = 100

	page := 1

	var out []ghAsset

	for {
		assetsURL := fmt.Sprintf(
			"https://api.github.com/repos/%s/releases/%d/assets?per_page=%d&page=%d",
			repo, releaseID, perPage, page)

		body, err := ghGet(ctx, client, assetsURL)
		if err != nil {
			return nil, err
		}

		var assets []ghAsset
		if err := json.Unmarshal(body, &assets); err != nil {
			return nil, fmt.Errorf("decode JSON: %w", err)
		}

		out = append(out, assets...)
		if len(assets) < perPage {
			break
		}

		page++
	}

	return out, nil
}

func htmlURLs(ctx context.Context, client *Client, src Source, defPattern string) ([]remoteFile, error) {
	base := src.strOr("url", "")

	html, err := client.fetchText(ctx, base)
	if err != nil {
		return nil, err
	}

	pattern := src.strOr("pattern", defPattern)
	seen := map[string]bool{}

	var out []remoteFile

	for _, m := range hrefRE.FindAllStringSubmatch(html, -1) {
		href := m[1]

		name := lastPathPart(stripQuery(href))
		if !fnmatch(pattern, name) {
			continue
		}

		u := resolveURL(base, href)
		if !seen[u] {
			seen[u] = true
			out = append(out, remoteFile{url: u})
		}
	}

	return out, nil
}

// expandSourceTags expands a github source with a `tags:` list into one copy
// per tag (each with a distinct display name), so a single source entry can
// pull several releases in one pass. Any other source passes through as-is.
func expandSourceTags(src Source) []Source {
	tags := src.strSlice("tags")
	if src.strOr("type", "") != "github" || len(tags) == 0 {
		return []Source{src}
	}

	name := src.strOr("name", src.strOr("type", ""))

	out := make([]Source, 0, len(tags))
	for _, tag := range tags {
		dup := Source{}
		maps.Copy(dup, src)
		delete(dup, "tags")
		dup["tag"] = tag
		dup["name"] = name + "@" + tag
		out = append(out, dup)
	}

	return out
}

// iterIPKURLs resolves one source into concrete package files (URL +
// integrity metadata where the source exposes it). defPattern is the file
// glob for sources without an explicit one (defaultPkgPattern). The second
// return value is the resolved release tag for github sources ("" for other
// types).
func iterIPKURLs(ctx context.Context, client *Client, src Source, defPattern string) ([]remoteFile, string, error) {
	switch src.strOr("type", "") {
	case "ipk":
		var out []remoteFile

		for _, u := range src.strSlice("urls") {
			expanded, err := expandIPKURL(ctx, client, u)
			if err != nil {
				return nil, "", err
			}

			out = append(out, urlsOnly(expanded)...)
		}

		return out, "", nil
	case "feed":
		files, err := feedURLs(ctx, client, src)
		return files, "", err
	case "github":
		return githubURLs(ctx, client, src, defPattern)
	case "github_dir":
		files, err := githubDirURLs(ctx, client, src, defPattern)
		return files, "", err
	case "html":
		files, err := htmlURLs(ctx, client, src, defPattern)
		return files, "", err
	default:
		return nil, "", fmt.Errorf("%w: unknown source type %q", errSource, src.strOr("type", ""))
	}
}
