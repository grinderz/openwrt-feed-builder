package feedbuilder

// Resolve configured sources into concrete .ipk download URLs.
//
// Supported source types:
//
//   - ipk        : explicit list of .ipk URLs
//   - feed       : an existing opkg feed; its Packages[.gz] index is parsed and
//                  the referenced .ipk files are mirrored (include/exclude globs)
//   - github     : .ipk assets attached to a GitHub release (latest or a tag)
//   - github_dir : a directory inside a GitHub repo (Contents API)
//   - html       : an HTML autoindex / directory listing scraped for .ipk links
//   - binary     : raw binaries / archives repacked into locally built .ipk
//                  files (handled in binary.go, not here)
//
// Each resolver only yields URLs; downloading is handled by the cache so all
// source types share one retrying, cached fetch path.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	hrefRE   = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)
	wildcard = "*?["
)

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
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// expandIPKURL expands a (possibly wildcarded) .ipk URL.
//
// Without a wildcard the URL is returned as-is. With a wildcard in the file name
// (e.g. .../bar_*_all.ipk) the containing directory is listed, the matching
// files are grouped by package name, and only the latest version of each is
// returned.
func expandIPKURL(client *Client, raw string) ([]string, error) {
	split, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	dirname, filename := path.Split(split.Path)
	if !hasWildcard(filename) {
		return []string{raw}, nil
	}

	base := fmt.Sprintf("%s://%s%s", split.Scheme, split.Host, dirname)
	html, err := client.fetchText(base)
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
		return nil, fmt.Errorf("no files matched %q at %s", filename, base)
	}

	// Pick the highest version per package name.
	type best struct{ version, url string }
	bestByKey := map[string]best{}
	for _, mt := range matches {
		var key, ver string
		if name, version, arch, ok := ipkMatch(mt.name); ok {
			key = name + "_" + arch
			ver = version
		} else { // unparseable name: treat each distinct name as its own group
			key, ver = mt.name, "0"
		}
		cur, exists := bestByKey[key]
		if !exists || compareVersion(ver, cur.version) > 0 {
			bestByKey[key] = best{ver, mt.full}
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
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i]
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
	any := false
	for _, pat := range includes {
		if fnmatch(pat, name) {
			any = true
			break
		}
	}
	if !any {
		return false
	}
	for _, pat := range excludes {
		if fnmatch(pat, name) {
			return false
		}
	}
	return true
}

func feedURLs(client *Client, src Source) ([]string, error) {
	base := strings.TrimRight(src.strOr("url", ""), "/")
	var text string
	var lastErr error
	got := false
	for _, candidate := range []string{"/Packages.gz", "/Packages"} {
		if strings.HasSuffix(candidate, ".gz") {
			raw, err := client.fetchBytes(base + candidate)
			if err != nil {
				lastErr = err
				continue
			}
			gz, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				lastErr = err
				continue
			}
			dec, err := readAllGzip(gz)
			if err != nil {
				lastErr = err
				continue
			}
			text = dec
			got = true
			break
		}
		t, err := client.fetchText(base + candidate)
		if err != nil {
			lastErr = err
			continue
		}
		text = t
		got = true
		break
	}
	if !got {
		return nil, fmt.Errorf("could not read package index from %s: %v", base, lastErr)
	}

	includes := src.strSlice("include")
	excludes := src.strSlice("exclude")
	var out []string
	for _, stanza := range parseIndex(text) {
		filename := stanza["Filename"]
		pkg := stanza["Package"]
		if filename == "" {
			continue
		}
		if !matchesInclExcl(pkg, includes, excludes) {
			continue
		}
		out = append(out, base+"/"+strings.TrimLeft(filename, "/"))
	}
	return out, nil
}

func readAllGzip(gz *gzip.Reader) (string, error) {
	defer gz.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(gz); err != nil {
		return "", err
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
func ghGet(client *Client, apiURL string) ([]byte, error) {
	resp, err := client.do(apiURL, githubHeaders())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if resp.StatusCode == 403 && resp.Header.Get("x-ratelimit-remaining") == "0" {
		return nil, fmt.Errorf("GitHub API rate limit exceeded. Set GITHUB_TOKEN to raise " +
			"the limit (export GITHUB_TOKEN=ghp_...). Unauthenticated requests are limited " +
			"to 60/hour per IP.")
	}
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("GitHub resource not found (check repo / tag / path): %s", apiURL)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API %s: HTTP %d", apiURL, resp.StatusCode)
	}
	return buf.Bytes(), nil
}

type ghContent struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	DownloadURL string `json:"download_url"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	ID      int64  `json:"id"`
	TagName string `json:"tag_name"`
}

// parseGithubTreeURL parses https://github.com/<owner>/<repo>/tree/<ref>/<path>[/<glob>].
func parseGithubTreeURL(raw string) (repo, ref, p, pattern string, err error) {
	m := ghTreeRE.FindStringSubmatch(raw)
	if m == nil {
		return "", "", "", "", fmt.Errorf("not a GitHub tree URL: %s", raw)
	}
	repo = m[1] + "/" + m[2]
	ref = m[3]
	rest := strings.TrimRight(m[4], "/")
	segs := strings.Split(rest, "/")
	last := segs[len(segs)-1]
	if hasWildcard(last) {
		pattern = last
		p = strings.Join(segs[:len(segs)-1], "/")
	} else {
		pattern = "*.ipk"
		p = rest
	}
	return repo, ref, p, pattern, nil
}

func githubDirURLs(client *Client, src Source) ([]string, error) {
	var repo, ref, p, pattern string
	if u := src.strOr("url", ""); u != "" {
		var err error
		repo, ref, p, pattern, err = parseGithubTreeURL(u)
		if err != nil {
			return nil, err
		}
		pattern = src.strOr("pattern", pattern)
		ref = src.strOr("ref", ref)
	} else {
		repo = src.strOr("repo", "")
		ref = src.strOr("ref", src.strOr("branch", ""))
		p = strings.Trim(src.strOr("path", ""), "/")
		pattern = src.strOr("pattern", "*.ipk")
	}

	recursive := src.boolOr("recursive", false)

	var out []string
	stack := []string{p}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		api := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", repo, current)
		if ref != "" {
			api += "?ref=" + ref
		}
		body, err := ghGet(client, api)
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
				out = append(out, entry.DownloadURL)
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
			return nil, err
		}
		return entries, nil
	}
	var single ghContent
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, err
	}
	return []ghContent{single}, nil
}

// githubURLs resolves a github release source. Besides the asset URLs it
// returns the resolved tag name (meaningful when tag is "latest"), which the
// caller may use to derive a per-source kmod version.
func githubURLs(client *Client, src Source) ([]string, string, error) {
	repo := src.strOr("repo", "")
	tag := src.strOr("tag", "latest")
	var api string
	if tag == "" || tag == "latest" {
		api = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	} else {
		api = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	}

	body, err := ghGet(client, api)
	if err != nil {
		return nil, "", err
	}
	release, err := decodeRelease(body)
	if err != nil {
		return nil, "", err
	}
	pattern := src.strOr("asset_match", "*.ipk")

	assets, err := githubReleaseAssets(client, repo, release.ID)
	if err != nil {
		return nil, "", err
	}
	var out []string
	for _, a := range assets {
		if fnmatch(pattern, a.Name) {
			out = append(out, a.BrowserDownloadURL)
		}
	}
	return out, orDefault(release.TagName, tag), nil
}

func decodeRelease(body []byte) (ghRelease, error) {
	var release ghRelease
	err := json.Unmarshal(body, &release)
	return release, err
}

// githubReleaseAssets lists every asset of a release. The release object only
// embeds the first page of assets (~30), so this pages through the dedicated
// assets endpoint to get them all.
func githubReleaseAssets(client *Client, repo string, releaseID int64) ([]ghAsset, error) {
	const perPage = 100
	page := 1
	var out []ghAsset
	for {
		assetsURL := fmt.Sprintf(
			"https://api.github.com/repos/%s/releases/%d/assets?per_page=%d&page=%d",
			repo, releaseID, perPage, page)
		ab, err := ghGet(client, assetsURL)
		if err != nil {
			return nil, err
		}
		var assets []ghAsset
		if err := json.Unmarshal(ab, &assets); err != nil {
			return nil, err
		}
		out = append(out, assets...)
		if len(assets) < perPage {
			break
		}
		page++
	}
	return out, nil
}

func htmlURLs(client *Client, src Source) ([]string, error) {
	base := src.strOr("url", "")
	html, err := client.fetchText(base)
	if err != nil {
		return nil, err
	}
	pattern := src.strOr("pattern", "*.ipk")
	seen := map[string]bool{}
	var out []string
	for _, m := range hrefRE.FindAllStringSubmatch(html, -1) {
		href := m[1]
		name := lastPathPart(stripQuery(href))
		if !fnmatch(pattern, name) {
			continue
		}
		u := resolveURL(base, href)
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
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
		copy := Source{}
		for k, v := range src {
			copy[k] = v
		}
		delete(copy, "tags")
		copy["tag"] = tag
		copy["name"] = name + "@" + tag
		out = append(out, copy)
	}
	return out
}

// iterIPKURLs resolves one source into concrete .ipk URLs. The second return
// value is the resolved release tag for github sources ("" for other types).
func iterIPKURLs(client *Client, src Source) ([]string, string, error) {
	switch src.strOr("type", "") {
	case "ipk":
		var out []string
		for _, u := range src.strSlice("urls") {
			expanded, err := expandIPKURL(client, u)
			if err != nil {
				return nil, "", err
			}
			out = append(out, expanded...)
		}
		return out, "", nil
	case "feed":
		urls, err := feedURLs(client, src)
		return urls, "", err
	case "github":
		return githubURLs(client, src)
	case "github_dir":
		urls, err := githubDirURLs(client, src)
		return urls, "", err
	case "html":
		urls, err := htmlURLs(client, src)
		return urls, "", err
	default:
		return nil, "", fmt.Errorf("unknown source type: %q", src.strOr("type", ""))
	}
}
