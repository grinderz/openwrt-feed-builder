package feedbuilder

// The `build` command, one method per step: collect, parse, route, place,
// prune, index + sign, write helpers, swap (--full), clean the cache.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BuildOptions are the `build` command-line flags.
type BuildOptions struct {
	Refresh, Full, Sign, Reindex bool
	IndexScript                  string
	Only                         []string
}

// parsedPkg is a collected package with its metadata, as routing sees it.
type parsedPkg struct {
	c                  collectedPkg
	fields             map[string]string
	arch, pkg, version string
	origName, target   string
	format             string
	branches           []string
}

// candidate is one destination a parsed package may be placed at.
type candidate struct {
	path, url          string
	arch, pkg, version string
	rel, dir, dest     string
	origName, slotKey  string
}

// buildRun is the state one build carries from step to step.
type buildRun struct {
	cfg      *Config
	opts     BuildOptions
	buildDir string

	leafDirs    map[string]bool            // feed dirs to (re)index
	feedArches  map[string]map[string]bool // rel feed dir -> real arches shipped there
	newVersions map[string]string          // "<dir>|<pkg>" -> version (what we shipped)
	prunedFiles []string

	skipped, routed, unchanged, superseded, pruned int
}

// Build runs the `build` command: collect every source, route the packages
// into the feed tree, index (and optionally sign) it, write the helpers.
func Build(ctx context.Context, cfg *Config, opts BuildOptions) error {
	client := newClient()

	cache, err := newCache(cfg.CacheDir, client, opts.Refresh)
	if err != nil {
		return err
	}

	if len(opts.Only) > 0 && opts.Full {
		fmt.Printf("note: --only %s with --full — the rebuilt feed will contain ONLY the "+
			"matching sources; run a full build without --only to restore the rest\n",
			strings.Join(opts.Only, ","))
	}

	// 1. Resolve every source to concrete package URLs and fetch them (cached).
	collected, sourceFailures, err := collect(ctx, cfg, client, cache, opts.Only)
	if err != nil {
		return err
	}

	// 2. Snapshot the previous feed (package -> version per dir) so we can
	//    report what changed at the end.
	//
	//    Default (incremental): merge into the existing output tree in place —
	//    unchanged packages are left alone (no copy, no re-index, no re-sign).
	//    Older versions of packages this run ships are pruned (step 3d);
	//    packages no longer produced by any source stay until a --full build.
	//
	//    --full: the tree is rebuilt from scratch in a sibling staging dir and
	//    swapped into place only when complete, so the previous feed stays
	//    intact (and keeps being served) if the build dies halfway. Packages
	//    no longer produced by any source disappear.
	oldVersions := snapshotVersions(cfg.OutputDir)
	run := &buildRun{
		cfg: cfg, opts: opts, buildDir: cfg.OutputDir,
		leafDirs: map[string]bool{}, feedArches: map[string]map[string]bool{},
		newVersions: map[string]string{},
	}
	swapped := false

	if opts.Full {
		run.buildDir = cfg.OutputDir + ".building"
		if err := os.RemoveAll(run.buildDir); err != nil {
			return fmt.Errorf("remove: %w", err)
		}

		defer func() {
			if !swapped {
				removeQuietly(run.buildDir)
			}
		}()
	}

	if err := os.MkdirAll(run.buildDir, dirPerm); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	// 3. Route each package into its layout directory.
	parsed, err := run.parse(ctx, collected)
	if err != nil {
		return err
	}

	run.place(run.candidates(parsed))
	run.prune()

	phasef("routed %d package(s) (%d already up to date), %d filtered out, "+
		"%d older version(s) dropped, %d superseded file(s) pruned, "+
		"%d feed dir(s) touched",
		run.routed, run.unchanged, run.skipped, run.superseded, run.pruned, len(run.leafDirs))

	// 4. Build the index in each touched leaf dir, signing when asked to.
	keys, err := run.signing(ctx)
	if err != nil {
		return err
	}

	if keys != nil {
		defer keys.close()
	}

	relPaths, indexFailures, err := run.index(ctx, keys)
	if err != nil {
		return err
	}
	// A feed dir whose index failed to (re)generate is broken for routers; a
	// --full build must not swap such a tree over the previous good one, and
	// any build must exit non-zero so wrappers notice.
	if indexFailures > 0 && opts.Full {
		return fmt.Errorf("%w: %d feed index(es) failed to build; "+
			"keeping the previous feed", errIndex, indexFailures)
	}

	// An incremental build only touched some dirs; the helpers and the router
	// help below must still describe the whole tree, so fold in the feed dirs
	// that were already there (from the pre-build index snapshot).
	if !opts.Full {
		relPaths = withSnapshotDirs(relPaths, oldVersions)
	}

	// 5. Write repo.pub / repo-apk.pem + per-release add.sh helpers, wired to
	//    the branch feed dirs this build actually produced.
	fingerprint := keys.keyID()
	if err := run.helpers(relPaths, keys != nil, fingerprint); err != nil {
		return err
	}

	// 6. --full only: swap the staged tree into place.
	if opts.Full {
		if err := swapTree(run.buildDir, cfg.OutputDir); err != nil {
			return err
		}

		swapped = true
	}

	reportChanges(oldVersions, run.newVersions, run.prunedFiles, opts.Full)

	// 7. Drop cache entries this run did not use (old versions' downloads and
	//    repacks, dropped sources). Only a run that visited every source
	//    knows what is still needed: --only runs and runs with failed
	//    sources keep the cache as is.
	switch {
	case len(opts.Only) > 0:
	case sourceFailures > 0 || indexFailures > 0:
		fmt.Println()
		phasef("cache: not cleaned %s", Dim("(some sources or indexes failed this run)"))
	default:
		cleanCache(cache)
	}

	printRouterHelp(cfg, relPaths, run.feedArches, keys != nil, fingerprint)

	if indexFailures > 0 {
		return fmt.Errorf("%w: %d feed index(es) failed to rebuild (see above)", errIndex, indexFailures)
	}

	return nil
}

// collect (step 1) fetches every source; an empty result is an error.
func collect(
	ctx context.Context, cfg *Config, client *Client, cache *Cache, only []string,
) ([]collectedPkg, int, error) {
	collected, failures := collectPackages(ctx, cfg, client, cache, only)
	if len(collected) > 0 {
		return collected, failures, nil
	}

	if failures > 0 {
		return nil, failures, fmt.Errorf("%w: nothing collected (%d source(s) failed); "+
			"check the messages above", errSource, failures)
	}

	return nil, failures, fmt.Errorf("%w: nothing collected; check your sources", errSource)
}

// parse (step 3a) reads and filters every collected package, remembering
// which real architectures are present. In the official layout there is no
// all/ directory (just like downloads.openwrt.org): Architecture:all packages
// are duplicated into every real architecture's feed instead. Each package
// also gets the branch(es) it is routed into (see packageBranches); one that
// fits no carried branch is skipped.
func (b *buildRun) parse(ctx context.Context, collected []collectedPkg) ([]parsedPkg, error) {
	archFilter := toSet(b.cfg.Architectures)
	targetFilter := toSet(b.cfg.Targets)
	unroutable := map[string]int{} // reason -> packages skipped for it

	var parsed []parsedPkg

	for _, item := range collected {
		fields, format, err := readPkgFields(item.path)
		if err != nil {
			problemf("not a valid .ipk/.apk package, skipping %s: %v", item.url, err)
			continue
		}

		branches, why := packageBranches(b.cfg.Layout, item, format)
		if len(branches) == 0 {
			unroutable[why]++
			b.skipped++

			continue
		}

		arch := fieldOr(fields, "Architecture", "unknown")
		if len(archFilter) > 0 && !archFilter[arch] && arch != archAll {
			b.skipped++
			continue
		}

		origName := stripQuery(lastPathPart(item.url))

		target := item.sourceTarget
		if target == "" {
			target = detectTarget(origName, arch)
		}

		if !targetAllowed(target, targetFilter) {
			b.skipped++
			continue
		}

		parsed = append(parsed, parsedPkg{
			c: item, fields: fields, arch: arch,
			pkg: fieldOr(fields, "Package", "unknown"), version: fieldOr(fields, "Version", "0"),
			origName: origName, target: target, format: format, branches: branches,
		})
	}

	for _, why := range sortedStringKeys(unroutable) {
		problemf("skipped %d package(s): %s", unroutable[why], why)
	}
	// apk feed dirs can only be indexed with apk-tools; find out before the
	// tree is touched — a package copied into a dir whose index then fails
	// would count as "up to date" on the next run and never get indexed
	for _, pkgInfo := range parsed {
		if pkgInfo.format == formatAPK {
			if err := b.cfg.APKTool.check(ctx); err != nil {
				return nil, err
			}

			break
		}
	}

	return parsed, nil
}

// candidates (step 3b) routes each parsed package into its destination(s).
// slotKey (same package in the same output dir) later collapses re-published
// older versions from any source so only the latest survives.
func (b *buildRun) candidates(parsed []parsedPkg) []candidate {
	realArches := toSet(b.cfg.Architectures)

	for _, pkgInfo := range parsed {
		if pkgInfo.arch != archAll {
			realArches[pkgInfo.arch] = true
		}
	}

	allArches := sortedKeys(realArches) // dirs an Architecture:all package lands in

	var cands []candidate

	warnedAllDir := false

	for _, pkgInfo := range parsed {
		dirArches := []string{pkgInfo.arch}
		// An Architecture:all package normally fans out into every real arch
		// feed (there is no all/ dir) — unless it is release-bound with a known
		// target, in which case it lands once in the release's target tree and
		// the arch plays no role in the path.
		releaseBound := pkgInfo.c.kmodVersion != "" && pkgInfo.target != ""
		if pkgInfo.arch == archAll && !releaseBound {
			if len(allArches) > 0 {
				dirArches = allArches
			} else if !warnedAllDir {
				warnedAllDir = true

				warnf("%s", "no real architecture known (no "+
					"`architectures:` filter and only Architecture:all packages); "+
					"they go under packages-<branch>/all/ — add.sh probes that dir as a "+
					"fallback, but consider setting `architectures:` so they are "+
					"fanned into real arch feeds")
			}
		}

		for _, branch := range pkgInfo.branches {
			for _, dirArch := range dirArches {
				cands = append(cands, b.candidate(pkgInfo, branch, dirArch))
			}
		}
	}

	return cands
}

func (b *buildRun) candidate(pkgInfo parsedPkg, branch, dirArch string) candidate {
	rel := relativeDir(pkgInfo.fields, dirArch, b.cfg.Layout, branch,
		pkgInfo.c.feedOverride, pkgInfo.target, pkgInfo.c.kmodVersion)
	dir := filepath.Join(b.buildDir, rel)

	return candidate{
		path: pkgInfo.c.path, url: pkgInfo.c.url,
		arch: pkgInfo.arch, pkg: pkgInfo.pkg, version: pkgInfo.version,
		rel: filepath.ToSlash(rel), dir: dir,
		dest:     filepath.Join(dir, pkgFileName(pkgInfo.format, pkgInfo.pkg, pkgInfo.version, pkgInfo.arch)),
		origName: pkgInfo.origName, slotKey: dir + "\x00" + pkgInfo.pkg,
	}
}

// place (step 3c) groups candidates per destination slot (same package in
// the same output dir) and ships the newest version of each, falling back to
// the next-newest when the newest one's file is unreadable or fails to copy.
// Equal-version duplicates are deduped (an all-arch copy next to a real-arch
// build, or the same file from two sources) or flagged as a collision (two
// different real builds sharing a dir).
func (b *buildRun) place(cands []candidate) {
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
		if sum, ok := shaCache[path]; ok {
			return sum, nil
		}

		sum, err := sha256File(path)
		if err == nil {
			shaCache[path] = sum
		}

		return sum, err
	}

	for _, slot := range slotOrder {
		b.placeSlot(bySlot[slot], fileSha)
	}
}

// placeSlot ships the newest usable candidate of one slot.
func (b *buildRun) placeSlot(group []candidate, fileSha func(string) (string, error)) {
	// newest version first; within one version real-arch builds before the
	// fanned Architecture:all copy, so the more specific build wins
	sort.SliceStable(group, func(left, right int) bool {
		if c := compareVersion(group[left].version, group[right].version); c != 0 {
			return c > 0
		}

		if ai, aj := group[left].arch == archAll, group[right].arch == archAll; ai != aj {
			return aj
		}

		return group[left].dest < group[right].dest
	})

	placedSha, placedVersion := "", ""
	for _, cand := range group {
		if placedSha != "" {
			if compareVersion(cand.version, placedVersion) < 0 {
				b.superseded++ // an older version of a package we kept
				continue
			}
			// Same version as the placed file. An Architecture:all package
			// rebuilt per target yields byte-different copies of the same
			// thing — keeping the first is correct (the official mirror
			// carries one), so dedupe silently. Warn only for two
			// different real builds sharing a dir.
			if sha, err := fileSha(cand.path); err == nil && sha != placedSha && cand.arch != archAll {
				problemf("collision: %s already holds %s with different "+
					"content; skipping %s. Same package built differently by two "+
					"sources/releases; kept the first.",
					cand.rel, cand.pkg, cand.origName)
			}

			continue
		}

		sha, err := fileSha(cand.path)
		if err != nil {
			problemf("%v", err)
			continue // try the next-newest candidate instead of dropping the package
		}

		placed, err := b.placeFile(cand, sha)
		if err != nil {
			problemf("%v", err)
			continue
		}

		placedSha, placedVersion = sha, cand.version

		if placed {
			b.leafDirs[cand.dir] = true
			b.routed++
		} else {
			b.unchanged++
		}

		if cand.arch != archAll {
			if b.feedArches[cand.rel] == nil {
				b.feedArches[cand.rel] = map[string]bool{}
			}

			b.feedArches[cand.rel][cand.arch] = true
		}

		b.newVersions[cand.rel+"|"+cand.pkg] = cand.version
	}
}

// placeFile copies a candidate to its destination; false means a
// byte-identical file already sits there (incremental builds only), which
// satisfies the slot without touching the dir, so its index/signature stay
// as they are. Size compare first: hashing the destination is only worth it
// when it could actually match.
func (b *buildRun) placeFile(cand candidate, sha string) (bool, error) {
	if !b.opts.Full && sameSize(cand.path, cand.dest) {
		if destSha, err := sha256File(cand.dest); err == nil && destSha == sha {
			return false, nil
		}
	}

	if err := os.MkdirAll(cand.dir, dirPerm); err != nil {
		return false, fmt.Errorf("create dir: %w", err)
	}

	if err := copyFile(cand.path, cand.dest); err != nil {
		return false, err
	}

	return true, nil
}

// prune (step 3d) removes superseded package files: for every package this
// run shipped (or confirmed in place), older versions of it in the same dir
// go. Packages from sources not part of this run keep all their files. A
// prune in a dir that was otherwise untouched marks it for re-indexing, so
// the index never references a removed file.
func (b *buildRun) prune() {
	keepByDir := map[string]map[string]string{} // dir -> pkg -> version kept

	for key, ver := range b.newVersions {
		if sep := strings.LastIndex(key, "|"); sep >= 0 {
			dir := filepath.Join(b.buildDir, filepath.FromSlash(key[:sep]))
			if keepByDir[dir] == nil {
				keepByDir[dir] = map[string]string{}
			}

			keepByDir[dir][key[sep+1:]] = ver
		}
	}

	for _, dir := range sortedStringKeys(keepByDir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() && supersededFile(dir, entry.Name(), keepByDir[dir]) {
				b.pruneFile(dir, entry.Name())
			}
		}
	}
}

// supersededFile reports whether a package file in dir is an older version
// of a package the run keeps.
func supersededFile(dir, name string, keep map[string]string) bool {
	// <pkg>_<ver>_<arch>.ipk / <pkg>-<ver>.apk
	var sep string

	switch {
	case strings.HasSuffix(name, ".ipk"):
		sep = "_"
	case strings.HasSuffix(name, ".apk"):
		sep = "-"
	default:
		return false
	}
	// cheap filename filter first; the package metadata decides
	candidate := false

	for pkg := range keep {
		if strings.HasPrefix(name, pkg+sep) {
			candidate = true
			break
		}
	}

	if !candidate {
		return false
	}

	fields, _, err := readPkgFields(filepath.Join(dir, name))
	if err != nil {
		return false
	}

	ver, ok := keep[fields["Package"]]

	return ok && compareVersion(fields["Version"], ver) < 0
}

func (b *buildRun) pruneFile(dir, name string) {
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		problemf("prune failed: %v", err)
		return
	}

	b.pruned++
	rel, _ := filepath.Rel(b.buildDir, dir)
	b.prunedFiles = append(b.prunedFiles, filepath.ToSlash(rel)+"/"+name)
	fmt.Println(Dim(fmt.Sprintf("  - pruned %s/%s", filepath.ToSlash(rel), name)))

	b.leafDirs[dir] = true
}

// signing prepares the keys when the build signs (opt-in, --sign); without
// it run the `sign` command afterwards — it signs every index and bakes the
// keys into repo.pub / repo-apk.pem / add.sh. nil keys mean unsigned.
//
// Secret keys coming from a command (sign.secret_key_cmd, e.g. "pass show
// openwrt/key") run once and are staged in private temp files removed at the
// end of the build. A signed feed without a distributable public key breaks
// every router that runs the installer, so a broken public key fails the
// build instead of degrading silently. The previous feed stays in place — the
// staged tree is discarded on return.
func (b *buildRun) signing(ctx context.Context) (*signKeys, error) {
	if !b.opts.Sign {
		return nil, nil //nolint:nilnil // unsigned build: no keys, no error
	}

	if !b.cfg.Sign.Enabled {
		warnf("%s", "--sign but sign.enabled is false in the config; "+
			"writing unsigned feed")

		return nil, nil //nolint:nilnil // unsigned build: no keys, no error
	}

	keys, err := prepareSignKeys(ctx, b.cfg, b.cfg.Layout.formats())
	if errors.Is(err, errSignUnavailable) {
		warnf("%v; writing unsigned feed", err)
		return nil, nil //nolint:nilnil // degraded to an unsigned build
	}

	return keys, err
}

// index (step 4) writes (and, with keys, signs) the index of every touched
// feed dir — every feed dir with --reindex. It returns the feed dirs indexed
// and how many failed.
func (b *buildRun) index(ctx context.Context, keys *signKeys) ([]string, int, error) {
	// --reindex: regenerate the index of every existing feed dir, not just the
	// touched ones — needed when the index format itself changes.
	if b.opts.Reindex {
		err := filepath.WalkDir(b.buildDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}

			if strings.HasSuffix(d.Name(), ".ipk") || strings.HasSuffix(d.Name(), ".apk") {
				b.leafDirs[filepath.Dir(path)] = true
			}

			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("--reindex: %w", err)
		}
	}

	leaves := sortedKeys(b.leafDirs)

	// apk dirs need apk-tools v3 for the index; check it once, up front
	var apkErr error

	for _, leaf := range leaves {
		if leafFormat(leaf) == formatAPK {
			apkErr = b.cfg.APKTool.check(ctx)
			break
		}
	}

	var relPaths []string

	failures := 0

	for _, leaf := range leaves {
		rel, _ := filepath.Rel(b.buildDir, leaf)
		rel = filepath.ToSlash(rel)

		if leafFormat(leaf) == formatAPK && apkErr != nil {
			failures++

			problemf("%s: %v", rel, apkErr)

			continue
		}

		if err := b.indexLeaf(ctx, keys, leaf, rel); err != nil {
			failures++

			problemf("%v", err)

			continue
		}

		relPaths = append(relPaths, rel)
	}

	return relPaths, failures, nil
}

func (b *buildRun) indexLeaf(ctx context.Context, keys *signKeys, leaf, rel string) error {
	count, err := writeIndex(ctx, leaf, b.opts.IndexScript, b.cfg.APKTool)
	if err != nil {
		return err
	}

	line := fmt.Sprintf("  %s: %d package(s)", rel, count)

	if keys != nil {
		if err := keys.signLeaf(ctx, b.cfg, leaf); err != nil {
			problemf("sign failed for %s: %v", rel, err)
		} else {
			line += " " + Green("[signed]")
		}
	} else {
		// the index just changed, so a signature from a previous signed
		// run no longer matches — a stale .sig is worse than none (opkg
		// errors out instead of taking the unsigned-feed path). apk keeps
		// its signature inside packages.adb, which mkndx just rewrote.
		removeQuietly(filepath.Join(leaf, "Packages.sig"))
	}

	fmt.Println(line)

	return nil
}

// helpers (step 5) writes the repo helpers for the feed dirs.
func (b *buildRun) helpers(relPaths []string, signed bool, fingerprint string) error {
	if err := writeHelpers(b.buildDir, b.cfg, relPaths, signed, fingerprint); err != nil {
		return err
	}

	if b.cfg.BaseURL == "" {
		fmt.Println(Dim("  (set 'base_url' in config so add.sh embeds the real URL; " +
			"using HOST:PORT placeholder)"))
	}

	return nil
}

// withSnapshotDirs adds the feed dirs of the pre-build snapshot to relPaths.
func withSnapshotDirs(relPaths []string, oldVersions map[string]string) []string {
	relSet := toSet(relPaths)

	for key := range oldVersions {
		if sep := strings.LastIndex(key, "|"); sep >= 0 {
			if rel := key[:sep]; !relSet[rel] {
				relSet[rel] = true
				relPaths = append(relPaths, rel)
			}
		}
	}

	sort.Strings(relPaths)

	return relPaths
}

// swapTree (step 6, --full) moves the staged tree into place. The previous
// feed is replaced only by a complete build; the window a concurrent 'serve'
// can see a missing dir is the instant between the two renames.
func swapTree(buildDir, outputDir string) error {
	oldDir := outputDir + ".old"
	if err := os.RemoveAll(oldDir); err != nil {
		return fmt.Errorf("remove: %w", err)
	}

	haveOld := false

	if _, err := os.Stat(outputDir); err == nil {
		if err := os.Rename(outputDir, oldDir); err != nil {
			return fmt.Errorf("rename: %w", err)
		}

		haveOld = true
	}

	if err := os.Rename(buildDir, outputDir); err != nil {
		if haveOld {
			_ = os.Rename(oldDir, outputDir) // best effort: put the previous feed back
		}

		return fmt.Errorf("rename: %w", err)
	}

	removeQuietly(oldDir)

	return nil
}

// cleanCache (step 7) drops the cache entries the run did not use.
func cleanCache(cache *Cache) {
	count, freed, err := cache.gc()
	if err != nil {
		warnf("cache cleanup: %v", err)
	} else if count > 0 {
		fmt.Println()
		phasef("cache: removed %d unused entr(ies), %.1f MiB freed", count, float64(freed)/mib)
	}
}
