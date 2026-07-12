package feedbuilder

// Command line interface: build / serve / genkey.

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var safeRE = regexp.MustCompile(`[^A-Za-z0-9._+~-]`)

// splitOnly parses the --only flag value into patterns.
func splitOnly(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

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
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
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

// stageSecretKey runs a shell command that prints a usign secret key and writes
// its output to a private (0600) temp file, since usign reads the key from a
// path. It returns the path and a cleanup func that removes the file. The key
// never touches the output tree.
func stageSecretKey(command string) (string, func(), error) {
	out, err := exec.Command("sh", "-c", command).Output()
	if err != nil {
		return "", nil, fmt.Errorf("sign.secret_key_cmd failed: %w", err)
	}
	key := bytes.TrimRight(out, "\n")
	if len(key) == 0 {
		return "", nil, fmt.Errorf("sign.secret_key_cmd produced no output")
	}
	f, err := os.CreateTemp("", "ofb-secret-*.key")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if _, err := f.Write(append(key, '\n')); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

type collectedPkg struct {
	url          string
	path         string
	feedOverride string
	sourceTarget string
	kmodVersion  string // point release the source is bound to ("" = none/layout default)
}

// sameSize reports whether two files exist and have equal size.
func sameSize(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && sa.Size() == sb.Size()
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
func collectPackages(cfg *Config, client *Client, cache *Cache, only []string) ([]collectedPkg, int) {
	var collected []collectedPkg
	var sdkSrcs []Source
	failures := 0
	for _, src := range cfg.Sources {
		if len(only) > 0 && !onlyMatch(only, src.strOr("type", ""), src.strOr("name", "")) {
			fmt.Printf("[%s] skipped (--only)\n", src.strOr("name", src.strOr("type", "")))
			continue
		}
		for _, src := range expandSourceTags(src) {
			name := src.strOr("name", src.strOr("type", ""))
			if !src.boolOr("enabled", true) {
				fmt.Printf("[%s] skipped (disabled)\n", name)
				continue
			}
			if src.strOr("type", "") == "binary" {
				pkgs, err := binaryPackages(cfg, client, cache, src)
				if err != nil {
					failures++
					fmt.Fprintf(os.Stderr, "[%s] skipped: %v\n", name, err)
					continue
				}
				fmt.Printf("[%s] %d package(s) repacked\n", name, len(pkgs))
				collected = append(collected, pkgs...)
				continue
			}
			if src.strOr("type", "") == "sdk" {
				// gathered and built after the loop: sdk sources sharing a
				// buildroot and release/target batch into one container run
				sdkSrcs = append(sdkSrcs, src)
				continue
			}
			files, resolvedTag, err := iterIPKURLs(client, src)
			if err != nil {
				failures++
				fmt.Fprintf(os.Stderr, "[%s] skipped: %v\n", name, err)
				continue
			}
			kmodVersion := sourceRelease(cfg.Layout, src, resolvedTag, name)
			fmt.Printf("[%s] %d package url(s)\n", name, len(files))
			for _, rf := range files {
				path, err := cache.get(rf)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ! failed %s: %v\n", rf.url, err)
					continue
				}
				collected = append(collected, collectedPkg{
					url:          rf.url,
					path:         path,
					feedOverride: src.strOr("feed", ""),
					sourceTarget: src.strOr("target", ""),
					kmodVersion:  kmodVersion,
				})
			}
		}
	}
	if len(sdkSrcs) > 0 {
		pkgs, sdkFailures := sdkCollect(cfg, cache, sdkSrcs)
		failures += sdkFailures
		if len(pkgs) > 0 {
			fmt.Printf("[sdk] %d package(s) built from source\n", len(pkgs))
		}
		collected = append(collected, pkgs...)
	}
	return collected, failures
}

// sourceRelease determines the point release a source is bound to: an explicit
// `kmod_version:` wins, otherwise it is derived from the resolved release tag
// when that tag is one of the declared layout.version releases (v24.10.7 ->
// 24.10.7). It routes the source's kmods into <release>/... and its userspace
// into that release's target packages/. A tag that looks like a branch point
// release but is not declared triggers a warning and falls back to the default.
func sourceRelease(layout Layout, src Source, resolvedTag, name string) string {
	if v := src.strOr("kmod_version", ""); v != "" {
		return v
	}
	if v := kmodVersionFromTag(resolvedTag, layout); v != "" {
		return v
	}
	if layout.Branch != "" {
		if v := strings.TrimPrefix(resolvedTag, "v"); strings.HasPrefix(v, layout.Branch+".") {
			fmt.Fprintf(os.Stderr, "[%s] warning: tag %s looks like a %s point release "+
				"but is not listed in layout.version; its kmods go under %s\n",
				name, resolvedTag, layout.Branch, layout.defaultRelease())
		}
	}
	return ""
}

func cmdBuild(cfg *Config, refresh, full bool, only []string) int {
	client := newClient()
	cache, err := newCache(cfg.CacheDir, client, refresh)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if len(only) > 0 && full {
		fmt.Printf("note: --only %s with --full — the rebuilt feed will contain ONLY the "+
			"matching sources; run a full build without --only to restore the rest\n",
			strings.Join(only, ","))
	}

	// 1. Resolve every source to concrete .ipk URLs and fetch them (cached).
	collected, sourceFailures := collectPackages(cfg, client, cache, only)
	if len(collected) == 0 {
		if sourceFailures > 0 {
			fmt.Fprintf(os.Stderr, "nothing collected (%d source(s) failed); "+
				"check the messages above\n", sourceFailures)
		} else {
			fmt.Fprintln(os.Stderr, "nothing collected; check your sources")
		}
		return 1
	}

	// 2. Snapshot the previous feed (package -> version per dir) so we can
	//    report what changed at the end.
	//
	//    Default (incremental): merge into the existing output tree in place —
	//    unchanged packages are left alone (no copy, no re-index, no re-sign),
	//    nothing is ever removed. Packages superseded upstream accumulate
	//    until a --full build prunes them.
	//
	//    --full: the tree is rebuilt from scratch in a sibling staging dir and
	//    swapped into place only when complete, so the previous feed stays
	//    intact (and keeps being served) if the build dies halfway. Packages
	//    no longer produced by any source disappear.
	oldVersions := snapshotVersions(cfg.OutputDir)
	buildDir := cfg.OutputDir
	swapped := false
	if full {
		buildDir = cfg.OutputDir + ".building"
		if err := os.RemoveAll(buildDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() {
			if !swapped {
				os.RemoveAll(buildDir)
			}
		}()
	}
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// 3. Route each package into its layout directory.
	archFilter := toSet(cfg.Architectures)
	targetFilter := toSet(cfg.Targets)

	// 3a. Parse and filter every collected package, remembering which real
	//     architectures are present. In the official layout there is no all/
	//     directory (just like downloads.openwrt.org): Architecture:all packages
	//     are duplicated into every real architecture's feed instead.
	type parsedPkg struct {
		c                  collectedPkg
		fields             map[string]string
		arch, pkg, version string
		origName, target   string
	}
	var parsed []parsedPkg
	realArches := map[string]bool{}
	skipped := 0
	for _, c := range collected {
		control, err := readControl(c.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! not a valid .ipk, skipping %s: %v\n", c.url, err)
			continue
		}
		fields := parseFields(control)
		arch := fieldOr(fields, "Architecture", "unknown")
		pkg := fieldOr(fields, "Package", "unknown")
		version := fieldOr(fields, "Version", "0")
		if len(archFilter) > 0 && !archFilter[arch] && arch != "all" {
			skipped++
			continue
		}
		origName := stripQuery(lastPathPart(c.url))
		target := c.sourceTarget
		if target == "" {
			target = detectTarget(origName, arch)
		}
		if !targetAllowed(target, targetFilter) {
			skipped++
			continue
		}
		if arch != "all" {
			realArches[arch] = true
		}
		parsed = append(parsed, parsedPkg{c, fields, arch, pkg, version, origName, target})
	}
	for a := range archFilter {
		realArches[a] = true
	}
	allArches := sortedKeys(realArches) // dirs an Architecture:all package lands in

	// 3b. Route each parsed package into candidate destination(s), tracking the
	//     newest version seen per destination slot (same package in the same
	//     output dir). slotKey collapses re-published older versions from any
	//     source so only the latest survives below.
	type candidate struct {
		path, url          string
		arch, pkg, version string
		rel, dir, dest     string
		origName, slotKey  string
	}
	var cands []candidate
	warnedAllDir := false
	for _, p := range parsed {
		dirArches := []string{p.arch}
		// An Architecture:all package normally fans out into every real arch
		// feed (there is no all/ dir) — unless it is release-bound with a known
		// target, in which case it lands once in the release's target tree and
		// the arch plays no role in the path.
		releaseBound := p.c.kmodVersion != "" && p.target != ""
		if p.arch == "all" && !releaseBound {
			if len(allArches) > 0 {
				dirArches = allArches
			} else if !warnedAllDir {
				warnedAllDir = true
				fmt.Fprintf(os.Stderr, "warning: no real architecture known (no "+
					"`architectures:` filter and only Architecture:all packages); "+
					"they go under packages-%s/all/ — add.sh probes that dir as a "+
					"fallback, but consider setting `architectures:` so they are "+
					"fanned into real arch feeds\n", cfg.Layout.Branch)
			}
		}
		for _, dirArch := range dirArches {
			rel := relativeDir(p.fields, dirArch, cfg.Layout, p.c.feedOverride, p.target, p.c.kmodVersion)
			dir := filepath.Join(buildDir, rel)
			cands = append(cands, candidate{
				path: p.c.path, url: p.c.url, arch: p.arch, pkg: p.pkg, version: p.version,
				rel: filepath.ToSlash(rel), dir: dir,
				dest:     filepath.Join(dir, safeName(p.pkg, p.version, p.arch)),
				origName: p.origName, slotKey: dir + "\x00" + p.pkg,
			})
		}
	}

	// 3c. Group candidates per destination slot (same package in the same
	//     output dir) and ship the newest version of each, falling back to the
	//     next-newest when the newest one's file is unreadable or fails to
	//     copy. Equal-version duplicates are deduped (an all-arch copy next to
	//     a real-arch build, or the same file from two sources) or flagged as
	//     a collision (two different real builds sharing a dir).
	bySlot := map[string][]candidate{}
	var slotOrder []string
	for _, cand := range cands {
		if _, ok := bySlot[cand.slotKey]; !ok {
			slotOrder = append(slotOrder, cand.slotKey)
		}
		bySlot[cand.slotKey] = append(bySlot[cand.slotKey], cand)
	}
	sort.Strings(slotOrder)

	shaCache := map[string]string{} // source path -> sha256 (all-pkgs reuse one path)
	fileSha := func(path string) (string, error) {
		if s, ok := shaCache[path]; ok {
			return s, nil
		}
		s, err := sha256File(path)
		if err == nil {
			shaCache[path] = s
		}
		return s, err
	}

	leafDirs := map[string]bool{}
	feedArches := map[string]map[string]bool{} // rel feed dir -> real arches shipped there
	newVersions := map[string]string{}         // "<dir>|<pkg>" -> version (what we shipped)
	routed, superseded, unchanged := 0, 0, 0
	for _, slot := range slotOrder {
		group := bySlot[slot]
		// newest version first; within one version real-arch builds before the
		// fanned Architecture:all copy, so the more specific build wins
		sort.SliceStable(group, func(i, j int) bool {
			if c := compareVersion(group[i].version, group[j].version); c != 0 {
				return c > 0
			}
			if ai, aj := group[i].arch == "all", group[j].arch == "all"; ai != aj {
				return aj
			}
			return group[i].dest < group[j].dest
		})
		placedSha, placedVersion := "", ""
		for _, cand := range group {
			if placedSha != "" {
				if compareVersion(cand.version, placedVersion) < 0 {
					superseded++ // an older version of a package we kept
					continue
				}
				// Same version as the placed file. An Architecture:all package
				// rebuilt per target yields byte-different copies of the same
				// thing — keeping the first is correct (the official mirror
				// carries one), so dedupe silently. Warn only for two
				// different real builds sharing a dir.
				if sha, err := fileSha(cand.path); err == nil && sha != placedSha && cand.arch != "all" {
					fmt.Fprintf(os.Stderr, "  ! collision: %s already holds %s with different "+
						"content; skipping %s. Same package built differently by two "+
						"sources/releases; kept the first.\n",
						cand.rel, cand.pkg, cand.origName)
				}
				continue
			}
			sha, err := fileSha(cand.path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ! %v\n", err)
				continue // try the next-newest candidate instead of dropping the package
			}
			// Incremental build: a byte-identical file already at the
			// destination satisfies the slot without copying — and without
			// touching the dir, so its index/signature stay as they are.
			// Size compare first: hashing the destination is only worth it
			// when it could actually match.
			if !full && sameSize(cand.path, cand.dest) {
				if destSha, err := sha256File(cand.dest); err == nil && destSha == sha {
					placedSha, placedVersion = sha, cand.version
					if cand.arch != "all" {
						if feedArches[cand.rel] == nil {
							feedArches[cand.rel] = map[string]bool{}
						}
						feedArches[cand.rel][cand.arch] = true
					}
					newVersions[cand.rel+"|"+cand.pkg] = cand.version
					unchanged++
					continue
				}
			}
			if err := os.MkdirAll(cand.dir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "  ! %v\n", err)
				continue
			}
			if err := copyFile(cand.path, cand.dest); err != nil {
				fmt.Fprintf(os.Stderr, "  ! %v\n", err)
				continue
			}
			placedSha, placedVersion = sha, cand.version
			leafDirs[cand.dir] = true
			if cand.arch != "all" {
				if feedArches[cand.rel] == nil {
					feedArches[cand.rel] = map[string]bool{}
				}
				feedArches[cand.rel][cand.arch] = true
			}
			newVersions[cand.rel+"|"+cand.pkg] = cand.version
			routed++
		}
	}

	fmt.Printf("routed %d package(s) (%d already up to date), %d filtered out, "+
		"%d older version(s) dropped, %d feed dir(s) touched\n",
		routed, unchanged, skipped, superseded, len(leafDirs))

	// 4. Build (and optionally sign) the index in each leaf dir.
	doSign := cfg.Sign.Enabled
	if doSign && !usignAvailable() {
		fmt.Fprintln(os.Stderr, "warning: usign not found on PATH; writing unsigned feed")
		doSign = false
	}

	// usign needs the secret key as a file. When it comes from a command
	// (sign.secret_key_cmd, e.g. "pass show openwrt/key"), run it once and stage
	// the output in a private temp file removed at the end of the build.
	secretKey := cfg.Sign.SecretKey
	if doSign && cfg.Sign.SecretKeyCmd != "" {
		path, cleanup, err := stageSecretKey(cfg.Sign.SecretKeyCmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v; writing unsigned feed\n", err)
			doSign = false
		} else {
			defer cleanup()
			secretKey = path
		}
	}

	// A signed feed without a distributable public key breaks every router
	// that runs the installer (no repo.pub, no key id in add.sh), so a broken
	// public key fails the build instead of degrading silently. The previous
	// feed stays in place — the staged tree is discarded on return.
	fingerprint := ""
	if doSign {
		if cfg.Sign.PublicKey == "" {
			fmt.Fprintln(os.Stderr, "warning: sign.public_key not set; repo.pub and "+
				"the key install step in add.sh will be missing")
		} else {
			data, err := os.ReadFile(cfg.Sign.PublicKey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: cannot read sign.public_key: %v\n", err)
				return 1
			}
			fingerprint, err = pubkeyFingerprint(string(data))
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: sign.public_key: %v\n", err)
				return 1
			}
		}
	}

	leaves := sortedKeys(leafDirs)

	relSet := map[string]bool{}
	var relPaths []string
	indexFailures := 0
	for _, leaf := range leaves {
		n, err := writeIndex(leaf)
		if err != nil {
			indexFailures++
			fmt.Fprintf(os.Stderr, "  ! %v\n", err)
			continue
		}
		rel, _ := filepath.Rel(buildDir, leaf)
		rel = filepath.ToSlash(rel)
		relSet[rel] = true
		relPaths = append(relPaths, rel)
		line := fmt.Sprintf("  %s: %d package(s)", rel, n)
		if doSign {
			if err := signIndex(leaf, secretKey); err != nil {
				fmt.Fprintf(os.Stderr, "  ! sign failed for %s: %v\n", rel, err)
			} else {
				line += " [signed]"
			}
		}
		fmt.Println(line)
	}

	// A feed dir whose index failed to (re)generate is broken for routers; a
	// --full build must not swap such a tree over the previous good one, and
	// any build must exit non-zero so wrappers notice.
	if indexFailures > 0 && full {
		fmt.Fprintf(os.Stderr, "error: %d feed index(es) failed to build; "+
			"keeping the previous feed\n", indexFailures)
		return 1
	}

	// An incremental build only touched some dirs; the helpers and the router
	// help below must still describe the whole tree, so fold in the feed dirs
	// that were already there (from the pre-build index snapshot).
	if !full {
		for key := range oldVersions {
			if i := strings.LastIndex(key, "|"); i >= 0 {
				if rel := key[:i]; !relSet[rel] {
					relSet[rel] = true
					relPaths = append(relPaths, rel)
				}
			}
		}
		sort.Strings(relPaths)
	}

	// 5. Write repo.pub + per-release add.sh helpers, wired to the branch feed
	//    dirs this build actually produced.
	branchPrefix := "packages-" + cfg.Layout.Branch + "/"
	feedSet := map[string]bool{}
	for _, rel := range relPaths {
		if strings.HasPrefix(rel, branchPrefix) {
			if segs := strings.Split(rel, "/"); len(segs) >= 3 {
				feedSet[segs[2]] = true
			}
		}
	}
	scripts, err := writeRepoHelpers(
		buildDir, cfg.Layout, sortedKeys(feedSet),
		cfg.BaseURL, cfg.FeedPrefix, doSign, fingerprint, cfg.Sign.PublicKey,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, s := range scripts {
		rel, _ := filepath.Rel(buildDir, s)
		fmt.Printf("  wrote %s\n", filepath.ToSlash(rel))
	}
	if doSign && fingerprint != "" {
		fmt.Println("  wrote repo.pub")
	}
	if cfg.BaseURL == "" {
		fmt.Println("  (set 'base_url' in config so add.sh embeds the real URL; " +
			"using HOST:PORT placeholder)")
	}

	// 6. --full only: swap the staged tree into place. The previous feed is
	//    replaced only by a complete build; the window a concurrent 'serve'
	//    can see a missing dir is the instant between the two renames.
	//    (Incremental builds worked in the output dir directly.)
	if full {
		oldDir := cfg.OutputDir + ".old"
		if err := os.RemoveAll(oldDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		haveOld := false
		if _, err := os.Stat(cfg.OutputDir); err == nil {
			if err := os.Rename(cfg.OutputDir, oldDir); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			haveOld = true
		}
		if err := os.Rename(buildDir, cfg.OutputDir); err != nil {
			if haveOld {
				os.Rename(oldDir, cfg.OutputDir) // put the previous feed back
			}
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		swapped = true
		os.RemoveAll(oldDir)
	}

	reportChanges(oldVersions, newVersions, full)

	printRouterHelp(cfg, relPaths, feedArches, doSign, fingerprint)
	if indexFailures > 0 {
		fmt.Fprintf(os.Stderr, "error: %d feed index(es) failed to rebuild (see above)\n", indexFailures)
		return 1
	}
	return 0
}

// snapshotVersions reads every existing Packages index under root and returns a
// map of "<dir>|<package>" -> version, where <dir> is the index directory
// relative to root. Used to diff what changed after a rebuild. A missing or
// unreadable feed yields an empty map.
func snapshotVersions(root string) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "Packages" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		rel = filepath.ToSlash(rel)
		for _, st := range parseIndex(string(data)) {
			if pkg := st["Package"]; pkg != "" {
				out[rel+"|"+pkg] = st["Version"]
			}
		}
		return nil
	})
	return out
}

// reportChanges prints, at the end of a build, what changed versus the previous
// feed: packages added, upgraded, downgraded or removed (with versions). Keys
// are "<dir>|<package>". Removals only exist in a --full rebuild — an
// incremental build never deletes anything, so entries missing from cur
// (sources filtered by --only, failed, or simply unchanged-and-untouched)
// are still on disk and are not reported.
func reportChanges(prev, cur map[string]string, full bool) {
	type change struct{ loc, pkg, from, to string }
	var added, removed, upgraded, downgraded []change
	split := func(k string) (string, string) {
		if i := strings.LastIndex(k, "|"); i >= 0 {
			return k[:i], k[i+1:]
		}
		return "", k
	}
	for k, nv := range cur {
		loc, pkg := split(k)
		ov, ok := prev[k]
		if !ok {
			added = append(added, change{loc, pkg, "", nv})
			continue
		}
		switch c := compareVersion(nv, ov); {
		case c > 0:
			upgraded = append(upgraded, change{loc, pkg, ov, nv})
		case c < 0:
			downgraded = append(downgraded, change{loc, pkg, ov, nv})
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

	if len(added)+len(upgraded)+len(downgraded)+len(removed) == 0 {
		fmt.Println("\nchanges: none (package set identical to previous build)")
		return
	}
	sortCh := func(s []change) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].loc != s[j].loc {
				return s[i].loc < s[j].loc
			}
			return s[i].pkg < s[j].pkg
		})
	}
	sortCh(upgraded)
	sortCh(downgraded)
	sortCh(added)
	sortCh(removed)

	fmt.Printf("\nchanges: %d new, %d updated, %d downgraded, %d removed\n",
		len(added), len(upgraded), len(downgraded), len(removed))
	for _, c := range upgraded {
		fmt.Printf("  ~ %s/%s: %s -> %s\n", c.loc, c.pkg, c.from, c.to)
	}
	for _, c := range downgraded {
		fmt.Printf("  v %s/%s: %s -> %s  (downgrade)\n", c.loc, c.pkg, c.from, c.to)
	}
	for _, c := range added {
		fmt.Printf("  + %s/%s: %s\n", c.loc, c.pkg, c.to)
	}
	for _, c := range removed {
		fmt.Printf("  - %s/%s: %s\n", c.loc, c.pkg, c.from)
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
			t, ok := trees[key]
			if !ok {
				t = &tree{}
				trees[key] = t
				treeOrder = append(treeOrder, key)
			}
			if segs[4] == "kmods" {
				t.kmods = append(t.kmods, rel)
			} else {
				t.pkgs = append(t.pkgs, rel)
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
		t := trees[key]
		archSet := map[string]bool{}
		for _, rel := range append(append([]string{}, t.pkgs...), t.kmods...) {
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
		rels = append(rels, t.pkgs...)
		rels = append(rels, t.kmods...)
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

func printRouterHelp(cfg *Config, relPaths []string, feedArches map[string]map[string]bool, signed bool, fingerprint string) {
	fmt.Println("\n=== add this feed on the router (24.10 / opkg) ===")
	base := orDefault(cfg.BaseURL, "http://HOST:PORT")
	fmt.Println("Easiest — run the installer matching the router's release (auto-detects")
	fmt.Println("arch / target / kernel and adds only the feeds that exist):")
	for _, v := range cfg.Layout.Versions {
		fmt.Printf("  wget -qO- %s/%s/add.sh | sh\n", base, v)
	}
	fmt.Println("\nOr add lines manually to /etc/opkg/customfeeds.conf.")
	fmt.Println("Each block below is complete for one device — copy the one matching your")
	fmt.Println("router's release, target and architecture:")
	for _, b := range routerBlocks(relPaths, feedArches, cfg.Layout.Branch) {
		fmt.Printf("\n  # %s\n", b.title)
		for _, rel := range b.rels {
			fmt.Printf("  src/gz %s %s/%s\n", feedLabel(cfg.FeedPrefix, rel), base, rel)
		}
	}
	fmt.Println("\n(the tree mirrors downloads.openwrt.org/releases/: release-independent")
	fmt.Printf(" userspace packages in packages-%s/<arch>/<feed>/, release-bound ones in\n", cfg.Layout.Branch)
	fmt.Println(" <release>/targets/<target>/<subtarget>/packages/, kmods next to them under")
	fmt.Println(" kmods/<kernel>/ keyed by the exact kernel each kmod depends on; each")
	fmt.Println(" <release>/add.sh is pinned to its release and resolves arch / target /")
	fmt.Println(" kernel from the router.)")
	if signed {
		fp := orDefault(fingerprint, "<fingerprint>")
		fmt.Println("\nThe feed is signed. Install the PUBLIC key on the router:")
		fmt.Printf("  scp %s root@router:/etc/opkg/keys/%s\n", cfg.Sign.PublicKey, fp)
		if fingerprint == "" {
			fmt.Println("  (run 'genkey' or check the public key; the filename must be the key id)")
		}
	} else {
		fmt.Println("\nThe feed is unsigned. Either sign it (recommended) or, on the router,")
		fmt.Println("comment out 'option check_signature 1' in /etc/opkg.conf.")
	}
	fmt.Println("\nThen: opkg update && opkg install <package>")
}

func cmdServe(cfg *Config) int {
	if err := serve(cfg.OutputDir, cfg.Serve.Host, cfg.Serve.Port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdGenkey(secret, public string) int {
	if !usignAvailable() {
		fmt.Fprintln(os.Stderr,
			"usign not found. Install it first:\n"+
				"  Debian/Ubuntu: apt install signify-openbsd  (binary may be 'signify-openbsd')\n"+
				"  or build OpenWrt's usign: https://git.openwrt.org/project/usign.git\n"+
				"  on OpenWrt itself:  opkg install usign")
		return 1
	}

	for _, dir := range []string{filepath.Dir(secret), filepath.Dir(public)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	cmd := exec.Command("usign", "-G", "-s", secret, "-p", public)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	pubData, err := os.ReadFile(public)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fingerprint, err := pubkeyFingerprint(string(pubData))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
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
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, `feedbuilder — collect .ipk packages from HTTP sources and build a signed opkg custom feed.

usage:
  feedbuilder [-c config.yaml] build [--refresh] [--full] [--only TYPE_OR_NAME[,...]]
  feedbuilder [-c config.yaml] serve
  feedbuilder [-c config.yaml] genkey [--secret PATH] [--public PATH]
  feedbuilder --version`)
}

// Run executes the CLI with the given arguments (excluding the program name)
// and returns the process exit code.
func Run(argv []string) int {
	// Parse the global -c/--config option (which may precede the command) and
	// the --version flag, then dispatch on the command.
	config := "config.yaml"
	var rest []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--version":
			fmt.Printf("feedbuilder %s\n", appVersion)
			return 0
		case arg == "-h" || arg == "--help":
			usage()
			return 0
		case arg == "-c" || arg == "--config":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "error: -c/--config needs a value")
				return 2
			}
			i++
			config = argv[i]
		case strings.HasPrefix(arg, "-c="):
			config = strings.TrimPrefix(arg, "-c=")
		case strings.HasPrefix(arg, "--config="):
			config = strings.TrimPrefix(arg, "--config=")
		default:
			rest = argv[i:]
			i = len(argv)
		}
	}

	if len(rest) == 0 {
		usage()
		return 2
	}

	command := rest[0]
	cmdArgs := rest[1:]

	switch command {
	case "build":
		fs := flag.NewFlagSet("build", flag.ContinueOnError)
		refresh := fs.Bool("refresh", false, "ignore cache, re-download everything")
		full := fs.Bool("full", false, "rebuild the output tree from scratch "+
			"(default merges incrementally: unchanged packages untouched, nothing removed)")
		only := fs.String("only", "", "build only sources whose type or name matches "+
			"(comma-separated globs, e.g. \"sdk\" or \"amnezia*,ssclash\")")
		if err := fs.Parse(cmdArgs); err != nil {
			return 2
		}
		cfg, err := loadConfig(config)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return cmdBuild(cfg, *refresh, *full, splitOnly(*only))
	case "serve":
		cfg, err := loadConfig(config)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return cmdServe(cfg)
	case "genkey":
		fs := flag.NewFlagSet("genkey", flag.ContinueOnError)
		secret := fs.String("secret", "keys/secret.key", "secret key output path")
		public := fs.String("public", "keys/public.key", "public key output path")
		if err := fs.Parse(cmdArgs); err != nil {
			return 2
		}
		// genkey does not need a valid config, matching the Python lazy-import design.
		return cmdGenkey(*secret, *public)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", command)
		usage()
		return 2
	}
}
