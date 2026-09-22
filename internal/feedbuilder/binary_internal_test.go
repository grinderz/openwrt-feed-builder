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
	t.Parallel()

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
	for _, tcase := range cases {
		got, err := parseMode(tcase.in, 0o755)
		if err != nil {
			t.Fatalf("parseMode(%v): %v", tcase.in, err)
		}

		if got != tcase.want {
			t.Errorf("parseMode(%v) = %o, want %o", tcase.in, got, tcase.want)
		}
	}

	if _, err := parseMode("rwxr-xr-x", 0o755); err == nil {
		t.Error("parseMode should reject non-octal strings")
	}
}

func TestExpandPlaceholders(t *testing.T) {
	t.Parallel()

	got := expandPlaceholders("x-{arch}-v{version}.gz", map[string]string{
		tKeyArch: tAssetArm64, tKeyVersion: tVersion123,
	})
	if got != "x-arm64-v1.2.3.gz" {
		t.Errorf("got %q", got)
	}
}

func TestUnpackAssetGzip(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

	gzw := gzip.NewWriter(&buf)

	tarWriter := tar.NewWriter(gzw)
	for name, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestUnpackAssetTarExtract(t *testing.T) {
	t.Parallel()

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
	single := tarGzArchive(t, map[string]string{tKeyTool: "solo"})

	out, err = unpackAsset("tool.tgz", single, "")
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "solo" {
		t.Errorf("got %q", out)
	}
}

func TestUnpackAssetZip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	zipWriter := zip.NewWriter(&buf)
	for name, content := range map[string]string{"a/tool.exe": "win", "a/tool": "nix"} {
		w, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := zipWriter.Close(); err != nil {
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

	tarReader, err := openTar(dataTar)
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]*tar.Header{}

	for {
		hdr, err := tarReader.Next()
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
	t.Parallel()

	binaries := map[string][]byte{
		tAssetMipsle: []byte("mips binary"),
		tAssetArm64:  []byte("arm binary"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		for arch, data := range binaries {
			if strings.Contains(req.URL.Path, arch) {
				_, _ = writer.Write(gzipBytes(t, data))
				return
			}
		}

		http.NotFound(writer, req)
	}))
	defer srv.Close()

	dir := t.TempDir()
	client := newClient()

	cache, err := newCache(filepath.Join(dir, "cache"), client, false)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Architectures: []string{tArchMipsel, tArchA53},
		Layout:        Layout{Versions: []string{tRel24107}, DefaultFeed: tFeedPackages},
	}
	src := Source{
		tKeyType:    tTypeBinary,
		tKeyName:    tPkgMihomo,
		tKeyVersion: "1.19.28",
		tKeyURL:     srv.URL + "/v{version}/mihomo-linux-{arch}-v{version}.gz",
		tKeyInstall: "/opt/clash/bin/clash",
		"mode":      "0755",
		"section":   "net",
		tKeyArchMap: map[string]any{
			tArchMipsel: tAssetMipsle,
			tArchA53:    tAssetArm64,
			tArchX86:    "amd64", // filtered out by cfg.Architectures
		},
		"description": "mihomo binary",
		"postinst":    "#!/bin/sh\nexit 0\n",
	}

	pkgs, err := binaryPackages(t.Context(), cfg, client, cache, src)
	if err != nil {
		t.Fatal(err)
	}

	if len(pkgs) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(pkgs))
	}

	for _, built := range pkgs {
		control, err := readControl(built.path)
		if err != nil {
			t.Fatalf("readControl(%s): %v", built.path, err)
		}

		fields := parseFields(control)
		if fields[tFieldPackage] != tPkgMihomo {
			t.Errorf("Package = %q", fields[tFieldPackage])
		}

		if fields["Version"] != "1.19.28-1" {
			t.Errorf("Version = %q", fields["Version"])
		}

		if fields["Section"] != "net" {
			t.Errorf("Section = %q", fields["Section"])
		}

		arch := fields["Architecture"]

		wantPayload := binaries[map[string]string{
			tArchMipsel: tAssetMipsle, tArchA53: tAssetArm64,
		}[arch]]
		if wantPayload == nil {
			t.Fatalf("unexpected arch %q", arch)
		}

		entries := readDataEntries(t, built.path)

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
		raw, _ := os.ReadFile(built.path)

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

	pkgs2, err := binaryPackages(t.Context(), cfg, client, cache, src)
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
	t.Parallel()

	dir := t.TempDir()
	client := newClient()

	cache, err := newCache(dir, client, false)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Layout: Layout{Versions: []string{tRel24107}}}

	cases := []struct {
		name string
		src  Source
	}{
		{"no install", Source{
			tKeyType: tTypeBinary, tKeyName: "x", tKeyVersion: "1",
			tKeyURL: tAssetURL, tKeyArch: tArchAll,
		}},
		{"relative install", Source{
			tKeyType: tTypeBinary, tKeyName: "x", tKeyVersion: "1",
			tKeyURL: tAssetURL, tKeyInstall: "opt/x", tKeyArch: tArchAll,
		}},
		{"no version or repo", Source{
			tKeyType: tTypeBinary, tKeyName: "x",
			tKeyURL: tAssetURL, tKeyInstall: tInstallX, tKeyArch: tArchAll,
		}},
		{"no arch or arch_map", Source{
			tKeyType: tTypeBinary, tKeyName: "x", tKeyVersion: "1",
			tKeyURL: tAssetURL, tKeyInstall: tInstallX,
		}},
		{"no url or asset_match", Source{
			tKeyType: tTypeBinary, tKeyName: "x", tKeyVersion: "1",
			tKeyInstall: tInstallX, tKeyArch: tArchAll,
		}},
	}
	for _, c := range cases {
		if _, err := binaryPackages(t.Context(), cfg, client, cache, c.src); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

func TestBinaryControlExtras(t *testing.T) {
	t.Parallel()

	src := Source{
		"depends":     []any{tDepLibc, tDepKmodTun},
		"maintainer":  "me",
		"control":     map[string]any{"License": tLicenseMIT},
		"description": "line1\nline2",
	}

	text := binaryControl(src, "pkg", "1.0-1", tArchAll, 42)
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
	t.Parallel()

	// The built ipk must survive the real index path: buildPackagesIndex reads
	// its control back via readControl.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzipBytes(t, fmt.Appendf(nil, "binary for %s", r.URL.Path)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	client := newClient()

	cache, err := newCache(filepath.Join(dir, "cache"), client, false)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Layout: Layout{Versions: []string{tRel24107}}}
	src := Source{
		tKeyType: tTypeBinary, tKeyName: tKeyTool, tKeyVersion: "2.0",
		tKeyURL: srv.URL + "/tool-{arch}.gz", tKeyInstall: "/usr/bin/tool",
		tKeyArchMap: map[string]any{tArchX86: "amd64"},
	}

	pkgs, err := binaryPackages(t.Context(), cfg, client, cache, src)
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

	count, err := writeIndex(t.Context(), feedDir, "", "")
	if err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Fatalf("indexed %d packages, want 1", count)
	}

	data, err := os.ReadFile(filepath.Join(feedDir, "Packages"))
	if err != nil {
		t.Fatal(err)
	}

	stanzas := parseIndex(string(data))
	if len(stanzas) != 1 || stanzas[0][tFieldPackage] != tKeyTool || stanzas[0]["SHA256sum"] == "" {
		t.Errorf("bad index:\n%s", data)
	}
}

func TestInsertIndexFields(t *testing.T) {
	t.Parallel()

	fields := "Filename: a_1.0_all.ipk\nSize: 1\n"

	// opkg drops fields after a multi-line Description, so the index
	// fields must land before it
	control := "Package: a\nVersion: 1.0\nDescription:  first line\n second line\n"
	got := insertIndexFields(control, fields)

	want := "Package: a\nVersion: 1.0\nFilename: a_1.0_all.ipk\nSize: 1\nDescription:  first line\n second line\n"
	if got != want {
		t.Errorf("with description:\ngot:\n%q\nwant:\n%q", got, want)
	}

	// no Description at all: fields go last
	control = "Package: a\nVersion: 1.0\n"
	got = insertIndexFields(control, fields)

	want = "Package: a\nVersion: 1.0\nFilename: a_1.0_all.ipk\nSize: 1\n"
	if got != want {
		t.Errorf("without description:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestStripIndexFields(t *testing.T) {
	t.Parallel()

	// SourceName / SourceDateEpoch parse as a repeated Source field on the
	// router (opkg matches field names by prefix) and corrupt opkg's blob
	// buffer, so none of the Source* fields may reach the index.
	control := "Package: kmod-x\n" +
		"Version: 1.0\n" +
		"Source: feeds/base/kmod-x\n" +
		"SourceName: kmod-x\n" +
		"Maintainer: Some One\n" +
		" continued maintainer line\n" +
		"Section: kernel\n" +
		"SourceDateEpoch: 1779897308\n" +
		"Description:  first line\n second line\n"

	want := "Package: kmod-x\n" +
		"Version: 1.0\n" +
		"Section: kernel\n" +
		"Description:  first line\n second line\n"
	if got := stripIndexFields(control); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestUpxOptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		val     any
		flags   []string
		enabled bool
	}{
		{nil, nil, false},
		{false, nil, false},
		{true, []string{tUPXBest, "--lzma"}, true},
		{"true", []string{tUPXBest, "--lzma"}, true},
		{"false", nil, false},
		{"", nil, false},
		{"-9 --brute", []string{"-9", "--brute"}, true},
	}
	for _, tcase := range cases {
		src := Source{}
		if tcase.val != nil {
			src["upx"] = tcase.val
		}

		flags, enabled := upxOptions(src)
		if enabled != tcase.enabled {
			t.Errorf("upx=%v: enabled=%v, want %v", tcase.val, enabled, tcase.enabled)
		}

		if strings.Join(flags, " ") != strings.Join(tcase.flags, " ") {
			t.Errorf("upx=%v: flags=%v, want %v", tcase.val, flags, tcase.flags)
		}
	}
}

func TestUpxCompressMissingBinary(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("upx"); err == nil {
		t.Skip("upx installed; this test covers the not-installed error")
	}

	cache := &Cache{dir: t.TempDir()}
	if _, err := upxCompress(t.Context(), cache, []byte("payload"), []string{tUPXBest}); err == nil {
		t.Fatal("expected error when upx is not in PATH")
	}
}

// With an opkg and an apk branch carried, a binary source is packaged in both
// formats; `openwrt:` narrows it to one.
func TestBinaryPackagesBothFormats(t *testing.T) {
	t.Parallel()

	tool := requireAPK(t, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(gzipBytes(t, []byte("arm binary")))
	}))
	defer srv.Close()

	dir := t.TempDir()
	client := newClient()

	cache, err := newCache(filepath.Join(dir, "cache"), client, false)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Layout:  Layout{Versions: []string{tRel24108, tRel25125}, DefaultFeed: tFeedPackages},
		APKTool: tool,
	}
	src := Source{
		tKeyType: tTypeBinary, tKeyName: tPkgMihomo, tKeyVersion: "1.19.28",
		tKeyURL:     srv.URL + "/mihomo-{arch}.gz",
		tKeyInstall: "/opt/clash/bin/clash",
		tKeyArchMap: map[string]any{tArchA53: tAssetArm64},
		"depends":   []any{tDepKmodTun},
	}

	pkgs, err := binaryPackages(t.Context(), cfg, client, cache, src)
	if err != nil {
		t.Fatal(err)
	}

	versions := map[string]string{}

	for _, p := range pkgs {
		fields, format, err := readPkgFields(p.path)
		if err != nil {
			t.Fatal(err)
		}

		versions[format] = fields["Version"]
		if fields[tFieldDepends] != tDepKmodTun {
			t.Errorf("%s Depends = %q", format, fields[tFieldDepends])
		}
	}

	if versions[formatIPK] != "1.19.28-1" || versions[formatAPK] != "1.19.28-r1" {
		t.Errorf("versions per format = %v", versions)
	}

	src["openwrt"] = tBranch25

	pkgs, err = binaryPackages(t.Context(), cfg, client, cache, src)
	if err != nil {
		t.Fatal(err)
	}

	if len(pkgs) != 1 || pkgs[0].branch != tBranch25 || !strings.HasSuffix(pkgs[0].path, ".apk") {
		t.Errorf("openwrt: 25.12 should build only the .apk, got %+v", pkgs)
	}

	src["version"] = "1.19.28-beta"
	if _, err := binaryPackages(t.Context(), cfg, client, cache, src); err == nil {
		t.Error("an invalid apk version must be rejected for apk branches")
	}
}
