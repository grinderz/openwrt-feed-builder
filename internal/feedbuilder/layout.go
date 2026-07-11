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
// layout.version lists the point releases the feed carries (all one branch);
// <release> is the per-source release (explicit kmod_version or from the
// source's release tag, accepted only when listed), falling back to the layout
// kmod_version, then the newest listed release. <kernel> is the exact kernel a
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
	"strings"
)

// pkgExts are the package file extensions; stripped before parsing the name.
var pkgExts = []string{".ipk", ".apk"}

// Layout describes the output directory layout.
type Layout struct {
	Versions    []string // point releases the feed carries, e.g. ["24.10.6", "24.10.7"]
	Branch      string   // derived from Versions, e.g. "24.10"
	DefaultFeed string
	KmodVersion string // explicit fallback release for kmods (default: newest of Versions)
}

// defaultRelease is the point release used for packages whose source carries no
// release of its own: the explicit kmod_version, else the newest of Versions.
func (l Layout) defaultRelease() string {
	if l.KmodVersion != "" {
		return l.KmodVersion
	}
	best := ""
	for _, v := range l.Versions {
		if best == "" || compareVersion(v, best) > 0 {
			best = v
		}
	}
	return orDefault(best, l.Branch)
}

// kernelDepRE matches the kernel dependency carried by every kmod, e.g.
//
//	Depends: kernel (= 6.6.30-1-4a3c1cd3a4e65473dcdef01a32cbb597), ...
var kernelDepRE = regexp.MustCompile(`kernel\s*\(\s*=\s*([^)]+?)\s*\)`)

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
	for _, release := range layout.Versions {
		if v == release {
			return v
		}
	}
	return ""
}

// kernelVermagic returns the exact kernel version string a kmod depends on, or "".
func kernelVermagic(fields map[string]string) string {
	m := kernelDepRE.FindStringSubmatch(fields["Depends"])
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// kernelABIRE matches the kernel package version format used since 24.10,
// e.g. "6.6.141~f31f6f85a36836e510d64a18a9a5f1bf-r1".
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
	for _, ext := range pkgExts {
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
func detectTarget(filename, arch string) string {
	if arch == "" {
		return ""
	}
	name := stripExt(filename)
	marker := "_" + arch
	idx := strings.Index(name, marker)
	if idx == -1 {
		return ""
	}
	suffix := strings.TrimLeft(name[idx+len(marker):], "_")
	if suffix == "" {
		return ""
	}
	tokens := strings.Split(suffix, "_")
	if len(tokens) >= 2 {
		// first token is the target, the rest form the subtarget
		return tokens[0] + "/" + strings.Join(tokens[1:], "_")
	}
	// A lone token cannot form a "target/subtarget" pair; a partial path like
	// <release>/targets/<token>/packages would never match the target/subtarget
	// URLs add.sh builds from $DISTRIB_TARGET. Fall back to the branch-wide
	// arch feed instead (target "" — still served and reachable).
	return ""
}

// targetAllowed reports whether a detected "target/subtarget" passes the
// targets filter. An empty filter allows everything. A package with no detected
// target (target == "") always passes: it is architecture-generic (e.g.
// luci-app-*, *_all) and not tied to a SoC. A filter entry may be a full
// "target/subtarget" (mediatek/mt7622), a lone subtarget (mt7621), or a lone
// target family (ramips) — any of which match. Entries are pre-trimmed of
// surrounding slashes in loadConfig.
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
// `target` is a "target/subtarget" string (from detectTarget or a per-source
// override). `sourceRelease` is the point release the package's source is
// bound to ("" = none).
func relativeDir(fields map[string]string, arch string, layout Layout, feedOverride, target, sourceRelease string) string {
	kmod := isKmod(fields)
	if target != "" && (kmod || sourceRelease != "") {
		release := orDefault(sourceRelease, layout.defaultRelease())
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
	return filepath.Join("packages-"+layout.Branch, arch, feed)
}
