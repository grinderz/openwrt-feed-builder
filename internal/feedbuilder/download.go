package feedbuilder

// HTTP helpers: a retrying client plus a tiny on-disk download cache.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// retryStatuses are the HTTP status codes that trigger a retry.
var retryStatuses = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

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
		userAgent: fmt.Sprintf("openwrt-feed-builder/%s", appVersion),
	}
}

// do issues a GET to url with the given extra headers, retrying transient
// failures. The caller owns closing the returned response body.
func (c *Client) do(url string, headers map[string]string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// backoff_factor * 2^(attempt-1), matching urllib3's schedule.
			time.Sleep(backoffFactor * time.Duration(1<<(attempt-1)))
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
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
		if retryStatuses[resp.StatusCode] && attempt < maxRetries {
			resp.Body.Close()
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// fetchBytes GETs url and returns the body, erroring on non-2xx.
func (c *Client) fetchBytes(url string) ([]byte, error) {
	resp, err := c.do(url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// fetchText is fetchBytes returning a string.
func (c *Client) fetchText(url string) (string, error) {
	b, err := c.fetchBytes(url)
	return string(b), err
}

// fetchToFile GETs url and streams the body straight to dest (via a temp file
// renamed into place), erroring on non-2xx. Package downloads can be large;
// they never need to sit in memory whole.
func (c *Client) fetchToFile(url, dest string) error {
	resp, err := c.do(url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// Cache caches raw downloads keyed by URL so re-runs don't re-fetch everything.
type Cache struct {
	dir     string
	client  *Client
	refresh bool
}

func newCache(dir string, client *Client, refresh bool) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, client: client, refresh: refresh}, nil
}

func (c *Cache) pathFor(url string) string {
	sum := sha1.Sum([]byte(url))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".ipk")
}

// get returns the local path of a remote file's content, downloading it if
// needed. An existing download is validated against the metadata the source
// exposes (size, sha256): a match skips the network entirely, a mismatch
// (upstream republished under the same URL, or a corrupted download)
// re-fetches. Sources without metadata keep the URL-existence behavior.
func (c *Cache) get(rf remoteFile) (string, error) {
	path := c.pathFor(rf.url)
	if !c.refresh {
		if _, err := os.Stat(path); err == nil {
			mismatch, err := cachedMismatch(path, rf)
			if err == nil && mismatch == "" {
				return path, nil
			}
			if mismatch != "" {
				fmt.Fprintf(os.Stderr, "  ~ cached %s: %s; re-downloading\n",
					lastPathPart(stripQuery(rf.url)), mismatch)
			}
		}
	}
	if err := c.client.fetchToFile(rf.url, path); err != nil {
		return "", err
	}
	if mismatch, err := cachedMismatch(path, rf); err != nil {
		return "", err
	} else if mismatch != "" {
		os.Remove(path)
		return "", fmt.Errorf("downloaded %s does not match the source's metadata: %s", rf.url, mismatch)
	}
	return path, nil
}

// cachedMismatch compares a local file against a remoteFile's expected size
// and sha256, returning a human-readable description of the first mismatch
// ("" = everything known matches).
func cachedMismatch(path string, rf remoteFile) (string, error) {
	if rf.size > 0 {
		st, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if st.Size() != rf.size {
			return fmt.Sprintf("size %d != expected %d", st.Size(), rf.size), nil
		}
	}
	if rf.sha256 != "" {
		sum, err := sha256File(path)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(sum, rf.sha256) {
			return "sha256 mismatch", nil
		}
	}
	return "", nil
}
