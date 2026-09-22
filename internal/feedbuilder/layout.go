package feedbuilder

// Output directory layout — mirrors downloads.openwrt.org/releases/.
//
// Release-independent userspace goes into a branch-wide arch feed shared by
// every point release; anything bound to a point release (kmods, and userspace
// from a release-bound source) goes under that release's target tree, exactly
// like the official target directories:
//
//	packages-<branch>/<arch>/<feed>/                       (userspace,
//	                                      release-independent sources)
//	<release>/targets/<target>/<subtarget>/kmods/<kernel>/ (kmods)
//	<release>/targets/<target>/<subtarget>/packages/       (release-bound
//	                          userspace, kmods w/o a kernel dependency)
//
// layout.version lists the point releases the feed carries, possibly from
// several branches; each branch gets its own packages-<branch>/ tree in the
// branch's package format (opkg .ipk + Packages up to 24.10, apk .apk +
// packages.adb from 25.12 on — see apk.go). <release> is the per-source
// release (explicit kmod_version or from the source's release tag, accepted
// only when listed), falling back to the layout kmod_version when it is on the
// package's branch, then the newest listed release of that branch. <kernel> is the exact kernel a
// kmod depends on, e.g. 6.6.141-1-f31f6f85a36836e510d64a18a9a5f1bf.
// There is no all/ directory: Architecture:all packages are duplicated into
// every real architecture's feed, like the official mirror. Release-bound
// packages with no detectable target fall back to the branch-wide arch feed —
// there they may collide with another release's build of the same file (kept
// first, with a warning).
//
// Note: tying kmods to a release/target via the path is organisational. The
// hard guarantee comes from each kmod's `Depends: kernel (= <exact-version>)`,
// which opkg enforces at install time.

import (
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// pkgExts are the package file extensions; stripped before parsing the name.
func pkgExts() []string { return []string{".ipk", ".apk"} }

// Layout describes the output directory layout.
type Layout struct {
	Versions    []string // point releases the feed carries, e.g. ["24.10.7", "25.12.5"]
	DefaultFeed string
	KmodVersion string // explicit fallback release for kmods of its branch (default: newest)
}

// releaseBranch returns the branch of a 3-part point release: "24.10.7" -> "24.10".
func releaseBranch(release string) string {
	if i := strings.LastIndex(release, "."); i > 0 {
		return release[:i]
	}

	return release
}

// branchFormat is the package format a branch uses: apk from 25.12 on (the
// first release on apk-tools v3), opkg before.
func branchFormat(branch string) string {
	major, _, _ := strings.Cut(branch, ".")
	if n, err := strconv.Atoi(major); err == nil && n >= 25 {
		return formatAPK
	}

	return formatIPK
}

// branches returns the distinct branches of the carried releases, oldest
// first, e.g. ["24.10", "25.12"].
func (l Layout) branches() []string {
	set := map[string]bool{}
	for _, v := range l.Versions {
		set[releaseBranch(v)] = true
	}

	out := sortedKeys(set)
	sort.SliceStable(out, func(i, j int) bool { return compareVersion(out[i], out[j]) < 0 })

	return out
}

// hasBranch reports whether the layout carries the branch.
func (l Layout) hasBranch(branch string) bool {
	return slices.Contains(l.branches(), branch)
}

// branchesFor returns the carried branches that use a package format.
func (l Layout) branchesFor(format string) []string {
	var out []string

	for _, b := range l.branches() {
		if branchFormat(b) == format {
			out = append(out, b)
		}
	}

	return out
}

// formats returns the package formats the carried branches need.
func (l Layout) formats() []string {
	set := map[string]bool{}
	for _, b := range l.branches() {
		set[branchFormat(b)] = true
	}

	return sortedKeys(set)
}

// releasesOf returns the carried point releases of one branch.
func (l Layout) releasesOf(branch string) []string {
	var out []string

	for _, v := range l.Versions {
		if releaseBranch(v) == branch {
			out = append(out, v)
		}
	}

	return out
}

// defaultRelease is the point release used for packages of a branch whose
// source carries no release of its own: the explicit kmod_version when it is
// on that branch, else the newest listed release of the branch.
func (l Layout) defaultRelease(branch string) string {
	if l.KmodVersion != "" && releaseBranch(l.KmodVersion) == branch {
		return l.KmodVersion
	}

	best := ""
	for _, v := range l.releasesOf(branch) {
		if best == "" || compareVersion(v, best) > 0 {
			best = v
		}
	}

	return orDefault(best, branch)
}

// kernelDepRE matches the kernel dependency carried by every kmod, in the
// opkg and the apk spelling, e.g.
//
//	Depends: kernel (= 6.6.30-1-4a3c1cd3a4e65473dcdef01a32cbb597), ...
//	depends: kernel=6.12.94~5a6c1f71be683ae9980b15d3ce73e24d-r1, ...
var kernelDepRE = regexp.MustCompile(
	`(?:^|[\s,])kernel\s*(?:\(\s*=\s*([^)]+?)\s*\)|=\s*([^\s,]+))`)

func isKmod(fields map[string]string) bool {
	name := fields["Package"]
	return strings.HasPrefix(name, "kmod-") || strings.EqualFold(fields["Section"], "kernel")
}

// kmodVersionFromTag derives a kmod point release from a source's release tag,
// e.g. "v24.10.7" -> "24.10.7". To avoid misrouting kmods on sources whose tags
// are project versions (v1.4.2), the derived value is only accepted when it is
// one of the point releases the layout declares. Returns "" otherwise.
func kmodVersionFromTag(tag string, layout Layout) string {
	v := strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	if slices.Contains(layout.Versions, v) {
		return v
	}

	return ""
}

// kernelVermagic returns the exact kernel version string a kmod depends on, or "".
func kernelVermagic(fields map[string]string) string {
	m := kernelDepRE.FindStringSubmatch(fields["Depends"])
	if m == nil {
		return ""
	}

	return strings.TrimSpace(m[1] + m[2])
}

// kernelABIRE matches the kernel package version format used since 24.10
// (opkg and apk alike), e.g. "6.6.141~f31f6f85a36836e510d64a18a9a5f1bf-r1".
var kernelABIRE = regexp.MustCompile(`^(.+)~([0-9a-f]+)-r(\d+)$`)

// kernelDirName converts a kernel dependency version into the directory name
// the official kmods feed uses: "6.6.141~<hash>-r1" -> "6.6.141-1-<hash>".
// Anything unrecognized (older releases already use the dir form) passes through.
func kernelDirName(v string) string {
	if m := kernelABIRE.FindStringSubmatch(v); m != nil {
		return m[1] + "-" + m[3] + "-" + m[2]
	}

	return v
}

func stripExt(filename string) string {
	for _, ext := range pkgExts() {
		if strings.HasSuffix(filename, ext) {
			return filename[:len(filename)-len(ext)]
		}
	}

	return filename
}

// detectTarget extracts a "target/subtarget" from a package file name, or "".
//
// The convention is <pkg>_<version>_<arch>_<target>_<subtarget>, i.e. the
// architecture is followed by the SoC target and subtarget. Examples:
//
//	amneziawg-tools_v25.12.4_aarch64_cortex-a53_mediatek_mt7622 -> mediatek/mt7622
//	kmod-foo_1.0_aarch64_cortex-a53_bcm4908_generic            -> bcm4908/generic
//	luci-app-x_4.6.1_all                                        -> ""
//
// targetPathSegments is the length of a "target/subtarget" pair.
const targetPathSegments = 2

func detectTarget(filename, arch string) string {
	if arch == "" {
		return ""
	}

	name := stripExt(filename)
	marker := "_" + arch

	_, after, ok := strings.Cut(name, marker)
	if !ok {
		return ""
	}

	suffix := strings.TrimLeft(after, "_")
	if suffix == "" {
		return ""
	}

	tokens := strings.Split(suffix, "_")
	if len(tokens) >= targetPathSegments {
		// first token is the target, the rest form the subtarget
		return tokens[0] + "/" + strings.Join(tokens[1:], "_")
	}
	// A lone token cannot form a "target/subtarget" pair; a partial path like
	// <release>/targets/<token>/packages would never match the target/subtarget
	// URLs add.sh builds from $DISTRIB_TARGET. Fall back to the branch-wide
	// arch feed instead (target "" — still served and reachable).
	return ""
}

// archFamilyRE matches an OpenWrt package architecture inside a file name
// (as a "_"-delimited token run, e.g. "_mipsel_24kc", "_x86_64"), used to tell
// "names a filtered-out architecture" from "carries no architecture at all".
var archFamilyRE = regexp.MustCompile(`_(aarch64|arm|armeb|mips|mipsel|mips64|mips64el|` +
	`i386|i486|i686|x86_64|powerpc|powerpc64|riscv64|loongarch64)(_|$)`)

// hasArchToken reports whether name holds "_<arch>" followed by "_" or the end.
func hasArchToken(name, arch string) bool {
	marker := "_" + arch
	for pos := 0; ; {
		j := strings.Index(name[pos:], marker)
		if j < 0 {
			return false
		}

		end := pos + j + len(marker)
		if end == len(name) || name[end] == '_' {
			return true
		}

		pos = end
	}
}

// fileNameAllowed decides from a package file name alone (plus the
// architecture a feed index states, when known) whether the package can pass
// the architectures / targets filters and a carried branch of its format —
// so filtered-out assets are never downloaded. It only rejects what the name
// proves: a name without a recognizable architecture passes and is filtered
// after download from its real metadata as before.
//
// formats are the package formats the package may be routed into; target
// is the source's `target:` override ("" = detect from the name).
func fileNameAllowed(name, knownArch, target string, formats, archFilter, targetFilter map[string]bool) bool {
	switch {
	case strings.HasSuffix(name, ".ipk") && !formats[formatIPK],
		strings.HasSuffix(name, ".apk") && !formats[formatAPK]:
		return false
	}

	if len(archFilter) == 0 {
		return true
	}

	if knownArch != "" && knownArch != archAll && knownArch != archNoarch && !archFilter[knownArch] {
		return false
	}

	base := stripExt(name)

	arch := ""
	for a := range archFilter {
		if hasArchToken(base, a) && len(a) > len(arch) {
			arch = a
		}
	}

	if arch == "" {
		for _, a := range []string{archAll, archNoarch} {
			if hasArchToken(base, a) {
				arch = a
			}
		}
	}

	if arch == "" {
		// an architecture the filter does not list, or none at all
		return !archFamilyRE.MatchString(base)
	}

	if target == "" {
		target = detectTarget(name, arch)
	}

	return targetAllowed(target, targetFilter)
}

// targetAllowed reports whether a detected "target/subtarget" passes the
// targets filter. An empty filter allows everything. A package with no detected
// target (target == "") always passes: it is architecture-generic (e.g.
// luci-app-*, *_all) and not tied to a SoC. A filter entry may be a full
// "target/subtarget" (mediatek/mt7622), a lone subtarget (mt7621), or a lone
// target family (ramips) — any of which match. Entries are pre-trimmed of
// surrounding slashes in LoadConfig.
func targetAllowed(target string, filter map[string]bool) bool {
	if len(filter) == 0 || target == "" {
		return true
	}

	t := strings.Trim(target, "/")
	if filter[t] {
		return true
	}

	segs := strings.Split(t, "/")
	if filter[segs[len(segs)-1]] { // subtarget alone, e.g. mt7621
		return true
	}

	return filter[segs[0]] // target family alone, e.g. ramips
}

// relativeDir returns the output-relative directory for one package (see the
// package comment for the tree shape). A release's targets/ tree needs a
// target: kmods there go under kmods/<kernel>/, everything else (release-bound
// userspace, kmods without a kernel dependency) into its packages/ — the same
// split the official target directories use. A package with no detectable
// target falls back to the branch-wide arch feed so it is still served rather
// than dropped.
//
// `branch` is the branch the package is routed into, `target` a
// "target/subtarget" string (from detectTarget or a per-source override).
// `sourceRelease` is the point release the package's source is bound to
// ("" = none; when set it lies on `branch`).
func relativeDir(fields map[string]string, arch string, layout Layout,
	branch, feedOverride, target, sourceRelease string,
) string {
	kmod := isKmod(fields)
	if target != "" && (kmod || sourceRelease != "") {
		release := orDefault(sourceRelease, layout.defaultRelease(branch))
		parts := []string{release, "targets"}

		parts = append(parts, strings.Split(strings.Trim(target, "/"), "/")...)
		if vermagic := kernelVermagic(fields); kmod && vermagic != "" {
			parts = append(parts, "kmods", kernelDirName(vermagic))
		} else {
			parts = append(parts, "packages")
		}

		return filepath.Join(parts...)
	}

	feed := orDefault(feedOverride, layout.DefaultFeed)

	return filepath.Join("packages-"+branch, arch, feed)
}
