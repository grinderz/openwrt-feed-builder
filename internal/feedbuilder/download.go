package feedbuilder

// HTTP helpers: a retrying client plus a tiny on-disk download cache.

import (
	"context"
	"crypto/sha1" //nolint:gosec // cache key only
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// retryStatus reports whether an HTTP status code triggers a retry.
func retryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}

	return false
}

const (
	maxRetries    = 4
	backoffFactor = 500 * time.Millisecond
	httpTimeout   = 120 * time.Second
)

// Client is a retrying HTTP client with a fixed User-Agent.
type Client struct {
	hc        *http.Client
	userAgent string
}

func newClient() *Client {
	return &Client{
		hc:        &http.Client{Timeout: httpTimeout},
		userAgent: "openwrt-feed-builder/" + Version,
	}
}

// do issues a GET to url with the given extra headers, retrying transient
// failures. The caller owns closing the returned response body.
func (c *Client) do(ctx context.Context, url string, headers map[string]string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// backoff_factor * 2^(attempt-1), matching urllib3's schedule.
			time.Sleep(backoffFactor * time.Duration(1<<(attempt-1)))
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}

		req.Header.Set("User-Agent", c.userAgent)

		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if retryStatus(resp.StatusCode) && attempt < maxRetries {
			closeQuietly(resp.Body)
			lastErr = fmt.Errorf("%w %d", errHTTPStatus, resp.StatusCode)

			continue
		}

		return resp, nil
	}

	return nil, lastErr
}

// fetchBytes GETs url and returns the body, erroring on non-2xx.
func (c *Client) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	resp, err := c.do(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %w %d", url, errHTTPStatus, resp.StatusCode)
	}

	return readAll(resp.Body)
}

// fetchText is fetchBytes returning a string.
func (c *Client) fetchText(ctx context.Context, url string) (string, error) {
	b, err := c.fetchBytes(ctx, url)
	return string(b), err
}

// fetchToFile GETs url and streams the body straight to dest (via a temp file
// renamed into place), erroring on non-2xx. Package downloads can be large;
// they never need to sit in memory whole.
func (c *Client) fetchToFile(ctx context.Context, url, dest string) error {
	resp, err := c.do(ctx, url, nil)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %w %d", url, errHTTPStatus, resp.StatusCode)
	}

	tmp := dest + ".tmp"

	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		closeQuietly(out)
		removeQuietly(tmp)

		return fmt.Errorf("copy: %w", err)
	}

	if err := out.Close(); err != nil {
		removeQuietly(tmp)
		return fmt.Errorf("close: %w", err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// Cache caches raw downloads keyed by URL so re-runs don't re-fetch everything.
// It also remembers every entry the current run used (downloads, repacked
// binaries, UPX results, sdk builds), so a complete build can drop the rest
// afterwards (gc) instead of letting old versions pile up forever.
type Cache struct {
	dir     string
	client  *Client
	refresh bool
	used    map[string]bool // absolute paths of entries this run used
}

func newCache(dir string, client *Client, refresh bool) (*Cache, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	return &Cache{dir: dir, client: client, refresh: refresh, used: map[string]bool{}}, nil
}

// mark records a cache entry (file, or sdk build dir) as used by this run.
func (c *Cache) mark(path string) {
	c.used[filepath.Clean(path)] = true
}

// Cache entries gc may remove. Only names the builder itself creates are
// touched, so a cache_dir pointing somewhere unexpected loses nothing else.
var (
	gcDownloadRE = regexp.MustCompile(`^[0-9a-f]{40}\.ipk(\.tmp)?$`) // <sha1(url)>.ipk
	gcBuiltRE    = regexp.MustCompile(`\.(ipk|apk)(\.tmp)?$`)        // built/<pkg>_<ver>_<arch>.*
	gcUpxRE      = regexp.MustCompile(`^[0-9a-f]{64}(\.work)?$`)     // upx/<sha256>
)

// sdkCacheDepth is how deep an sdk build dir sits under <cache>/sdk:
// <source>/<builder>/<release>/<target>-<subtarget>.
const sdkCacheDepth = 4

// gc removes every cache entry this run did not use: downloads of URLs no
// source resolves to any more (old releases, dropped sources), repacked
// binaries and UPX results of old versions, sdk builds of combinations no
// longer configured. Only safe after a run that visited every source — the
// caller skips it for --only runs and runs with failed sources. Returns the
// number of entries removed and the bytes freed.
func (c *Cache) gc() (int, int64, error) {
	removed, freed := 0, int64(0)
	drop := func(path string, size int64) error {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove: %w", err)
		}

		removed++
		freed += size

		return nil
	}

	for sub, pattern := range map[string]*regexp.Regexp{"": gcDownloadRE, "built": gcBuiltRE, "upx": gcUpxRE} {
		entries, err := os.ReadDir(filepath.Join(c.dir, sub))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return removed, freed, fmt.Errorf("read dir: %w", err)
		}

		for _, entry := range entries {
			path := filepath.Join(c.dir, sub, entry.Name())
			if !entry.Type().IsRegular() || !pattern.MatchString(entry.Name()) || c.used[path] {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				return removed, freed, fmt.Errorf("stat: %w", err)
			}

			if err := drop(path, info.Size()); err != nil {
				return removed, freed, err
			}
		}
	}

	// sdk builds are whole dirs; an unused one goes with its contents, then
	// any parent left empty
	sdkRoot := filepath.Join(c.dir, "sdk")

	var stale []string

	_ = filepath.WalkDir(sdkRoot, func(path string, d os.DirEntry, err error) error { // no sdk/: nothing to do
		if err != nil || !d.IsDir() || path == sdkRoot {
			return nil //nolint:nilerr // unreadable entries are left alone
		}

		rel, _ := filepath.Rel(sdkRoot, path)
		if strings.Count(rel, string(filepath.Separator)) == sdkCacheDepth-1 {
			if !c.used[path] {
				stale = append(stale, path)
			}

			return filepath.SkipDir
		}

		return nil
	})

	for _, dir := range stale {
		size := int64(0)

		_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error { // size is informational
			if err == nil && d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					size += info.Size()
				}
			}

			return nil
		})
		if err := drop(dir, size); err != nil {
			return removed, freed, err
		}

		parent := filepath.Dir(dir)
		for parent != sdkRoot && strings.HasPrefix(parent, sdkRoot) {
			if os.Remove(parent) != nil { // fails while not empty
				break
			}

			parent = filepath.Dir(parent)
		}
	}

	return removed, freed, nil
}

func (c *Cache) pathFor(url string) string {
	sum := sha1.Sum([]byte(url)) //nolint:gosec // cache key, not security
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".ipk")
}

// get returns the local path of a remote file's content, downloading it if
// needed. An existing download is validated against the metadata the source
// exposes (size, sha256): a match skips the network entirely, a mismatch
// (upstream republished under the same URL, or a corrupted download)
// re-fetches. Sources without metadata keep the URL-existence behavior.
func (c *Cache) get(ctx context.Context, file remoteFile) (string, error) {
	path := c.pathFor(file.url)
	c.mark(path)

	if !c.refresh {
		if _, err := os.Stat(path); err == nil {
			mismatch, err := cachedMismatch(path, file)
			if err == nil && mismatch == "" {
				return path, nil
			}

			if mismatch != "" {
				fmt.Fprintf(os.Stderr, "  ~ cached %s: %s; re-downloading\n",
					lastPathPart(stripQuery(file.url)), mismatch)
			}
		}
	}

	if err := c.client.fetchToFile(ctx, file.url, path); err != nil {
		return "", err
	}

	if mismatch, err := cachedMismatch(path, file); err != nil {
		return "", err
	} else if mismatch != "" {
		removeQuietly(path)
		return "", fmt.Errorf("%w: %s does not match the source's metadata: %s", errDownload, file.url, mismatch)
	}

	return path, nil
}

// cachedMismatch compares a local file against a remoteFile's expected size
// and sha256, returning a human-readable description of the first mismatch
// ("" = everything known matches).
func cachedMismatch(path string, file remoteFile) (string, error) {
	if file.size > 0 {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("stat: %w", err)
		}

		if info.Size() != file.size {
			return fmt.Sprintf("size %d != expected %d", info.Size(), file.size), nil
		}
	}

	if file.sha256 != "" {
		sum, err := sha256File(path)
		if err != nil {
			return "", err
		}

		if !strings.EqualFold(sum, file.sha256) {
			return "sha256 mismatch", nil
		}
	}

	return "", nil
}
