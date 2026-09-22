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
// One package is built per arch_map entry (skipping architectures excluded by
// the global `architectures:` filter) and per package format the carried
// branches need: an .ipk for opkg branches (24.10 and older), an .apk for apk
// branches (25.12+, built with apk mkpkg under fakeroot; version
// <version>-r<revision>, `control:` passes only license/url/origin through).
// `openwrt:` / `kmod_version:` on the source narrow it to that branch's format.
// Placeholders available in url / asset_match / extract / install:
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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
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

	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}

	out := make(map[string]string, len(raw))
	for k, item := range raw {
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
func parseMode(val any, def int64) (int64, error) {
	switch typed := val.(type) {
	case nil:
		return def, nil
	case string:
		s := strings.TrimPrefix(strings.TrimPrefix(typed, "0o"), "0O")
		if s == "" {
			return def, nil
		}

		n, err := strconv.ParseInt(s, 8, 32)
		if err != nil {
			return 0, fmt.Errorf("%w %q (expected octal like \"0755\")", errBadMode, typed)
		}

		return n, nil
	case int:
		return parseModeInt(int64(typed))
	case int64:
		return parseModeInt(typed)
	case float64:
		return parseModeInt(int64(typed))
	default:
		return 0, fmt.Errorf("%w value %v", errBadMode, val)
	}
}

// Mode bounds: permission bits alone, and with setuid/setgid/sticky.
const (
	modePermMax = 0o777
	modeMax     = 0o7777
)

func parseModeInt(mode int64) (int64, error) {
	if mode < 0 {
		return 0, fmt.Errorf("%w value %d", errBadMode, mode)
	}

	if mode <= modePermMax {
		return mode, nil // yaml already octal-decoded a leading-zero literal
	}

	s := strconv.FormatInt(mode, 10)
	if !strings.ContainsAny(s, "89") {
		if m, err := strconv.ParseInt(s, 8, 32); err == nil && m <= modeMax {
			return m, nil // bare decimal like 755: digits are the octal mode
		}
	}

	if mode <= modeMax {
		return mode, nil
	}

	return 0, fmt.Errorf("%w value %d (use a quoted octal string like \"0755\")", errBadMode, mode)
}

// resolveBinaryRelease determines the version/tag for a binary source. With a
// `repo:`, the GitHub release (tag or latest) is queried and its tag becomes
// {tag}; the version defaults to the tag without a leading v. Without a repo,
// explicit `version:` (and optional `tag:`) are used verbatim. The release ID
// is returned so assets can be listed lazily (asset_match mode).
// binaryRelease is what a binary source resolved to: the package version, the
// release tag ({tag}) and the GitHub release id (0 without a repo).
type binaryRelease struct {
	version, tag string
	id           int64
}

func resolveBinaryRelease(ctx context.Context, client *Client, src Source) (binaryRelease, error) {
	version := src.strOr("version", "")
	tag := src.strOr("tag", "")

	var releaseID int64

	repo := src.strOr("repo", "")

	if repo != "" {
		var api string
		if tag == "" || tag == "latest" {
			api = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
		} else {
			api = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
		}

		body, err := ghGet(ctx, client, api)
		if err != nil {
			return binaryRelease{}, err
		}

		release, err := decodeRelease(body)
		if err != nil {
			return binaryRelease{}, err
		}

		tag = release.TagName
		releaseID = release.ID
	}

	if version == "" {
		version = strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	}

	if version == "" {
		return binaryRelease{}, fmt.Errorf("%w needs a `version:` or a `repo:`/`tag:` to derive one", errBinarySource)
	}

	return binaryRelease{version: version, tag: tag, id: releaseID}, nil
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
		return nil, fmt.Errorf("%w needs an `arch_map:` (opkg arch -> asset token) or an `arch:`", errBinarySource)
	}

	if len(cfg.Architectures) > 0 {
		filter := toSet(cfg.Architectures)
		for arch := range arches {
			if !filter[arch] && arch != archAll {
				delete(arches, arch)
			}
		}
	}

	return arches, nil
}

// binaryPackages builds one .ipk per architecture for a `type: binary` source
// and returns them as collected packages (path points at the locally built
// .ipk inside the cache's built/ directory).
func binaryPackages(
	ctx context.Context, cfg *Config, client *Client, cache *Cache, src Source,
) ([]collectedPkg, error) {
	job, err := newBinaryJob(ctx, cfg, client, cache, src)
	if err != nil {
		return nil, err
	}

	var out []collectedPkg

	for _, arch := range sortedKeys(toStrSet(job.arches)) {
		pkgs, err := job.buildArch(ctx, arch)
		if err != nil {
			return nil, err
		}

		out = append(out, pkgs...)
	}

	return out, nil
}

// binaryJob is one validated binary source, resolved to a release.
type binaryJob struct {
	cfg        *Config
	cache      *Cache
	src        Source
	pkg        string
	install    string
	urlTmpl    string
	assetMatch string
	mode       int64
	version    string
	tag        string
	pkgVersion string // opkg: <version>-<revision>
	apkVersion string // apk: <version>-r<revision>
	arches     map[string]string
	assets     []ghAsset
	formats    []string
	builtDir   string
}

func newBinaryJob(ctx context.Context, cfg *Config, client *Client, cache *Cache, src Source) (*binaryJob, error) {
	job := &binaryJob{
		cfg: cfg, cache: cache, src: src,
		pkg:        src.strOr("package", src.strOr("name", "")),
		install:    src.strOr("install", ""),
		urlTmpl:    src.strOr("url", ""),
		assetMatch: src.strOr("asset_match", ""),
		builtDir:   filepath.Join(cache.dir, "built"),
	}

	if job.pkg == "" {
		return nil, fmt.Errorf("%w needs a `package:` or `name:`", errBinarySource)
	}

	if job.install == "" || !strings.HasPrefix(job.install, "/") {
		return nil, fmt.Errorf("%w needs an absolute `install:` path, got %q", errBinarySource, job.install)
	}

	if job.urlTmpl == "" && job.assetMatch == "" {
		return nil, fmt.Errorf("%w needs a `url:` template or `asset_match:` (with `repo:`)", errBinarySource)
	}

	var err error
	if job.mode, err = parseMode(src["mode"], execPerm); err != nil {
		return nil, err
	}

	release, err := resolveBinaryRelease(ctx, client, src)
	if err != nil {
		return nil, err
	}

	job.version, job.tag = release.version, release.tag

	if job.arches, err = binaryArches(cfg, src); err != nil {
		return nil, err
	}

	// Assets are listed once, and only when asset_match needs them.
	if job.urlTmpl == "" {
		repo := src.strOr("repo", "")
		if repo == "" {
			return nil, fmt.Errorf("%w: `asset_match:` needs a `repo:`", errBinarySource)
		}

		if job.assets, err = githubReleaseAssets(ctx, client, repo, release.id); err != nil {
			return nil, err
		}
	}

	job.pkgVersion, job.apkVersion = job.version, job.version
	if revision := src.strOr("revision", "1"); revision != "" {
		job.pkgVersion += "-" + revision
		job.apkVersion += "-r" + revision
	}

	job.formats = binaryFormats(cfg, src)
	for _, f := range job.formats {
		if f == formatAPK && !apkVersionRE.MatchString(job.apkVersion) {
			return nil, fmt.Errorf("%w: version %q is not a valid apk version (25.12+ "+
				"branches); set an explicit `version:` like \"1.2.3\"", errBinarySource, job.apkVersion)
		}
	}

	if err := os.MkdirAll(job.builtDir, dirPerm); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	return job, nil
}

// buildArch fetches and unpacks the asset of one architecture and packages it
// in every format the job needs.
func (job *binaryJob) buildArch(ctx context.Context, arch string) ([]collectedPkg, error) {
	vars := map[string]string{
		"version": job.version, "tag": job.tag, "arch": job.arches[arch], "pkg_arch": arch,
	}

	assetURL, err := job.assetURL(vars, arch)
	if err != nil {
		return nil, err
	}

	rawPath, err := job.cache.get(ctx, remoteFile{url: assetURL})
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", assetURL, err)
	}

	raw, err := os.ReadFile(rawPath)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	assetName := lastPathPart(stripQuery(assetURL))

	payload, err := unpackAsset(assetName, raw, expandPlaceholders(job.src.strOr("extract", ""), vars))
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", assetName, err)
	}

	if upxFlags, enabled := upxOptions(job.src); enabled {
		payload, err = upxCompress(ctx, job.cache, payload, upxFlags)
		if err != nil {
			return nil, fmt.Errorf("upx %s (%s): %w", job.pkg, arch, err)
		}
	}

	installPath := expandPlaceholders(job.install, vars)

	out := make([]collectedPkg, 0, len(job.formats))

	for _, format := range job.formats {
		dest, err := job.packageAs(ctx, format, arch, installPath, payload)
		if err != nil {
			return nil, err
		}

		out = append(out, collectedPkg{
			url:          assetURL,
			path:         dest,
			feedOverride: job.src.strOr("feed", ""),
			sourceTarget: job.src.strOr("target", ""),
			kmodVersion:  job.src.strOr("kmod_version", ""),
			branch:       job.src.strOr("openwrt", ""),
		})
	}

	return out, nil
}

// assetURL is the download URL of one architecture: the url template, or the
// release asset matching asset_match.
func (job *binaryJob) assetURL(vars map[string]string, arch string) (string, error) {
	if assetURL := expandPlaceholders(job.urlTmpl, vars); assetURL != "" {
		return assetURL, nil
	}

	pattern := expandPlaceholders(job.assetMatch, vars)
	for _, asset := range job.assets {
		if fnmatch(pattern, asset.Name) {
			return asset.BrowserDownloadURL, nil
		}
	}

	return "", fmt.Errorf("%w: no release asset matched %q for arch %s", errBinarySource, pattern, arch)
}

// packageAs builds the payload into a package of one format and returns its path.
func (job *binaryJob) packageAs(ctx context.Context, format, arch, installPath string, payload []byte) (string, error) {
	dest := filepath.Join(job.builtDir, safeName(job.pkg, job.pkgVersion, arch))
	if format == formatAPK {
		dest = strings.TrimSuffix(dest, ".ipk") + ".apk"
	}

	job.cache.mark(dest)

	if format == formatAPK {
		if err := job.cfg.APKTool.check(ctx); err != nil {
			return "", err
		}

		spec := binaryAPKSpec(job.src, job.pkg, job.apkVersion, arch, installPath, job.mode, payload)

		return dest, job.cfg.APKTool.mkpkg(ctx, dest, spec)
	}

	control := binaryControl(job.src, job.pkg, job.pkgVersion, arch, len(payload))

	scripts := map[string]string{}
	if s := job.src.strOr("postinst", ""); s != "" {
		scripts["postinst"] = s
	}

	if s := job.src.strOr("prerm", ""); s != "" {
		scripts["prerm"] = s
	}

	return dest, buildIPK(dest, control, scripts, installPath, job.mode, payload)
}

// binaryFormats returns the package formats a binary source is built in:
// those of the carried branches, narrowed by the source's `kmod_version:` or
// `openwrt:` branch to that one branch's format.
func binaryFormats(cfg *Config, src Source) []string {
	if r := src.strOr("kmod_version", ""); r != "" {
		return []string{branchFormat(releaseBranch(r))}
	}

	if b := src.strOr("openwrt", ""); b != "" {
		return []string{branchFormat(b)}
	}

	return cfg.Layout.formats()
}

// opkgDepRE matches a versioned opkg dependency, e.g. "foo (>= 1.2)".
var opkgDepRE = regexp.MustCompile(`^(\S+)\s*\(\s*([<>=]+)\s*([^)\s]+)\s*\)$`)

// apkDep converts an opkg-style dependency into apk syntax:
// "foo (>= 1.2)" -> "foo>=1.2", "foo (<< 2)" -> "foo<2".
func apkDep(dep string) string {
	dep = strings.TrimSpace(dep)

	match := opkgDepRE.FindStringSubmatch(dep)
	if match == nil {
		return dep
	}

	op := strings.NewReplacer(">>", ">", "<<", "<").Replace(match[2])

	return match[1] + op + match[3]
}

// binaryAPKSpec maps a binary source onto an apk mkpkg package description.
// opkg maintainer scripts become their apk counterparts (postinst ->
// post-install, prerm -> pre-deinstall).
func binaryAPKSpec(src Source, pkg, version, arch, installPath string, mode int64, payload []byte) apkPkgSpec {
	depends := make([]string, 0, len(src.strSlice("depends")))

	provides := make([]string, 0, len(src.strSlice("provides")))
	for _, d := range src.strSlice("depends") {
		depends = append(depends, apkDep(d))
	}

	for _, p := range src.strSlice("provides") {
		provides = append(provides, apkDep(p))
	}

	info := map[string]string{}
	if m := src.strOr("maintainer", ""); m != "" {
		info["maintainer"] = m
	}

	for k, v := range src.strMap("control") {
		switch lk := strings.ToLower(k); lk {
		case "license", "url", "origin":
			info[lk] = v
		}
	}

	scripts := map[string]string{}
	if s := src.strOr("postinst", ""); s != "" {
		scripts["post-install"] = s
	}

	if s := src.strOr("prerm", ""); s != "" {
		scripts["pre-deinstall"] = s
	}

	desc := src.strOr("description", pkg+" (repacked binary)")

	return apkPkgSpec{
		name: pkg, version: version, arch: arch,
		description: strings.Join(strings.Fields(desc), " "),
		depends:     depends, provides: provides,
		info: info, scripts: scripts,
		installPath: installPath, mode: mode, payload: payload,
	}
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
	var out strings.Builder

	field := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&out, "%s: %s\n", k, v)
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

	desc := src.strOr("description", pkg+" (repacked binary)")
	field("Description", strings.ReplaceAll(strings.TrimRight(desc, "\n"), "\n", "\n "))

	return out.String()
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
		gzr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer closeQuietly(gzr)

		return readAll(gzr)
	case strings.HasSuffix(lower, ".xz"):
		xr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("xz: %w", err)
		}

		return readAll(xr)
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
func upxCompress(ctx context.Context, cache *Cache, payload []byte, flags []string) ([]byte, error) {
	if _, err := exec.LookPath("upx"); err != nil {
		return nil, fmt.Errorf("%w: `upx: true` set but upx not found in PATH", errUPX)
	}

	sum := sha256.Sum256(append(payload, []byte("\x00"+strings.Join(flags, " "))...))
	upxDir := filepath.Join(cache.dir, "upx")
	cached := filepath.Join(upxDir, hex.EncodeToString(sum[:]))
	cache.mark(cached)

	if data, err := os.ReadFile(cached); err == nil { //nolint:gosec // G703: paths under the configured cache dir
		return data, nil
	}

	if err := os.MkdirAll(upxDir, dirPerm); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	tmp := cached + ".work"
	if err := writeFile(tmp, payload, execPerm); err != nil {
		return nil, err
	}
	defer removeQuietly(tmp)

	cmd := command(ctx, "upx", append(append([]string{"-q"}, flags...), tmp)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}

	packed, err := os.ReadFile(tmp) //nolint:gosec // G703: paths under the configured cache dir
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	if err := os.Rename(tmp, cached); err != nil { //nolint:gosec // G703: paths under the configured cache dir
		return nil, fmt.Errorf("rename: %w", err)
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
	tarReader, err := openTar(blob)
	if err != nil {
		return nil, err
	}

	var (
		found []byte
		names []string
	)

	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
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

		if found, err = io.ReadAll(tarReader); err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
	}

	if found == nil {
		return nil, fmt.Errorf("%w: no member matched %q (members: %s)",
			errArchive, extract, strings.Join(names, ", "))
	}

	return found, nil
}

func extractFromZip(data []byte, extract string) ([]byte, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}

	var (
		found []byte
		names []string
	)

	for _, member := range zipReader.File {
		if member.FileInfo().IsDir() {
			continue
		}

		names = append(names, member.Name)
		if extract != "" && !memberMatches(extract, member.Name) {
			continue
		}

		if found != nil {
			return nil, ambiguousArchive(extract, names)
		}

		reader, err := member.Open()
		if err != nil {
			return nil, fmt.Errorf("zip: %w", err)
		}

		found, err = io.ReadAll(reader)
		closeQuietly(reader)

		if err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
	}

	if found == nil {
		return nil, fmt.Errorf("%w: no member matched %q (members: %s)",
			errArchive, extract, strings.Join(names, ", "))
	}

	return found, nil
}

func ambiguousArchive(extract string, names []string) error {
	if extract == "" {
		return fmt.Errorf("%w holds several files; set `extract:` to pick one (members: %s)",
			errArchive, strings.Join(names, ", "))
	}

	return fmt.Errorf("%w: `extract: %s` matched several members (%s); make it more specific",
		errArchive, extract, strings.Join(names, ", "))
}

// ipkEpoch is the fixed timestamp used in generated archives so rebuilding an
// unchanged package yields a byte-identical .ipk (the dedup/collision logic in
// cmdBuild compares content hashes).
func ipkEpoch() time.Time { return time.Unix(0, 0) }

// buildIPK writes an OpenWrt .ipk (outer gzipped tar holding debian-binary,
// control.tar.gz and data.tar.gz — the layout readControl already accepts)
// installing payload at installPath with the given mode.
func buildIPK(dest, control string, scripts map[string]string, installPath string, mode int64, payload []byte) error {
	dataTar, err := tarGzPayload(installPath, mode, payload)
	if err != nil {
		return err
	}

	controlEntries := []tarEntry{{name: "./control", mode: filePerm, data: []byte(control)}}
	for _, name := range sortedKeys(toSet(mapKeys(scripts))) {
		controlEntries = append(controlEntries,
			tarEntry{name: "./" + name, mode: execPerm, data: []byte(scripts[name])})
	}

	controlTar, err := tarGz(controlEntries)
	if err != nil {
		return err
	}

	outer, err := tarGz([]tarEntry{
		{name: "./debian-binary", mode: filePerm, data: []byte("2.0\n")},
		{name: "./control.tar.gz", mode: filePerm, data: controlTar},
		{name: "./data.tar.gz", mode: filePerm, data: dataTar},
	})
	if err != nil {
		return err
	}

	return writeFile(dest, outer, filePerm)
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

	entries := []tarEntry{{name: "./", mode: dirPerm, dir: true}}
	for i := 1; i < len(segs); i++ {
		entries = append(entries, tarEntry{
			name: "./" + strings.Join(segs[:i], "/") + "/", mode: dirPerm, dir: true,
		})
	}

	entries = append(entries, tarEntry{name: "." + clean, mode: mode, data: payload})

	return tarGz(entries)
}

// tarGz renders entries into a deterministic gzipped tar (root ownership,
// fixed epoch timestamps, no gzip header metadata).
func tarGz(entries []tarEntry) ([]byte, error) {
	var buf bytes.Buffer

	gzw := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzw)

	for _, entry := range entries {
		hdr := &tar.Header{
			Name:    entry.name,
			Mode:    entry.mode,
			Uid:     0,
			Gid:     0,
			Uname:   "root",
			Gname:   "root",
			ModTime: ipkEpoch(),
			Format:  tar.FormatGNU,
		}
		if entry.dir {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(entry.data))
		}

		if err := tarWriter.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}

		if !entry.dir {
			if _, err := tarWriter.Write(entry.data); err != nil {
				return nil, fmt.Errorf("tar: %w", err)
			}
		}
	}

	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("tar: %w", err)
	}

	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}

	return buf.Bytes(), nil
}
