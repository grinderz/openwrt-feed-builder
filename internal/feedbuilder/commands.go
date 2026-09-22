package feedbuilder

// The commands behind the CLI (internal/cli) — sign / verify / howto / serve /
// genkey / indexdiff — and the helpers they share with build.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// mib is one mebibyte, for size reports.
const mib = 1 << 20

var safeRE = regexp.MustCompile(`[^A-Za-z0-9._+~-]`)

func safeName(pkg, version, arch string) string {
	raw := fmt.Sprintf("%s_%s_%s.ipk", pkg, version, arch)
	return safeRE.ReplaceAllString(raw, "_")
}

func sha256File(path string) (string, error) {
	return hashFile(path, sha256.New())
}

// copyFile copies src to dst; a failed copy removes the destination so no
// truncated file is left behind (a partial .ipk in a feed dir would later
// abort that directory's index).
func copyFile(src, dst string) error {
	input, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer closeQuietly(input)

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

	if _, err := io.Copy(out, input); err != nil {
		closeQuietly(out)
		removeQuietly(dst)

		return fmt.Errorf("copy: %w", err)
	}

	if err := out.Close(); err != nil {
		removeQuietly(dst)
		return fmt.Errorf("close: %w", err)
	}

	return nil
}

// toSet builds a lookup set from a slice.
func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, it := range items {
		set[it] = true
	}

	return set
}

// sortedKeys returns the keys of a set in sorted order.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// sortedStringKeys returns the keys of any string-keyed map in sorted order.
func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// stageSecretKey runs a shell command that prints a secret key (usign or apk
// PEM) and writes its output to a private (0600) temp file, since usign and
// apk read the key from a path. It returns the path and a cleanup func that
// removes the file. The key never touches the output tree.
func stageSecretKey(ctx context.Context, keyCmd string) (string, func(), error) {
	out, err := command(ctx, "sh", "-c", keyCmd).Output()
	if err != nil {
		return "", nil, fmt.Errorf("secret key command %q failed: %w", keyCmd, err)
	}

	key := bytes.TrimRight(out, "\n")
	if len(key) == 0 {
		return "", nil, fmt.Errorf("%w: secret key command %q produced no output", errKey, keyCmd)
	}

	tmp, err := os.CreateTemp("", "ofb-secret-*.key")
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}

	cleanup := func() { removeQuietly(tmp.Name()) }

	if err := tmp.Chmod(secretPerm); err != nil {
		closeQuietly(tmp)
		cleanup()

		return "", nil, fmt.Errorf("chmod: %w", err)
	}

	if _, err := tmp.Write(append(key, '\n')); err != nil {
		closeQuietly(tmp)
		cleanup()

		return "", nil, fmt.Errorf("write: %w", err)
	}

	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close: %w", err)
	}

	return tmp.Name(), cleanup, nil
}

type collectedPkg struct {
	url          string
	path         string
	feedOverride string
	sourceTarget string
	kmodVersion  string // point release the source is bound to ("" = none/layout default)
	branch       string // OpenWrt branch the source is pinned to (`openwrt:`; "" = every branch of the package's format)
}

// sameSize reports whether two files exist and have equal size.
func sameSize(first, second string) bool {
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false
	}

	sb, err := os.Stat(second)

	return err == nil && firstInfo.Size() == sb.Size()
}

// onlyMatch reports whether a source passes the --only filter: any pattern
// (shell glob) matching its type or its name selects it.
func onlyMatch(only []string, typ, name string) bool {
	for _, pattern := range only {
		if fnmatch(pattern, typ) || fnmatch(pattern, name) {
			return true
		}
	}

	return false
}

// collectPackages resolves every configured source to concrete .ipk URLs and
// fetches them (cached), returning the downloaded packages and how many sources
// failed. A github source with a `tags:` list expands into one pass per tag.
// Each source is best-effort: a failure (404, timeout, rate limit, bad config)
// skips just that source, not the whole build. A non-empty `only` restricts
// the build to sources whose type or name matches one of its glob patterns.
func collectPackages(
	ctx context.Context, cfg *Config, client *Client, cache *Cache, only []string,
) ([]collectedPkg, int) {
	archFilter, targetFilter := toSet(cfg.Architectures), toSet(cfg.Targets)

	var (
		collected []collectedPkg
		sdkSrcs   []Source
	)

	failures := 0

	for _, src := range cfg.Sources {
		if len(only) > 0 && !onlyMatch(only, src.strOr("type", ""), src.strOr("name", "")) {
			sourceLinef(src.strOr("name", src.strOr("type", "")), "%s", Dim("skipped (--only)"))
			continue
		}

		for _, src := range expandSourceTags(src) {
			name := src.strOr("name", src.strOr("type", ""))
			if !src.boolOr("enabled", true) {
				sourceLinef(name, "%s", Dim("skipped (disabled)"))
				continue
			}

			if src.strOr("type", "") == "binary" {
				pkgs, err := binaryPackages(ctx, cfg, client, cache, src)
				if err != nil {
					failures++

					warnf("%s: skipped: %v", name, err)

					continue
				}

				sourceLinef(name, "%d package(s) repacked", len(pkgs))
				collected = append(collected, pkgs...)

				continue
			}

			if src.strOr("type", "") == "sdk" {
				// gathered and built after the loop: sdk sources sharing a
				// buildroot and release/target batch into one container run
				sdkSrcs = append(sdkSrcs, src)
				continue
			}

			files, resolvedTag, err := iterIPKURLs(ctx, client, src, defaultPkgPattern(cfg.Layout))
			if err != nil {
				failures++

				warnf("%s: skipped: %v", name, err)

				continue
			}

			kmodVersion := sourceRelease(cfg.Layout, src, resolvedTag, name)
			// skip what the file name already rules out, before downloading
			formats := toSet(cfg.Layout.formats())
			if pin := orDefault(releaseBranch(kmodVersion), src.strOr("openwrt", "")); pin != "" {
				formats = map[string]bool{branchFormat(pin): true}
			}

			var wanted []remoteFile

			for _, rf := range files {
				if fileNameAllowed(lastPathPart(stripQuery(rf.url)), rf.arch, src.strOr("target", ""),
					formats, archFilter, targetFilter) {
					wanted = append(wanted, rf)
				}
			}

			if skipped := len(files) - len(wanted); skipped > 0 {
				sourceLinef(name, "%d package url(s), %s", len(wanted),
					Dim(fmt.Sprintf("%d skipped by name (arch/target/format filters)", skipped)))
			} else {
				sourceLinef(name, "%d package url(s)", len(files))
			}

			for _, file := range wanted {
				path, err := cache.get(ctx, file)
				if err != nil {
					problemf("failed %s: %v", file.url, err)
					continue
				}

				collected = append(collected, collectedPkg{
					url:          file.url,
					path:         path,
					feedOverride: src.strOr("feed", ""),
					sourceTarget: src.strOr("target", ""),
					kmodVersion:  kmodVersion,
					branch:       src.strOr("openwrt", ""),
				})
			}
		}
	}

	if len(sdkSrcs) > 0 {
		pkgs, sdkFailures := sdkCollect(ctx, cfg, cache, sdkSrcs)
		failures += sdkFailures

		if len(pkgs) > 0 {
			sourceLinef("sdk", "%d package(s) built from source", len(pkgs))
		}

		collected = append(collected, pkgs...)
	}

	return collected, failures
}

// sourceRelease determines the point release a source is bound to: an explicit
// `kmod_version:` wins, otherwise it is derived from the resolved release tag
// when that tag is one of the declared layout.version releases (v24.10.7 ->
// 24.10.7). It routes the source's kmods into <release>/... and its userspace
// into that release's target packages/. A tag that looks like a point release
// of a carried branch but is not declared triggers a warning and falls back
// to the default.
func sourceRelease(layout Layout, src Source, resolvedTag, name string) string {
	if v := src.strOr("kmod_version", ""); v != "" {
		return v
	}

	if v := kmodVersionFromTag(resolvedTag, layout); v != "" {
		return v
	}

	for _, branch := range layout.branches() {
		if v := strings.TrimPrefix(resolvedTag, "v"); strings.HasPrefix(v, branch+".") {
			warnf("%s: tag %s looks like a %s point release "+
				"but is not listed in layout.version; its kmods go under %s",
				name, resolvedTag, branch, layout.defaultRelease(branch))
		}
	}

	return ""
}

// packageBranches decides which carried branches a collected package goes
// into: the branch of its source's release when it has one, else the
// source's explicit `openwrt:` branch, else every branch using the package's
// format (a .ipk lands in the opkg branches, an .apk in the apk ones). An
// empty result comes with the reason the package cannot be routed.
func packageBranches(layout Layout, c collectedPkg, format string) ([]string, string) {
	pinned := c.branch
	if c.kmodVersion != "" {
		pinned = releaseBranch(c.kmodVersion)
	}

	if pinned != "" {
		switch {
		case !layout.hasBranch(pinned):
			return nil, fmt.Sprintf("branch %s is not in layout.version", pinned)
		case branchFormat(pinned) != format:
			return nil, fmt.Sprintf("%s feeds use .%s packages, not .%s",
				pinned, branchFormat(pinned), format)
		}

		return []string{pinned}, ""
	}

	if branches := layout.branchesFor(format); len(branches) > 0 {
		return branches, ""
	}

	if format == formatAPK {
		return nil, "no apk branch (25.12+) in layout.version"
	}

	return nil, "no opkg branch (24.10 or older) in layout.version"
}

// branchFeeds maps each carried branch to the feed dir names produced under
// its packages-<branch>/<arch>/<feed>/ tree.
func branchFeeds(relPaths []string) map[string][]string {
	sets := map[string]map[string]bool{}

	for _, rel := range relPaths {
		segs := strings.Split(rel, "/")
		if len(segs) < 3 || !strings.HasPrefix(segs[0], "packages-") {
			continue
		}

		branch := strings.TrimPrefix(segs[0], "packages-")
		if sets[branch] == nil {
			sets[branch] = map[string]bool{}
		}

		sets[branch][segs[2]] = true
	}

	out := map[string][]string{}
	for b, set := range sets {
		out[b] = sortedKeys(set)
	}

	return out
}

// writeHelpers writes the repo helpers (add.sh per release, public keys) for
// the feed dirs in relPaths and reports what it wrote.
func writeHelpers(root string, cfg *Config, relPaths []string, signed bool, fingerprint string) error {
	scripts, err := writeRepoHelpers(root, cfg, branchFeeds(relPaths), signed, fingerprint)
	if err != nil {
		return err
	}

	for _, s := range scripts {
		rel, _ := filepath.Rel(root, s)
		fmt.Println(Dim("  wrote " + filepath.ToSlash(rel)))
	}

	formats := toSet(cfg.Layout.formats())
	if signed && formats[formatIPK] && cfg.Sign.PublicKey != "" {
		fmt.Println(Dim("  wrote repo.pub"))
	}

	if signed && formats[formatAPK] && cfg.Sign.APKPublicKey != "" {
		fmt.Println(Dim("  wrote " + apkPubName))
	}

	return nil
}

// snapshotVersions reads every existing feed index (Packages or packages.adb)
// under root and returns a map of "<dir>|<package>" -> version, where <dir> is
// the index directory relative to root. Used to diff what changed after a
// rebuild. A missing or unreadable feed yields an empty map.
func snapshotVersions(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isIndexFile(d.Name()) {
			return nil //nolint:nilerr // unreadable entries just do not count
		}

		stanzas, err := readIndexFile(path)
		if err != nil {
			return nil //nolint:nilerr // an unreadable index counts as empty
		}

		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)

		for _, st := range stanzas {
			if pkg := st["Package"]; pkg != "" {
				out[rel+"|"+pkg] = st["Version"]
			}
		}

		return nil
	})

	return out
}

// reportChanges prints, at the end of a build, what changed versus the previous
// feed: packages added, upgraded, downgraded or removed (with versions), plus
// the superseded .ipk files pruned this run. Keys are "<dir>|<package>".
// Removals only exist in a --full rebuild — in an incremental build, entries
// missing from cur (sources filtered by --only, failed, or simply
// unchanged-and-untouched) are still on disk and are not reported; the only
// deletions are the pruned files, listed by path.
func reportChanges(prev, cur map[string]string, prunedFiles []string, full bool) {
	type change struct{ loc, pkg, from, to string }

	var added, removed, upgraded, downgraded []change

	split := func(k string) (string, string) {
		if i := strings.LastIndex(k, "|"); i >= 0 {
			return k[:i], k[i+1:]
		}

		return "", k
	}
	for k, newVer := range cur {
		loc, pkg := split(k)

		oldVer, ok := prev[k]
		if !ok {
			added = append(added, change{loc, pkg, "", newVer})
			continue
		}

		switch c := compareVersion(newVer, oldVer); {
		case c > 0:
			upgraded = append(upgraded, change{loc, pkg, oldVer, newVer})
		case c < 0:
			downgraded = append(downgraded, change{loc, pkg, oldVer, newVer})
		}
	}

	if full {
		for k, ov := range prev {
			if _, ok := cur[k]; !ok {
				loc, pkg := split(k)
				removed = append(removed, change{loc, pkg, ov, ""})
			}
		}
	}

	if len(added)+len(upgraded)+len(downgraded)+len(removed)+len(prunedFiles) == 0 {
		fmt.Println()
		phasef("changes: none %s", Dim("(package set identical to previous build)"))

		return
	}

	sortCh := func(changes []change) {
		sort.Slice(changes, func(i, j int) bool {
			if changes[i].loc != changes[j].loc {
				return changes[i].loc < changes[j].loc
			}

			return changes[i].pkg < changes[j].pkg
		})
	}
	sortCh(upgraded)
	sortCh(downgraded)
	sortCh(added)
	sortCh(removed)

	fmt.Println()
	phasef("changes: %d new, %d updated, %d downgraded, %d removed, %d superseded file(s) pruned",
		len(added), len(upgraded), len(downgraded), len(removed), len(prunedFiles))

	for _, c := range upgraded {
		fmt.Printf("  %s %s/%s: %s -> %s\n", Cyan("~"), c.loc, c.pkg, c.from, Bold(c.to))
	}

	for _, c := range downgraded {
		fmt.Printf("  %s %s/%s: %s -> %s  %s\n", Yellow("v"), c.loc, c.pkg, c.from, c.to, Yellow("(downgrade)"))
	}

	for _, c := range added {
		fmt.Printf("  %s %s/%s: %s\n", Green("+"), c.loc, c.pkg, Bold(c.to))
	}

	for _, c := range removed {
		fmt.Printf("  %s %s/%s: %s\n", Red("-"), c.loc, c.pkg, c.from)
	}

	for _, f := range prunedFiles {
		fmt.Println(Dim(fmt.Sprintf("  x %s  (superseded)", f)))
	}
}

func fieldOr(fields map[string]string, key, def string) string {
	if v, ok := fields[key]; ok {
		return v
	}

	return def
}

var feedLabelRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func feedLabel(prefix, relPath string) string {
	return orDefault(prefix, "custom") + "_" + feedLabelRE.ReplaceAllString(relPath, "_")
}

// routerBlock is one ready-to-paste customfeeds.conf block: all the feed lines
// one device needs (branch-wide arch feeds + its release's target tree).
type routerBlock struct {
	title string
	rels  []string
}

// routerBlocks groups the generated feed dirs into per-device blocks. Every
// "<release>/targets/<target>/<subtarget>" tree becomes one block holding the
// branch-wide packages-<branch>/<arch>/ feed(s) for the architectures shipped
// in that tree, followed by the tree's own packages/ and kmods/ dirs. Branch
// feeds not covered by any target tree get a trailing arch-only block, so
// each printed block is complete on its own.
func routerBlocks(relPaths []string, feedArches map[string]map[string]bool, branch string) []routerBlock {
	branchByArch := map[string]string{} // arch -> packages-<branch>/<arch>/<feed> rel

	var branchOrder []string

	type tree struct{ pkgs, kmods []string }

	trees := map[string]*tree{} // "<release>/targets/<target>/<subtarget>"

	var treeOrder []string

	for _, rel := range relPaths {
		segs := strings.Split(rel, "/")
		if strings.HasPrefix(rel, "packages-"+branch+"/") && len(segs) >= 2 {
			arch := segs[1]
			if _, ok := branchByArch[arch]; !ok {
				branchOrder = append(branchOrder, arch)
			}

			branchByArch[arch] = rel

			continue
		}

		if len(segs) >= 5 && segs[1] == "targets" {
			key := strings.Join(segs[:4], "/")

			entry, ok := trees[key]
			if !ok {
				entry = &tree{}
				trees[key] = entry
				treeOrder = append(treeOrder, key)
			}

			if segs[4] == "kmods" {
				entry.kmods = append(entry.kmods, rel)
			} else {
				entry.pkgs = append(entry.pkgs, rel)
			}

			continue
		}
		// anything unexpected: show it as its own block rather than dropping it
		trees[rel] = &tree{pkgs: []string{rel}}
		treeOrder = append(treeOrder, rel)
	}

	// arches shipped in each tree (union over its dirs) -> branch feed lines
	coveredArch := map[string]bool{}

	var blocks []routerBlock

	for _, key := range treeOrder {
		tree := trees[key]
		archSet := map[string]bool{}

		for _, rel := range append(append([]string{}, tree.pkgs...), tree.kmods...) {
			for a := range feedArches[rel] {
				archSet[a] = true
			}
		}

		var rels []string

		arches := sortedKeys(archSet)
		for _, a := range arches {
			if b, ok := branchByArch[a]; ok {
				rels = append(rels, b)
				coveredArch[a] = true
			}
		}

		rels = append(rels, tree.pkgs...)
		rels = append(rels, tree.kmods...)

		title := key
		if len(arches) > 0 {
			title += " (" + strings.Join(arches, ", ") + ")"
		}

		blocks = append(blocks, routerBlock{title: title, rels: rels})
	}

	for _, arch := range branchOrder {
		if !coveredArch[arch] {
			blocks = append(blocks, routerBlock{
				title: arch + " (release-independent packages only)",
				rels:  []string{branchByArch[arch]},
			})
		}
	}

	return blocks
}

// relBranch returns the carried branch a feed dir belongs to: the one in its
// packages-<branch>/ prefix or of its <release>/ prefix ("" = none).
func relBranch(layout Layout, rel string) string {
	first, _, _ := strings.Cut(rel, "/")

	branch := ""
	if b, ok := strings.CutPrefix(first, "packages-"); ok {
		branch = b
	} else if releaseRE.MatchString(first) {
		branch = releaseBranch(first)
	}

	if layout.hasBranch(branch) {
		return branch
	}

	return ""
}

func printRouterHelp(cfg *Config, relPaths []string, feedArches map[string]map[string]bool,
	signed bool, fingerprint string,
) {
	base := orDefault(cfg.BaseURL, "http://HOST:PORT")
	byBranch := map[string][]string{}

	var other []string

	for _, rel := range relPaths {
		if b := relBranch(cfg.Layout, rel); b != "" {
			byBranch[b] = append(byBranch[b], rel)
		} else {
			other = append(other, rel)
		}
	}

	formats := map[string]bool{}

	for _, branch := range cfg.Layout.branches() {
		formats[branchFormat(branch)] = true
		printBranchHelp(cfg, base, branch, byBranch[branch], feedArches)
	}

	if len(other) > 0 {
		fmt.Println()
		phasef("feed dirs outside the branches in layout.version")

		for _, rel := range other {
			fmt.Printf("  %s/%s\n", base, rel)
		}
	}

	fmt.Println()
	fmt.Println(Dim("(the tree mirrors downloads.openwrt.org/releases/: release-independent\n" +
		" userspace packages in packages-<branch>/<arch>/<feed>/, release-bound ones in\n" +
		" <release>/targets/<target>/<subtarget>/packages/, kmods next to them under\n" +
		" kmods/<kernel>/ keyed by the exact kernel each kmod depends on; each\n" +
		" <release>/add.sh is pinned to its release and resolves arch / target /\n" +
		" kernel from the router. 24.10 and older use opkg (.ipk, Packages), 25.12+\n" +
		" use apk (.apk, packages.adb).)"))

	printKeyHelp(cfg, formats, signed, fingerprint)
}

// printBranchHelp prints how a router of one branch adds the feed: the
// release-pinned installers, then the manual feed lines per device. A branch
// with nothing built yet still gets its installers, with a note.
func printBranchHelp(cfg *Config, base, branch string, rels []string, feedArches map[string]map[string]bool) {
	apk := branchFormat(branch) == formatAPK

	tool, list := "opkg", "/etc/opkg/customfeeds.conf"
	if apk {
		tool, list = "apk", "/etc/apk/repositories.d/customfeeds.list"
	}

	fmt.Println()
	phasef("add this feed on the router (%s / %s)", branch, tool)

	if len(rels) == 0 {
		fmt.Printf("  %s — run a build first (make feed); the installer then is:\n",
			Yellow("nothing built for "+branch+" yet"))
	} else {
		fmt.Println("Easiest — run the installer matching the router's release (auto-detects")
		fmt.Println("arch / target / kernel and adds only the feeds that exist):")
	}

	for _, v := range cfg.Layout.releasesOf(branch) {
		fmt.Printf("  wget -qO- %s/%s/add.sh | sh\n", base, v)
	}

	if len(rels) == 0 {
		return
	}

	fmt.Printf("\nOr add lines manually to %s.\n", list)
	fmt.Println("Each block below is complete for one device — copy the one matching your")
	fmt.Println("router's release, target and architecture:")

	for _, b := range routerBlocks(rels, feedArches, branch) {
		fmt.Printf("\n  %s\n", Cyan("# "+b.title))

		for _, rel := range b.rels {
			if apk {
				fmt.Printf("  %s/%s/packages.adb\n", base, rel)
			} else {
				fmt.Printf("  src/gz %s %s/%s\n", feedLabel(cfg.FeedPrefix, rel), base, rel)
			}
		}
	}
}

// printKeyHelp tells how routers trust the feed (or how to work unsigned).
func printKeyHelp(cfg *Config, formats map[string]bool, signed bool, fingerprint string) {
	fmt.Println()

	if signed {
		phasef("the feed is signed — install the PUBLIC key on the router")

		if formats[formatIPK] {
			fp := orDefault(fingerprint, "<fingerprint>")
			fmt.Printf("  opkg: scp %s root@router:/etc/opkg/keys/%s\n", cfg.Sign.PublicKey, fp)

			if fingerprint == "" {
				fmt.Println(Dim("  (run 'genkey' or check the public key; the filename must be the key id)"))
			}
		}

		if formats[formatAPK] {
			fmt.Printf("  apk:  scp %s root@router:/etc/apk/keys/%s.pem\n",
				orDefault(cfg.Sign.APKPublicKey, "<apk public key>"), orDefault(cfg.FeedPrefix, "custom"))
		}

		fmt.Println(Dim("  (add.sh installs the key itself)"))
	} else {
		phasef("the feed is unsigned")
		fmt.Println("Sign it with the `sign` command (or build --sign); otherwise:")

		if formats[formatIPK] {
			fmt.Println("  opkg: comment out 'option check_signature 1' in /etc/opkg.conf")
		}

		if formats[formatAPK] {
			fmt.Println("  apk:  pass --allow-untrusted to apk")
		}
	}

	fmt.Println()

	if formats[formatIPK] {
		fmt.Println("Then: opkg update && opkg install <package>")
	}

	if formats[formatAPK] {
		fmt.Println("Then: apk update && apk add <package>")
	}
}

// indexLeaves returns every feed dir under root holding an index of either
// format (Packages or packages.adb), sorted.
func indexLeaves(root string) []string {
	var leaves []string

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { // missing root: no leaves
		if err == nil && !d.IsDir() && isIndexFile(d.Name()) {
			leaves = append(leaves, filepath.Dir(p))
		}

		return nil
	})

	sort.Strings(leaves)

	return leaves
}

// leafFormats returns the distinct package formats of the given feed dirs.
func leafFormats(leaves []string) []string {
	set := map[string]bool{}
	for _, leaf := range leaves {
		set[leafFormat(leaf)] = true
	}

	return sortedKeys(set)
}

// Sign signs an existing feed tree in place: every Packages index gets a
// fresh Packages.sig, every packages.adb an embedded signature, and the repo
// helpers (repo.pub, repo-apk.pem, per-release add.sh) are regenerated with
// the keys baked in. Lets an unsigned build (e.g. produced on a build host
// that has no keys) be signed afterwards, or a feed re-signed after a key
// rotation — without rebuilding anything.
func Sign(ctx context.Context, cfg *Config) error {
	if !cfg.Sign.Enabled {
		return fmt.Errorf("%w: sign.enabled is false in the config", errConfig)
	}
	// every feed dir under the output tree holds an index
	leaves := indexLeaves(cfg.OutputDir)
	if len(leaves) == 0 {
		return fmt.Errorf("%w: no feed indexes under %s — run build first", errIndex, cfg.OutputDir)
	}

	keys, err := prepareSignKeys(ctx, cfg, leafFormats(leaves))
	if err != nil {
		return err
	}
	defer keys.close()

	failures := 0

	var relPaths []string

	for _, leaf := range leaves {
		rel, _ := filepath.Rel(cfg.OutputDir, leaf)
		rel = filepath.ToSlash(rel)

		if err := keys.signLeaf(ctx, cfg, leaf); err != nil {
			failures++

			problemf("sign failed for %s: %v", rel, err)

			continue
		}

		relPaths = append(relPaths, rel)
		fmt.Printf("  %s %s\n", rel, Green("[signed]"))
	}

	// regenerate the helpers so add.sh installs the public key on the router
	if err := writeHelpers(cfg.OutputDir, cfg, relPaths, true, keys.fingerprint); err != nil {
		return err
	}

	if failures > 0 {
		return fmt.Errorf("%w: %d index(es) failed to sign", errIndex, failures)
	}

	return nil
}

// Verify validates the signatures of an existing feed tree: every Packages
// index must have a Packages.sig that verifies against sign.public_key, every
// packages.adb a signature apk accepts with sign.apk_public_key, the served
// repo.pub / repo-apk.pem must match those keys, and every release's add.sh
// must still carry the key-install step (an unsigned rebuild regenerates
// add.sh without it). Read-only; non-zero exit when anything is missing or
// does not verify. dir overrides the config's output_dir ("" = use it).
func Verify(ctx context.Context, cfg *Config, dir string) error {
	root := cfg.OutputDir
	if dir != "" {
		root = dir
	}

	leaves := indexLeaves(root)
	if len(leaves) == 0 {
		return fmt.Errorf("%w: no feed indexes under %s", errIndex, root)
	}

	formats := toSet(leafFormats(leaves))
	if err := verifyTools(ctx, cfg, formats); err != nil {
		return err
	}

	ok, missing, bad := verifyLeaves(ctx, cfg, root, leaves)

	badPublished, err := verifyPublished(cfg, root, formats)
	if err != nil {
		return err
	}

	bad += badPublished

	phasef("verified %d index(es): %s ok, %s missing, %s invalid", len(leaves),
		Green(strconv.Itoa(ok)), countRed(missing), countRed(bad))

	if missing+bad > 0 {
		return fmt.Errorf("%w: %d missing, %d invalid", errVerify, missing, bad)
	}

	return nil
}

// countRed renders a problem count, red when non-zero.
func countRed(n int) string {
	if n == 0 {
		return strconv.Itoa(n)
	}

	return Red(strconv.Itoa(n))
}

// verifyTools checks the keys and tools verifying the given formats needs.
func verifyTools(ctx context.Context, cfg *Config, formats map[string]bool) error {
	if formats[formatIPK] {
		if cfg.Sign.PublicKey == "" {
			return fmt.Errorf("%w: sign.public_key is not set in the config", errConfig)
		}

		if !usignAvailable() {
			return fmt.Errorf("%w: usign not found on PATH", errUsign)
		}
	}

	if formats[formatAPK] {
		if cfg.Sign.APKPublicKey == "" {
			return fmt.Errorf("%w: sign.apk_public_key is not set in the config", errConfig)
		}

		if err := cfg.APKTool.check(ctx); err != nil {
			return err
		}
	}

	return nil
}

// verifyLeaves checks every index signature, printing one line per feed dir.
func verifyLeaves(ctx context.Context, cfg *Config, root string, leaves []string) (int, int, int) {
	ok, missing, bad := 0, 0, 0

	for _, leaf := range leaves {
		rel, _ := filepath.Rel(root, leaf)
		rel = filepath.ToSlash(rel)

		var err error

		if leafFormat(leaf) == formatAPK {
			if signed, serr := adbSigned(filepath.Join(leaf, apkIndexName)); serr == nil && !signed {
				missing++

				fmt.Printf("  %s: %s\n", rel, Red("MISSING signature"))

				continue
			}

			err = cfg.APKTool.verify(ctx, leaf, cfg.Sign.APKPublicKey)
		} else {
			if _, serr := os.Stat(filepath.Join(leaf, "Packages.sig")); serr != nil {
				missing++

				fmt.Printf("  %s: %s\n", rel, Red("MISSING signature"))

				continue
			}

			err = verifyIndex(ctx, leaf, cfg.Sign.PublicKey)
		}

		if err != nil {
			bad++

			fmt.Printf("  %s: %s (%v)\n", rel, Red("INVALID"), err)

			continue
		}

		ok++

		fmt.Printf("  %s: %s\n", rel, Green("ok"))
	}

	return ok, missing, bad
}

// verifyPublished checks what routers fetch next to the indexes: the served
// public keys must be the configured ones, and every release's add.sh must
// carry the key-install step. Returns the number of problems found.
func verifyPublished(cfg *Config, root string, formats map[string]bool) (int, error) {
	bad := 0

	for _, pub := range []struct{ format, want, served string }{
		{formatIPK, cfg.Sign.PublicKey, "repo.pub"},
		{formatAPK, cfg.Sign.APKPublicKey, apkPubName},
	} {
		if !formats[pub.format] {
			continue
		}

		want, err := os.ReadFile(pub.want)
		if err != nil {
			return 0, fmt.Errorf("%w: cannot read %s: %w", errKey, pub.want, err)
		}

		if got, err := os.ReadFile(filepath.Join(root, pub.served)); err != nil {
			bad++

			fmt.Printf("  %s: %s (run `sign`)\n", pub.served, Red("MISSING"))
		} else if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
			bad++

			fmt.Printf("  %s: %s\n", pub.served, Red("does NOT match the configured public key"))
		} else {
			fmt.Printf("  %s: %s\n", pub.served, Green("ok"))
		}
	}

	// an unsigned rebuild regenerates <release>/add.sh WITHOUT the key-install
	// step while leaving valid signatures elsewhere — routers running such an
	// installer would fail signature checks, so treat it as a broken tree
	fingerprint := ""

	if formats[formatIPK] {
		want, err := os.ReadFile(cfg.Sign.PublicKey)
		if err == nil {
			fingerprint, err = pubkeyFingerprint(string(want))
		}

		if err != nil {
			return 0, fmt.Errorf("%w: sign.public_key: %w", errKey, err)
		}
	}

	for _, release := range cfg.Layout.Versions {
		format := branchFormat(releaseBranch(release))
		if !formats[format] {
			continue
		}

		data, err := os.ReadFile(filepath.Join(root, release, "add.sh"))
		if err != nil {
			continue // release not carried by this tree
		}

		marker := fingerprint
		if format == formatAPK {
			marker = `SIGNED="1"`
		}

		if !strings.Contains(string(data), marker) {
			bad++

			fmt.Printf("  %s/add.sh: %s (run `sign`)\n", release, Red("no key-install step"))
		} else {
			fmt.Printf("  %s/add.sh: %s\n", release, Green("ok"))
		}
	}

	return bad, nil
}

// Howto prints, for an already built output tree, the same "add this feed
// on the router" instructions that build prints at the end — without building
// anything. Feed dirs and their architectures are reconstructed from the
// indexes on disk; the tree counts as signed when every index is signed.
func Howto(cfg *Config) error {
	var relPaths []string

	feedArches := map[string]map[string]bool{}
	signedCount := 0

	for _, leaf := range indexLeaves(cfg.OutputDir) {
		rel, _ := filepath.Rel(cfg.OutputDir, leaf)
		rel = filepath.ToSlash(rel)
		relPaths = append(relPaths, rel)

		index := filepath.Join(leaf, indexFileName(leafFormat(leaf)))
		if leafFormat(leaf) == formatAPK {
			if signed, err := adbSigned(index); err == nil && signed {
				signedCount++
			}
		} else if _, err := os.Stat(filepath.Join(leaf, "Packages.sig")); err == nil {
			signedCount++
		}

		stanzas, err := readIndexFile(index)
		if err != nil {
			continue
		}

		for _, st := range stanzas {
			if arch := st["Architecture"]; arch != "" && arch != archAll {
				if feedArches[rel] == nil {
					feedArches[rel] = map[string]bool{}
				}

				feedArches[rel][arch] = true
			}
		}
	}

	if len(relPaths) == 0 {
		return fmt.Errorf("%w: no feed indexes under %s — run build first", errIndex, cfg.OutputDir)
	}

	signed := signedCount == len(relPaths)
	fingerprint := ""

	if signed && cfg.Sign.PublicKey != "" {
		if data, err := os.ReadFile(cfg.Sign.PublicKey); err == nil {
			if fp, err := pubkeyFingerprint(string(data)); err == nil {
				fingerprint = fp
			}
		}
	}

	printRouterHelp(cfg, relPaths, feedArches, signed, fingerprint)

	return nil
}

// Serve serves the output tree over HTTP until ctx is cancelled.
func Serve(ctx context.Context, cfg *Config) error {
	return serve(ctx, cfg.OutputDir, cfg.Serve.Host, cfg.Serve.Port)
}

// GenkeyAPK generates the ECDSA P-256 keypair apk feeds (25.12+) are
// signed with — natively, no openssl or apk needed.
func GenkeyAPK(secret, public string) error {
	for _, dir := range []string{filepath.Dir(secret), filepath.Dir(public)} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create dir: %w", err)
		}
	}

	if err := genAPKKey(secret, public); err != nil {
		return err
	}

	fmt.Printf("apk secret key: %s\n", secret)
	fmt.Printf("apk public key: %s\n", public)
	fmt.Println("\nIn config.yaml set:")
	fmt.Println("  sign:")
	fmt.Println("    enabled: true")
	fmt.Printf("    apk_secret_key: %s\n", secret)
	fmt.Printf("    apk_public_key: %s\n", public)
	fmt.Println("\nOn a 25.12+ router install the public key (add.sh does this for you):")
	fmt.Printf("  scp %s root@router:/etc/apk/keys/<name>.pem\n", public)

	return nil
}

// Genkey generates the usign keypair opkg feeds are signed with.
func Genkey(ctx context.Context, secret, public string) error {
	if !usignAvailable() {
		return fmt.Errorf("%w not found. Install it first:\n"+
			"  Debian/Ubuntu: apt install signify-openbsd  (binary may be 'signify-openbsd')\n"+
			"  or build OpenWrt's usign: https://git.openwrt.org/project/usign.git\n"+
			"  on OpenWrt itself:  opkg install usign", errUsign)
	}

	for _, dir := range []string{filepath.Dir(secret), filepath.Dir(public)} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create dir: %w", err)
		}
	}

	cmd := command(ctx, "usign", "-G", "-s", secret, "-p", public)
	cmd.Stdout = os.Stdout

	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("usign -G: %w", err)
	}

	pubData, err := os.ReadFile(public)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	fingerprint, err := pubkeyFingerprint(string(pubData))
	if err != nil {
		return err
	}

	fmt.Printf("secret key: %s\n", secret)
	fmt.Printf("public key: %s\n", public)
	fmt.Printf("key id (router filename): %s\n", fingerprint)
	fmt.Println("\nIn config.yaml set:")
	fmt.Println("  sign:")
	fmt.Println("    enabled: true")
	fmt.Printf("    secret_key: %s\n", secret)
	fmt.Printf("    public_key: %s\n", public)
	fmt.Println("\nOn the router install the public key:")
	fmt.Printf("  scp %s root@router:/etc/opkg/keys/%s\n", public, fingerprint)

	return nil
}

// IndexDiff regenerates every feed index twice — natively and with the
// official ipkg-make-index.sh — and shows a unified diff per feed dir.
// Nothing on disk is touched. Known, expected deviations of the script:
// control fields pass through unstripped (Source*, Maintainer), no MD5Sum,
// kernel/libc packages skipped.
func IndexDiff(ctx context.Context, cfg *Config, script string) error {
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("%w: index script: %w", errIndex, err)
	}

	leafSet := map[string]bool{}

	_ = filepath.WalkDir(cfg.OutputDir, func(path string, d os.DirEntry, err error) error { // empty set reported below
		if err != nil || d.IsDir() {
			return err
		}

		if strings.HasSuffix(d.Name(), ".ipk") {
			leafSet[filepath.Dir(path)] = true
		}

		return nil
	})
	if len(leafSet) == 0 {
		return fmt.Errorf("%w: no feed dirs with .ipk files under %s", errIndex, cfg.OutputDir)
	}

	differing := 0

	for _, leaf := range sortedKeys(leafSet) {
		rel, _ := filepath.Rel(cfg.OutputDir, leaf)

		native, _, err := buildPackagesIndex(leaf)
		if err != nil {
			return fmt.Errorf("%s: native indexer: %w", rel, err)
		}

		scripted, _, err := scriptPackagesIndex(ctx, leaf, script)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}

		if native == scripted {
			fmt.Printf("== %s: identical\n", rel)
			continue
		}

		differing++

		fmt.Printf("== %s: differs\n", rel)

		tmp, err := os.MkdirTemp("", "feedbuilder-indexdiff-")
		if err != nil {
			return fmt.Errorf("create temp dir: %w", err)
		}

		nativePath := filepath.Join(tmp, "Packages.native")
		scriptPath := filepath.Join(tmp, "Packages.script")
		errN := writeFile(nativePath, []byte(native), filePerm)

		errS := writeFile(scriptPath, []byte(scripted), filePerm)
		if errN != nil || errS != nil {
			removeQuietly(tmp)

			return fmt.Errorf("write: %w", cmp.Or(errN, errS))
		}

		diff := command(ctx, "diff", "-u", "--label", rel+" (native)", nativePath,
			"--label", rel+" (ipkg-make-index.sh)", scriptPath)
		diff.Stdout = os.Stdout
		diff.Stderr = os.Stderr
		_ = diff.Run() // exit 1 just means "files differ"

		removeQuietly(tmp)
	}

	fmt.Printf("%d dir(s) compared, %d differ\n", len(leafSet), differing)

	return nil
}
