package feedbuilder

// Source type "binary": download raw binaries / archives (e.g. GitHub release
// assets), wrap each into a locally built .ipk and hand it to the normal
// routing / indexing / signing pipeline. This turns "put this file from the
// internet at /path with mode X" into a regular package, so it works both with
// the ImageBuilder (PACKAGES=...) and with an ASU server (custom repository).
//
// Example — the mihomo binary SSClash expects at /opt/clash/bin/clash:
//
//	- type: binary
//	  name: mihomo
//	  repo: MetaCubeX/mihomo         # version comes from the release tag
//	  tag: latest                    # or a fixed tag like "v1.19.28"
//	  url: https://github.com/MetaCubeX/mihomo/releases/download/{tag}/mihomo-linux-{arch}-{tag}.gz
//	  install: /opt/clash/bin/clash
//	  mode: "0755"
//	  arch_map:                      # opkg architecture -> URL token
//	    mipsel_24kc: mipsle-softfloat
//	    aarch64_cortex-a53: arm64
//
// One .ipk is built per arch_map entry (skipping architectures excluded by the
// global `architectures:` filter). Placeholders available in url / asset_match
// / extract / install:
//
//	{version}   package version (explicit `version:`, else the tag without v)
//	{tag}       release tag verbatim (github mode)
//	{arch}      the arch_map VALUE for the entry being built (asset token)
//	{pkg_arch}  the arch_map KEY (opkg architecture)
//
// The asset is unpacked according to its file extension: .gz / .xz single
// files are decompressed; .tar[.gz/.xz] / .tgz / .txz / .zip archives need an
// `extract:` glob when they hold more than one file. Anything else is used
// as-is.
//
// `upx: true` compresses the unpacked binary with UPX before packaging
// (requires upx on the build host), shrinking Installed-Size — useful when a
// package barely misses the target's free overlay space. A string value passes
// custom flags instead of the default "--best --lzma". Note the executable
// unpacks itself into RAM at startup, so runtime memory grows by roughly the
// uncompressed size.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

// strMap returns the value of key as a string->string map (YAML mapping).
func (s Source) strMap(key string) map[string]string {
	v, ok := s[key]
	if !ok || v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, item := range m {
		out[k] = asString(item, "")
	}
	return out
}

// expandPlaceholders substitutes {name} placeholders from vars.
func expandPlaceholders(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// parseMode converts a config mode value into file mode bits. Strings are read
// as octal ("0755", "0o644"). Integers come pre-parsed by yaml.v3: a
// leading-zero literal (`mode: 0644`) is already octal-decoded, so small values
// are the mode bits themselves; a bare decimal like `mode: 755` (no leading
// zero) exceeds 0o777 and its decimal digits are the intended octal, so those
// are re-read digit-wise. Unusual modes (setuid/sticky etc.) should be quoted.
func parseMode(v any, def int64) (int64, error) {
	switch t := v.(type) {
	case nil:
		return def, nil
	case string:
		s := strings.TrimPrefix(strings.TrimPrefix(t, "0o"), "0O")
		if s == "" {
			return def, nil
		}
		n, err := strconv.ParseInt(s, 8, 32)
		if err != nil {
			return 0, fmt.Errorf("bad mode %q (expected octal like \"0755\")", t)
		}
		return n, nil
	case int:
		return parseModeInt(int64(t))
	case int64:
		return parseModeInt(t)
	case float64:
		return parseModeInt(int64(t))
	default:
		return 0, fmt.Errorf("bad mode value %v", v)
	}
}

func parseModeInt(n int64) (int64, error) {
	if n < 0 {
		return 0, fmt.Errorf("bad mode value %d", n)
	}
	if n <= 0o777 {
		return n, nil // yaml already octal-decoded a leading-zero literal
	}
	s := strconv.FormatInt(n, 10)
	if !strings.ContainsAny(s, "89") {
		if m, err := strconv.ParseInt(s, 8, 32); err == nil && m <= 0o7777 {
			return m, nil // bare decimal like 755: digits are the octal mode
		}
	}
	if n <= 0o7777 {
		return n, nil
	}
	return 0, fmt.Errorf("bad mode value %d (use a quoted octal string like \"0755\")", n)
}

// resolveBinaryRelease determines the version/tag for a binary source. With a
// `repo:`, the GitHub release (tag or latest) is queried and its tag becomes
// {tag}; the version defaults to the tag without a leading v. Without a repo,
// explicit `version:` (and optional `tag:`) are used verbatim. The release ID
// is returned so assets can be listed lazily (asset_match mode).
func resolveBinaryRelease(client *Client, src Source) (version, tag string, releaseID int64, err error) {
	version = src.strOr("version", "")
	tag = src.strOr("tag", "")
	repo := src.strOr("repo", "")

	if repo != "" {
		var api string
		if tag == "" || tag == "latest" {
			api = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
		} else {
			api = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
		}
		body, err := ghGet(client, api)
		if err != nil {
			return "", "", 0, err
		}
		release, err := decodeRelease(body)
		if err != nil {
			return "", "", 0, err
		}
		tag = release.TagName
		releaseID = release.ID
	}
	if version == "" {
		version = strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	}
	if version == "" {
		return "", "", 0, fmt.Errorf("binary source needs a `version:` or a `repo:`/`tag:` to derive one")
	}
	return version, tag, releaseID, nil
}

// binaryArches returns the (opkg arch -> asset token) pairs to build, honoring
// the global architectures filter. Without an arch_map, a single `arch:` (e.g.
// "all") is used with the arch itself as the token.
func binaryArches(cfg *Config, src Source) (map[string]string, error) {
	arches := src.strMap("arch_map")
	if len(arches) == 0 {
		if a := src.strOr("arch", ""); a != "" {
			arches = map[string]string{a: a}
		}
	}
	if len(arches) == 0 {
		return nil, fmt.Errorf("binary source needs an `arch_map:` (opkg arch -> asset token) or an `arch:`")
	}
	if len(cfg.Architectures) > 0 {
		filter := toSet(cfg.Architectures)
		for arch := range arches {
			if !filter[arch] && arch != "all" {
				delete(arches, arch)
			}
		}
	}
	return arches, nil
}

// binaryPackages builds one .ipk per architecture for a `type: binary` source
// and returns them as collected packages (path points at the locally built
// .ipk inside the cache's built/ directory).
func binaryPackages(cfg *Config, client *Client, cache *Cache, src Source) ([]collectedPkg, error) {
	pkg := src.strOr("package", src.strOr("name", ""))
	if pkg == "" {
		return nil, fmt.Errorf("binary source needs a `package:` or `name:`")
	}
	install := src.strOr("install", "")
	if install == "" || !strings.HasPrefix(install, "/") {
		return nil, fmt.Errorf("binary source needs an absolute `install:` path, got %q", install)
	}
	urlTemplate := src.strOr("url", "")
	assetMatch := src.strOr("asset_match", "")
	if urlTemplate == "" && assetMatch == "" {
		return nil, fmt.Errorf("binary source needs a `url:` template or `asset_match:` (with `repo:`)")
	}
	mode, err := parseMode(src["mode"], 0o755)
	if err != nil {
		return nil, err
	}

	version, tag, releaseID, err := resolveBinaryRelease(client, src)
	if err != nil {
		return nil, err
	}
	arches, err := binaryArches(cfg, src)
	if err != nil {
		return nil, err
	}

	// Assets are listed once, and only when asset_match needs them.
	var assets []ghAsset
	if urlTemplate == "" {
		repo := src.strOr("repo", "")
		if repo == "" {
			return nil, fmt.Errorf("`asset_match:` needs a `repo:`")
		}
		assets, err = githubReleaseAssets(client, repo, releaseID)
		if err != nil {
			return nil, err
		}
	}

	pkgVersion := version
	if revision := src.strOr("revision", "1"); revision != "" {
		pkgVersion += "-" + revision
	}

	builtDir := filepath.Join(cache.dir, "built")
	if err := os.MkdirAll(builtDir, 0o755); err != nil {
		return nil, err
	}

	var out []collectedPkg
	for _, arch := range sortedKeys(toStrSet(arches)) {
		token := arches[arch]
		vars := map[string]string{
			"version": version, "tag": tag, "arch": token, "pkg_arch": arch,
		}

		assetURL := expandPlaceholders(urlTemplate, vars)
		if assetURL == "" {
			pattern := expandPlaceholders(assetMatch, vars)
			for _, a := range assets {
				if fnmatch(pattern, a.Name) {
					assetURL = a.BrowserDownloadURL
					break
				}
			}
			if assetURL == "" {
				return nil, fmt.Errorf("no release asset matched %q for arch %s", pattern, arch)
			}
		}

		rawPath, err := cache.get(remoteFile{url: assetURL})
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", assetURL, err)
		}
		raw, err := os.ReadFile(rawPath)
		if err != nil {
			return nil, err
		}
		assetName := lastPathPart(stripQuery(assetURL))
		payload, err := unpackAsset(assetName, raw, expandPlaceholders(src.strOr("extract", ""), vars))
		if err != nil {
			return nil, fmt.Errorf("unpack %s: %w", assetName, err)
		}

		if upxFlags, enabled := upxOptions(src); enabled {
			payload, err = upxCompress(cache, payload, upxFlags)
			if err != nil {
				return nil, fmt.Errorf("upx %s (%s): %w", pkg, arch, err)
			}
		}

		control := binaryControl(src, pkg, pkgVersion, arch, len(payload))
		scripts := map[string]string{}
		if s := src.strOr("postinst", ""); s != "" {
			scripts["postinst"] = s
		}
		if s := src.strOr("prerm", ""); s != "" {
			scripts["prerm"] = s
		}

		dest := filepath.Join(builtDir, safeName(pkg, pkgVersion, arch))
		installPath := expandPlaceholders(install, vars)
		if err := buildIPK(dest, control, scripts, installPath, mode, payload); err != nil {
			return nil, err
		}
		out = append(out, collectedPkg{
			url:          assetURL,
			path:         dest,
			feedOverride: src.strOr("feed", ""),
			sourceTarget: src.strOr("target", ""),
			kmodVersion:  src.strOr("kmod_version", ""),
		})
	}
	return out, nil
}

// toStrSet builds a lookup set from a string map's keys.
func toStrSet(m map[string]string) map[string]bool {
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return set
}

// binaryControl renders the control file for a built package. Standard fields
// come first, then any extra `control:` mapping entries (sorted), Description
// last with continuation lines indented.
func binaryControl(src Source, pkg, version, arch string, installedSize int) string {
	var b strings.Builder
	field := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	field("Package", pkg)
	field("Version", version)
	field("Architecture", arch)
	field("Installed-Size", strconv.Itoa(installedSize))
	field("Section", src.strOr("section", "utils"))
	field("Maintainer", src.strOr("maintainer", ""))
	field("Depends", strings.Join(src.strSlice("depends"), ", "))
	field("Provides", strings.Join(src.strSlice("provides"), ", "))
	field("Conflicts", strings.Join(src.strSlice("conflicts"), ", "))

	extra := src.strMap("control")
	for _, k := range sortedKeys(toStrSet(extra)) {
		field(k, extra[k])
	}

	desc := src.strOr("description", fmt.Sprintf("%s (repacked binary)", pkg))
	field("Description", strings.ReplaceAll(strings.TrimRight(desc, "\n"), "\n", "\n "))
	return b.String()
}

// unpackAsset turns a downloaded asset into the file payload to install,
// dispatching on the asset's file extension. For archives, extract is a glob
// matched against each member's full path and base name; when empty, an
// archive with exactly one regular file unpacks implicitly.
func unpackAsset(name string, data []byte, extract string) ([]byte, error) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"),
		strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"),
		strings.HasSuffix(lower, ".tar"):
		return extractFromTar(data, extract)
	case strings.HasSuffix(lower, ".zip"):
		return extractFromZip(data, extract)
	case strings.HasSuffix(lower, ".gz"):
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		return io.ReadAll(gz)
	case strings.HasSuffix(lower, ".xz"):
		xr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(xr)
	default:
		return data, nil
	}
}

// upxOptions reads the `upx:` setting: false/absent disables, true uses the
// default flags, a string supplies custom flags (whitespace-separated).
func upxOptions(src Source) ([]string, bool) {
	if b, ok := yamlBool(src["upx"]); ok {
		if !b {
			return nil, false
		}
		return []string{"--best", "--lzma"}, true
	}
	if v, ok := src["upx"].(string); ok && v != "" {
		return strings.Fields(v), true
	}
	return nil, false
}

// upxCompress runs upx over payload and returns the packed executable. Results
// are cached under <cache>/upx keyed by the payload hash and flags, since UPX
// with --lzma over a large binary takes noticeable time on every build.
func upxCompress(cache *Cache, payload []byte, flags []string) ([]byte, error) {
	if _, err := exec.LookPath("upx"); err != nil {
		return nil, fmt.Errorf("`upx: true` set but upx not found in PATH")
	}

	sum := sha256.Sum256(append(payload, []byte("\x00"+strings.Join(flags, " "))...))
	upxDir := filepath.Join(cache.dir, "upx")
	cached := filepath.Join(upxDir, hex.EncodeToString(sum[:]))
	if data, err := os.ReadFile(cached); err == nil {
		return data, nil
	}
	if err := os.MkdirAll(upxDir, 0o755); err != nil {
		return nil, err
	}

	tmp := cached + ".work"
	if err := os.WriteFile(tmp, payload, 0o755); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	cmd := exec.Command("upx", append(append([]string{"-q"}, flags...), tmp)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	packed, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, cached); err != nil {
		return nil, err
	}
	return packed, nil
}

// memberMatches reports whether an archive member matches the extract glob
// (against the full slash path or the base name alone).
func memberMatches(pattern, member string) bool {
	member = strings.TrimPrefix(path.Clean(member), "./")
	return fnmatch(pattern, member) || fnmatch(pattern, path.Base(member))
}

func extractFromTar(blob []byte, extract string) ([]byte, error) {
	tr, err := openTar(blob)
	if err != nil {
		return nil, err
	}
	var found []byte
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		names = append(names, hdr.Name)
		if extract != "" && !memberMatches(extract, hdr.Name) {
			continue
		}
		if found != nil {
			return nil, ambiguousArchive(extract, names)
		}
		if found, err = io.ReadAll(tr); err != nil {
			return nil, err
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no archive member matched %q (members: %s)",
			extract, strings.Join(names, ", "))
	}
	return found, nil
}

func extractFromZip(data []byte, extract string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var found []byte
	var names []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		names = append(names, f.Name)
		if extract != "" && !memberMatches(extract, f.Name) {
			continue
		}
		if found != nil {
			return nil, ambiguousArchive(extract, names)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		found, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no archive member matched %q (members: %s)",
			extract, strings.Join(names, ", "))
	}
	return found, nil
}

func ambiguousArchive(extract string, names []string) error {
	if extract == "" {
		return fmt.Errorf("archive holds several files; set `extract:` to pick one (members: %s)",
			strings.Join(names, ", "))
	}
	return fmt.Errorf("`extract: %s` matched several members (%s); make it more specific",
		extract, strings.Join(names, ", "))
}

// ipkEpoch is the fixed timestamp used in generated archives so rebuilding an
// unchanged package yields a byte-identical .ipk (the dedup/collision logic in
// cmdBuild compares content hashes).
var ipkEpoch = time.Unix(0, 0)

// buildIPK writes an OpenWrt .ipk (outer gzipped tar holding debian-binary,
// control.tar.gz and data.tar.gz — the layout readControl already accepts)
// installing payload at installPath with the given mode.
func buildIPK(dest, control string, scripts map[string]string, installPath string, mode int64, payload []byte) error {
	dataTar, err := tarGzPayload(installPath, mode, payload)
	if err != nil {
		return err
	}

	controlEntries := []tarEntry{{name: "./control", mode: 0o644, data: []byte(control)}}
	for _, name := range sortedKeys(toSet(mapKeys(scripts))) {
		controlEntries = append(controlEntries,
			tarEntry{name: "./" + name, mode: 0o755, data: []byte(scripts[name])})
	}
	controlTar, err := tarGz(controlEntries)
	if err != nil {
		return err
	}

	outer, err := tarGz([]tarEntry{
		{name: "./debian-binary", mode: 0o644, data: []byte("2.0\n")},
		{name: "./control.tar.gz", mode: 0o644, data: controlTar},
		{name: "./data.tar.gz", mode: 0o644, data: dataTar},
	})
	if err != nil {
		return err
	}
	return os.WriteFile(dest, outer, 0o644)
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type tarEntry struct {
	name string
	mode int64
	dir  bool
	data []byte
}

// tarGzPayload builds data.tar.gz: the parent directories of installPath
// (0755) followed by the file itself.
func tarGzPayload(installPath string, mode int64, payload []byte) ([]byte, error) {
	clean := path.Clean("/" + strings.TrimPrefix(installPath, "/"))
	segs := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	entries := []tarEntry{{name: "./", mode: 0o755, dir: true}}
	for i := 1; i < len(segs); i++ {
		entries = append(entries, tarEntry{
			name: "./" + strings.Join(segs[:i], "/") + "/", mode: 0o755, dir: true,
		})
	}
	entries = append(entries, tarEntry{name: "." + clean, mode: mode, data: payload})
	return tarGz(entries)
}

// tarGz renders entries into a deterministic gzipped tar (root ownership,
// fixed epoch timestamps, no gzip header metadata).
func tarGz(entries []tarEntry) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:    e.name,
			Mode:    e.mode,
			Uid:     0,
			Gid:     0,
			Uname:   "root",
			Gname:   "root",
			ModTime: ipkEpoch,
			Format:  tar.FormatGNU,
		}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if !e.dir {
			if _, err := tw.Write(e.data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
