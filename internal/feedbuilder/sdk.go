package feedbuilder

// Source type "sdk": compile packages from source with the official OpenWrt
// SDK, driven through an openwrt-buildroot checkout. Its `make pkg.build`
// target runs the SDK docker image for one release/target/subtarget and drops
// every built package (.ipk up to 24.10, .apk from 25.12 on) into a tmp dir
// this resolver then harvests, so the feed can carry packages (including
// kmods) that no upstream publishes as binaries.
//
// Example — AmneziaWG built from the package feed branch of
// Slava-Shchipunov/awg-openwrt:
//
//	- type: sdk
//	  name: amneziawg
//	  buildroot: ../openwrt-buildroot
//	  feeds:
//	    - src-git awg https://github.com/Slava-Shchipunov/awg-openwrt.git;feat/add-custom-feed
//	  packages: [kmod-amneziawg, amneziawg-tools, luci-proto-amneziawg]
//
// `builder:` picks the SDK flavor: "official" (default, the openwrt/sdk
// docker image, buildroot target pkg.build) or "src" (an SDK produced by the
// buildroot's own src build, target pkg.sdk.build — needs the SDK archive in
// the buildroot's artifacts, i.e. a prior `make src.all`). Use "src" when the
// target's kernel/config diverges from the official one (patched trees,
// custom vermagic).
//
// All sdk sources sharing a buildroot, an SDK flavor and a (release × target)
// pair are batched into ONE SDK container run: their `feeds:` lines are merged (keep
// feed names distinct across sources) and the union of their `packages:` is
// built in a single pass, so the expensive parts — cloning the feeds and
// packaging the kernel-module set — happen once per pair, not per source or
// per package. The matrix defaults to layout.version × the global targets
// list and can be narrowed per source with `releases:` / `targets:` (entries
// must be full "target/subtarget").
//
// Routing: kmods carry an exact kernel dependency and land under their
// release's <release>/targets/<t>/<st>/kmods/<kernel>/; plain userspace is
// ABI-stable within the branch and goes into the shared
// packages-<branch>/<arch>/ feed, visible to every point release.
// `release_bound: true` pins the userspace to the built release too (into its
// targets/<t>/<st>/packages/) — for tools that must ship in lockstep with
// their kmod.
//
// `include:` / `exclude:` globs (on the built Package name) select which of
// the harvested package files each source keeps; the default keeps `<pkg>*` for
// each entry of `packages:`, which covers subpackages but drops dependency
// packages the SDK built along the way (and, in a batched run, the other sources'
// packages).
//
// The buildroot needs a versions/<release>.mk for every release built (and
// docker + GNU make >= 4.4 on the host; `make:` overrides the make binary,
// default gmake when present). Results are cached per source under
// <cache>/sdk/ — a combination re-runs only with --refresh or when its cache
// dir is incomplete, mirroring how downloads are cached by URL; in a batched
// run only the sources that actually need building are included.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultArtifactsDir is the buildroot's artifacts directory (relative to its
// checkout), matching openwrt-buildroot's ARTIFACTS_DIR. The SDK build output
// (<artifacts>/pkg/<release>/tmp) and the generated feeds-extra file
// (<artifacts>/feedbuilder/) live under it; override per source with
// `artifacts_dir:` when the buildroot uses a different location.
const defaultArtifactsDir = "artifacts"

type sdkBuild struct {
	release, target, subtarget string
}

func (b sdkBuild) String() string {
	return fmt.Sprintf("%s %s/%s", b.release, b.target, b.subtarget)
}

// sdkMatrix expands the release × target combinations one sdk source builds.
// Defaults come from the global config (layout.version and targets); entries
// must be full "target/subtarget" pairs since the SDK needs both.
func sdkMatrix(cfg *Config, src Source) ([]sdkBuild, error) {
	releases := src.strSlice("releases")
	if len(releases) == 0 {
		releases = cfg.Layout.Versions
	}

	targets := src.strSlice("targets")
	if len(targets) == 0 {
		targets = cfg.Targets
	}

	var out []sdkBuild

	for _, release := range releases {
		for _, t := range targets {
			segs := strings.Split(strings.Trim(t, "/"), "/")
			if len(segs) != targetPathSegments {
				return nil, fmt.Errorf("%w needs full \"target/subtarget\" entries "+
					"(the SDK is target-specific), got %q — set a `targets:` list on the source", errSDKSource, t)
			}

			out = append(out, sdkBuild{release, segs[0], segs[1]})
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%w has no release/target combinations; set "+
			"layout.version and targets in the config, or `releases:`/`targets:` on the source", errSDKSource)
	}

	return out, nil
}

// sdkMakeBin picks the make binary: the source's `make:` override, else gmake
// (the buildroot needs GNU make >= 4.4, which macOS' /usr/bin/make is not).
func sdkMakeBin(src Source) string {
	if m := src.strOr("make", ""); m != "" {
		return m
	}

	if _, err := exec.LookPath("gmake"); err == nil {
		return "gmake"
	}

	return "make"
}

// sdkPlan is one validated sdk source, ready to be batched into builds.
type sdkPlan struct {
	name         string
	buildroot    string
	builder      string // "official" (openwrt/sdk image) or "src" (src-built SDK)
	artifactsDir string // buildroot's artifacts dir, relative to its checkout
	releaseBound bool   // pin userspace to the built release too (kmods always are)
	pkgs         []string
	includes     []string
	excludes     []string
	feeds        []string
	builds       []sdkBuild
	makeBin      string
	feedOverride string
}

// makeTarget is the buildroot target matching the chosen SDK flavor.
func (p *sdkPlan) makeTarget() string {
	if p.builder == "src" {
		return "pkg.sdk.build"
	}

	return "pkg.build"
}

func sdkPlanSource(cfg *Config, src Source) (*sdkPlan, error) {
	buildroot := src.strOr("buildroot", "")
	if buildroot == "" {
		return nil, fmt.Errorf("%w needs a `buildroot:` path (an openwrt-buildroot checkout)", errSDKSource)
	}

	if !filepath.IsAbs(buildroot) {
		buildroot = filepath.Join(cfg.BaseDir, buildroot)
	}

	if _, err := os.Stat(filepath.Join(buildroot, "Makefile")); err != nil {
		return nil, fmt.Errorf("`buildroot:` does not look like an openwrt-buildroot checkout: %w", err)
	}

	pkgs := src.strSlice("packages")
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("%w needs a `packages:` list", errSDKSource)
	}

	includes := src.strSlice("include")
	if len(includes) == 0 {
		for _, p := range pkgs {
			includes = append(includes, p+"*")
		}
	}

	builds, err := sdkMatrix(cfg, src)
	if err != nil {
		return nil, err
	}

	builder := src.strOr("builder", "official")
	if builder != "official" && builder != "src" {
		return nil, fmt.Errorf("%w `builder:` must be \"official\" (openwrt/sdk "+
			"image) or \"src\" (SDK produced by the buildroot's src build), got %q", errSDKSource, builder)
	}

	return &sdkPlan{
		name:         src.strOr("name", "sdk"),
		buildroot:    buildroot,
		builder:      builder,
		artifactsDir: src.strOr("artifacts_dir", defaultArtifactsDir),
		releaseBound: src.boolOr("release_bound", false),
		pkgs:         pkgs,
		includes:     includes,
		excludes:     src.strSlice("exclude"),
		feeds:        src.strSlice("feeds"),
		builds:       builds,
		makeBin:      sdkMakeBin(src),
		feedOverride: src.strOr("feed", ""),
	}, nil
}

// cacheDir is where one source's built packages for one build combination
// live; a .done marker inside distinguishes a completed build from the debris
// of an interrupted one.
func (p *sdkPlan) cacheDir(cache *Cache, b sdkBuild) string {
	return filepath.Join(cache.dir, "sdk", safeRE.ReplaceAllString(p.name, "_"),
		p.builder, b.release, b.target+"-"+b.subtarget)
}

// sdkCollect builds every enabled `type: sdk` source and returns the built
// package files as collected packages (paths point into the cache's sdk/
// directory). The second return value counts sources that failed (bad config,
// failed compile, nothing harvested) — each is skipped best-effort like any
// other source.
func sdkCollect(ctx context.Context, cfg *Config, cache *Cache, srcs []Source) ([]collectedPkg, int) {
	failures := 0

	var plans []*sdkPlan

	for _, src := range srcs {
		plan, err := sdkPlanSource(cfg, src)
		if err != nil {
			failures++

			warnf("%s: skipped: %v", src.strOr("name", "sdk"), err)

			continue
		}

		plans = append(plans, plan)
	}

	// Group the plans by buildroot + SDK flavor + artifacts dir + build
	// combination; each group is at most one SDK container run.
	type group struct {
		buildroot string
		build     sdkBuild
		members   []*sdkPlan
	}

	groups := map[string]*group{}

	var order []string

	for _, plan := range plans {
		for _, b := range plan.builds {
			key := plan.buildroot + "\x00" + plan.builder + "\x00" + plan.artifactsDir + "\x00" + b.String()

			grp, ok := groups[key]
			if !ok {
				grp = &group{buildroot: plan.buildroot, build: b}
				groups[key] = grp
				order = append(order, key)
			}

			grp.members = append(grp.members, plan)
		}
	}

	failedPlans := map[*sdkPlan]bool{}

	var out []collectedPkg

	for _, key := range order {
		grp := groups[key]

		var needed []*sdkPlan

		for _, plan := range grp.members {
			cache.mark(plan.cacheDir(cache, grp.build))

			_, err := os.Stat(filepath.Join(plan.cacheDir(cache, grp.build), ".done"))
			if cache.refresh || err != nil {
				needed = append(needed, plan)
			} else {
				sourceLinef(plan.name, "using cached build (%s)", grp.build)
			}
		}

		if len(needed) > 0 {
			for p, err := range sdkRunGroup(ctx, cache, grp.buildroot, grp.build, needed) {
				warnf("%s: failed (%s): %v", p.name, grp.build, err)

				if !failedPlans[p] {
					failedPlans[p] = true
					failures++
				}
			}
		}

		for _, plan := range grp.members {
			dir := plan.cacheDir(cache, grp.build)
			if _, err := os.Stat(filepath.Join(dir, ".done")); err != nil {
				continue
			}
			// a failed rebuild keeps the previous completed build on disk;
			// serve it (the feed stays complete) but say so
			if failedPlans[plan] {
				warnf("%s: build failed; keeping previous cached build (%s)", plan.name, grp.build)
			}

			ipks, _ := filepath.Glob(filepath.Join(dir, "*.ipk"))

			apks, _ := filepath.Glob(filepath.Join(dir, "*.apk"))
			for _, file := range append(ipks, apks...) {
				// kmods are pinned to the release they were compiled for
				// (exact kernel dependency); plain userspace is ABI-stable
				// within the branch and goes into the shared arch feed unless
				// the source opts into `release_bound: true`
				kmodVersion := grp.build.release

				if !plan.releaseBound {
					if fields, _, err := readPkgFields(file); err == nil && !isKmod(fields) {
						kmodVersion = ""
					}
				}

				out = append(out, collectedPkg{
					url: "sdk://" + plan.name + "/" + grp.build.release + "/" +
						grp.build.target + "/" + grp.build.subtarget + "/" + filepath.Base(file),
					path:         file,
					feedOverride: plan.feedOverride,
					sourceTarget: grp.build.target + "/" + grp.build.subtarget,
					kmodVersion:  kmodVersion,
					branch:       releaseBranch(grp.build.release),
				})
			}
		}
	}

	return out, failures
}

// sdkRunGroup runs one SDK container build for every source in needed: their
// feeds are merged, the union of their packages is compiled in a single
// `make pkg.build` invocation, and each source then harvests its own packages
// from the shared build output. Returned map holds the per-source errors (a
// failed make fails them all; a failed harvest only that source).
func sdkRunGroup(
	ctx context.Context, cache *Cache, buildroot string, build sdkBuild, needed []*sdkPlan,
) map[*sdkPlan]error {
	errs := map[*sdkPlan]error{}
	failAll := func(err error) map[*sdkPlan]error {
		for _, p := range needed {
			errs[p] = err
		}

		return errs
	}

	names := make([]string, 0, len(needed))

	var feeds, pkgs []string

	seenFeed, seenPkg := map[string]bool{}, map[string]bool{}

	for _, plan := range needed {
		names = append(names, plan.name)
		for _, f := range plan.feeds {
			if !seenFeed[f] {
				seenFeed[f] = true
				feeds = append(feeds, f)
			}
		}

		for _, pk := range plan.pkgs {
			if !seenPkg[pk] {
				seenPkg[pk] = true
				pkgs = append(pkgs, pk)
			}
		}
	}

	label := strings.Join(names, "+")

	target := needed[0].makeTarget()
	args := []string{
		"-C", buildroot, target,
		"PKGS=" + strings.Join(pkgs, " "),
		"OWRT_RELEASE=" + build.release,
		"SRC_TARGET=" + build.target,
		"SRC_SUBTARGET=" + build.subtarget,
	}
	// The merged feed lines go through a file inside the buildroot (its
	// Makefile bind-mounts SDK_FEEDS_EXTRA relative to itself).
	if len(feeds) > 0 {
		rel := filepath.Join(needed[0].artifactsDir, "feedbuilder",
			safeRE.ReplaceAllString("feeds-"+build.release+"-"+build.target+"-"+build.subtarget, "_")+".conf")

		full := filepath.Join(buildroot, rel)
		if err := os.MkdirAll(filepath.Dir(full), dirPerm); err != nil {
			return failAll(err)
		}

		if err := writeFile(full, []byte(strings.Join(feeds, "\n")+"\n"), filePerm); err != nil {
			return failAll(err)
		}

		args = append(args, "SDK_FEEDS_EXTRA="+filepath.ToSlash(rel))
	}

	sourceLinef(label, "building %s (%s)", strings.Join(pkgs, " "), build)

	cmd := command(ctx, needed[0].makeBin, args...)
	cmd.Stdout = os.Stdout

	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return failAll(fmt.Errorf("%s %s (%s): %w — is versions/%s.mk "+
			"present in the buildroot?", needed[0].makeBin, target, build, err, build.release))
	}

	tmpDir := filepath.Join(buildroot, needed[0].artifactsDir, "pkg", build.release, "tmp")
	for _, plan := range needed {
		dir := plan.cacheDir(cache, build)
		if err := os.RemoveAll(dir); err != nil {
			errs[plan] = err
			continue
		}

		if err := os.MkdirAll(dir, dirPerm); err != nil {
			errs[plan] = err
			continue
		}

		kept, err := sdkHarvest(tmpDir, dir, plan.includes, plan.excludes)
		if err != nil {
			errs[plan] = err
			continue
		}

		if kept == 0 {
			errs[plan] = fmt.Errorf("%w: %s (%s) built no package matching %v; "+
				"widen the source's `include:` globs", errSDKSource, target, build, plan.includes)

			continue
		}

		if err := writeFile(filepath.Join(dir, ".done"), []byte("ok\n"), filePerm); err != nil {
			errs[plan] = err
		}
	}

	return errs
}

// sdkHarvest copies the .ipk / .apk files from one SDK run's tmp dir into
// dest, keeping only those whose Package name passes the include/exclude
// globs (the SDK builds dependency packages along the way — a plain glob on
// everything would drag those into the feed). Returns how many were kept.
func sdkHarvest(tmpDir, dest string, includes, excludes []string) (int, error) {
	var files []string

	err := filepath.WalkDir(tmpDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() && (strings.HasSuffix(d.Name(), ".ipk") || strings.HasSuffix(d.Name(), ".apk")) {
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("reading SDK build output %s: %w", tmpDir, err)
	}

	kept := 0

	for _, file := range files {
		fields, _, err := readPkgFields(file)
		if err != nil {
			problemf("skipping unreadable build output %s: %v", file, err)
			continue
		}

		pkg := fieldOr(fields, "Package", "")
		if !matchesInclExcl(pkg, includes, excludes) {
			continue
		}

		if err := copyFile(file, filepath.Join(dest, filepath.Base(file))); err != nil {
			return 0, err
		}

		kept++
	}

	return kept, nil
}
