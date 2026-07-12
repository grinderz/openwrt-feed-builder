package feedbuilder

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{nil, 0o755},
		{"0755", 0o755},
		{"0644", 0o644},
		{"0o600", 0o600},
		{755, 0o755}, // bare decimal (yaml saw no leading zero): digits are octal
		{644, 0o644},
		{493, 0o755}, // yaml.v3 already octal-decoded `mode: 0755` into 493
		{420, 0o644}, // yaml.v3 already octal-decoded `mode: 0644` into 420
	}
	for _, c := range cases {
		got, err := parseMode(c.in, 0o755)
		if err != nil {
			t.Fatalf("parseMode(%v): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseMode(%v) = %o, want %o", c.in, got, c.want)
		}
	}
	if _, err := parseMode("rwxr-xr-x", 0o755); err == nil {
		t.Error("parseMode should reject non-octal strings")
	}
}

func TestExpandPlaceholders(t *testing.T) {
	got := expandPlaceholders("x-{arch}-v{version}.gz", map[string]string{
		"arch": "arm64", "version": "1.2.3",
	})
	if got != "x-arm64-v1.2.3.gz" {
		t.Errorf("got %q", got)
	}
}

func TestUnpackAssetGzip(t *testing.T) {
	payload := []byte("ELF fake binary")
	out, err := unpackAsset("tool-linux-arm64-v1.gz", gzipBytes(t, payload), "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, payload) {
		t.Errorf("payload mismatch")
	}
}

func TestUnpackAssetRaw(t *testing.T) {
	payload := []byte("raw binary")
	out, err := unpackAsset("tool-linux-arm64", payload, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, payload) {
		t.Errorf("payload mismatch")
	}
}

func tarGzArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackAssetTarExtract(t *testing.T) {
	arc := tarGzArchive(t, map[string]string{
		"tool-v1/README.md": "docs",
		"tool-v1/tool":      "the binary",
	})
	out, err := unpackAsset("tool.tar.gz", arc, "*/tool")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "the binary" {
		t.Errorf("got %q", out)
	}

	// several files and no extract glob -> error
	if _, err := unpackAsset("tool.tar.gz", arc, ""); err == nil {
		t.Error("expected error for ambiguous archive without extract")
	}

	// single file unpacks implicitly
	single := tarGzArchive(t, map[string]string{"tool": "solo"})
	out, err = unpackAsset("tool.tgz", single, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "solo" {
		t.Errorf("got %q", out)
	}
}

func TestUnpackAssetZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{"a/tool.exe": "win", "a/tool": "nix"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := unpackAsset("tool.zip", buf.Bytes(), "a/tool")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "nix" {
		t.Errorf("got %q", out)
	}
}

// readDataTar extracts data.tar.gz members from a built ipk: name -> header.
func readDataEntries(t *testing.T, ipkPath string) map[string]*tar.Header {
	t.Helper()
	raw, err := os.ReadFile(ipkPath)
	if err != nil {
		t.Fatal(err)
	}
	dataTar, err := extractMemberFromTar(raw, "data.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := openTar(dataTar)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		h := *hdr
		out[hdr.Name] = &h
	}
	return out
}

func TestBinaryPackages(t *testing.T) {
	binaries := map[string][]byte{
		"mipsle-softfloat": []byte("mips binary"),
		"arm64":            []byte("arm binary"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for arch, data := range binaries {
			if strings.Contains(r.URL.Path, arch) {
				w.Write(gzipBytes(t, data))
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	client := newClient()
	cache, err := newCache(filepath.Join(dir, "cache"), client, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Architectures: []string{"mipsel_24kc", "aarch64_cortex-a53"},
		Layout:        Layout{Versions: []string{"24.10.7"}, Branch: "24.10", DefaultFeed: "packages"},
	}
	src := Source{
		"type":    "binary",
		"name":    "mihomo",
		"version": "1.19.28",
		"url":     srv.URL + "/v{version}/mihomo-linux-{arch}-v{version}.gz",
		"install": "/opt/clash/bin/clash",
		"mode":    "0755",
		"section": "net",
		"arch_map": map[string]any{
			"mipsel_24kc":        "mipsle-softfloat",
			"aarch64_cortex-a53": "arm64",
			"x86_64":             "amd64", // filtered out by cfg.Architectures
		},
		"description": "mihomo binary",
		"postinst":    "#!/bin/sh\nexit 0\n",
	}

	pkgs, err := binaryPackages(cfg, client, cache, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(pkgs))
	}

	for _, p := range pkgs {
		control, err := readControl(p.path)
		if err != nil {
			t.Fatalf("readControl(%s): %v", p.path, err)
		}
		fields := parseFields(control)
		if fields["Package"] != "mihomo" {
			t.Errorf("Package = %q", fields["Package"])
		}
		if fields["Version"] != "1.19.28-1" {
			t.Errorf("Version = %q", fields["Version"])
		}
		if fields["Section"] != "net" {
			t.Errorf("Section = %q", fields["Section"])
		}
		arch := fields["Architecture"]
		wantPayload := binaries[map[string]string{
			"mipsel_24kc": "mipsle-softfloat", "aarch64_cortex-a53": "arm64",
		}[arch]]
		if wantPayload == nil {
			t.Fatalf("unexpected arch %q", arch)
		}

		entries := readDataEntries(t, p.path)
		hdr, ok := entries["./opt/clash/bin/clash"]
		if !ok {
			t.Fatalf("no ./opt/clash/bin/clash in data.tar.gz; members: %v", mapKeysHdr(entries))
		}
		if hdr.Mode&0o777 != 0o755 {
			t.Errorf("mode = %o, want 0755", hdr.Mode&0o777)
		}
		if hdr.Size != int64(len(wantPayload)) {
			t.Errorf("size = %d, want %d", hdr.Size, len(wantPayload))
		}
		if _, ok := entries["./opt/clash/bin/"]; !ok {
			t.Error("missing parent dir entry ./opt/clash/bin/")
		}

		// postinst must land in control.tar.gz
		raw, _ := os.ReadFile(p.path)
		controlTar, err := extractMemberFromTar(raw, "control.tar.gz")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := extractMemberFromTar(controlTar, "postinst"); err != nil {
			t.Errorf("postinst missing from control.tar.gz: %v", err)
		}
	}

	// rebuild must be byte-identical (deterministic output)
	before, err := os.ReadFile(pkgs[0].path)
	if err != nil {
		t.Fatal(err)
	}
	pkgs2, err := binaryPackages(cfg, client, cache, src)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(pkgs2[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("rebuilt ipk differs; build is not deterministic")
	}
}

func mapKeysHdr(m map[string]*tar.Header) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBinaryPackagesValidation(t *testing.T) {
	dir := t.TempDir()
	client := newClient()
	cache, err := newCache(dir, client, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Layout: Layout{Versions: []string{"24.10.7"}, Branch: "24.10"}}

	cases := []struct {
		name string
		src  Source
	}{
		{"no install", Source{"type": "binary", "name": "x", "version": "1",
			"url": "http://e/x.gz", "arch": "all"}},
		{"relative install", Source{"type": "binary", "name": "x", "version": "1",
			"url": "http://e/x.gz", "install": "opt/x", "arch": "all"}},
		{"no version or repo", Source{"type": "binary", "name": "x",
			"url": "http://e/x.gz", "install": "/opt/x", "arch": "all"}},
		{"no arch or arch_map", Source{"type": "binary", "name": "x", "version": "1",
			"url": "http://e/x.gz", "install": "/opt/x"}},
		{"no url or asset_match", Source{"type": "binary", "name": "x", "version": "1",
			"install": "/opt/x", "arch": "all"}},
	}
	for _, c := range cases {
		if _, err := binaryPackages(cfg, client, cache, c.src); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

func TestBinaryControlExtras(t *testing.T) {
	src := Source{
		"depends":     []any{"libc", "kmod-tun"},
		"maintainer":  "me",
		"control":     map[string]any{"License": "MIT"},
		"description": "line1\nline2",
	}
	text := binaryControl(src, "pkg", "1.0-1", "all", 42)
	for _, want := range []string{
		"Package: pkg\n", "Version: 1.0-1\n", "Architecture: all\n",
		"Installed-Size: 42\n", "Depends: libc, kmod-tun\n",
		"Maintainer: me\n", "License: MIT\n", "Description: line1\n line2\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("control missing %q:\n%s", want, text)
		}
	}
}

func TestBinaryEndToEndIndex(t *testing.T) {
	// The built ipk must survive the real index path: buildPackagesIndex reads
	// its control back via readControl.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(gzipBytes(t, fmt.Appendf(nil, "binary for %s", r.URL.Path)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	client := newClient()
	cache, err := newCache(filepath.Join(dir, "cache"), client, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Layout: Layout{Versions: []string{"24.10.7"}, Branch: "24.10"}}
	src := Source{
		"type": "binary", "name": "tool", "version": "2.0",
		"url": srv.URL + "/tool-{arch}.gz", "install": "/usr/bin/tool",
		"arch_map": map[string]any{"x86_64": "amd64"},
	}
	pkgs, err := binaryPackages(cfg, client, cache, src)
	if err != nil {
		t.Fatal(err)
	}
	feedDir := filepath.Join(dir, "feed")
	if err := os.MkdirAll(feedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(pkgs[0].path, filepath.Join(feedDir, filepath.Base(pkgs[0].path))); err != nil {
		t.Fatal(err)
	}
	n, err := writeIndex(feedDir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("indexed %d packages, want 1", n)
	}
	data, err := os.ReadFile(filepath.Join(feedDir, "Packages"))
	if err != nil {
		t.Fatal(err)
	}
	stanzas := parseIndex(string(data))
	if len(stanzas) != 1 || stanzas[0]["Package"] != "tool" || stanzas[0]["SHA256sum"] == "" {
		t.Errorf("bad index:\n%s", data)
	}
}

func TestUpxOptions(t *testing.T) {
	cases := []struct {
		val     any
		flags   []string
		enabled bool
	}{
		{nil, nil, false},
		{false, nil, false},
		{true, []string{"--best", "--lzma"}, true},
		{"true", []string{"--best", "--lzma"}, true},
		{"false", nil, false},
		{"", nil, false},
		{"-9 --brute", []string{"-9", "--brute"}, true},
	}
	for _, c := range cases {
		src := Source{}
		if c.val != nil {
			src["upx"] = c.val
		}
		flags, enabled := upxOptions(src)
		if enabled != c.enabled {
			t.Errorf("upx=%v: enabled=%v, want %v", c.val, enabled, c.enabled)
		}
		if strings.Join(flags, " ") != strings.Join(c.flags, " ") {
			t.Errorf("upx=%v: flags=%v, want %v", c.val, flags, c.flags)
		}
	}
}

func TestUpxCompressMissingBinary(t *testing.T) {
	if _, err := exec.LookPath("upx"); err == nil {
		t.Skip("upx installed; this test covers the not-installed error")
	}
	cache := &Cache{dir: t.TempDir()}
	if _, err := upxCompress(cache, []byte("payload"), []string{"--best"}); err == nil {
		t.Fatal("expected error when upx is not in PATH")
	}
}
