package feedbuilder

import (
	"strings"
	"testing"
)

func officialLayout() Layout {
	return Layout{
		Versions:    []string{tRel24106, tRel24107},
		DefaultFeed: tFeedPackages,
	}
}

func TestOfficialDirUserspace(t *testing.T) {
	t.Parallel()

	layout := officialLayout()
	fields := map[string]string{tFieldPackage: "amneziawg-tools", "Architecture": tArchMipsel}

	cases := []struct {
		name, target, sourceRelease, want string
	}{
		// release-bound with a target -> the release's target packages dir,
		// like downloads.openwrt.org/releases/<release>/targets/<t>/<st>/packages/
		{"release-bound", tTargetMT7621, tRel24107, tTreeMT7621Pkgs},
		{"unbound", tTargetMT7621, "", tFeedMipsel},
		// release-bound but no detectable target -> branch-wide arch feed
		{"release-bound no target", "", tRel24107, tFeedMipsel},
	}
	for _, c := range cases {
		got := relativeDir(fields, tArchMipsel, layout, tBranch24, "", c.target, c.sourceRelease)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOfficialDirKmod(t *testing.T) {
	t.Parallel()

	layout := officialLayout()
	fields := map[string]string{
		tFieldPackage: "kmod-amneziawg",
		tFieldDepends: "kernel (=6.6.141~f31f6f85a36836e510d64a18a9a5f1bf-r1), kmod-udptunnel4",
	}

	got := relativeDir(fields, tArchMipsel, layout, tBranch24, "", tTargetMT7621, tRel24107)

	want := "24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-f31f6f85a36836e510d64a18a9a5f1bf"
	if got != want {
		t.Errorf("kmod with vermagic: got %q, want %q", got, want)
	}

	// no kernel dependency -> the release's target packages feed
	noDep := map[string]string{tFieldPackage: "kmod-foo"}

	got = relativeDir(noDep, tArchMipsel, layout, tBranch24, "", tTargetMT7621, tRel24106)
	if want := "24.10.6/targets/ramips/mt7621/packages"; got != want {
		t.Errorf("kmod without vermagic: got %q, want %q", got, want)
	}

	// no target -> falls back to the branch-wide arch feed
	got = relativeDir(noDep, tArchMipsel, layout, tBranch24, "", "", tRel24106)
	if want := tFeedMipsel; got != want {
		t.Errorf("kmod without target: got %q, want %q", got, want)
	}

	// unbound kmod -> default release (newest listed)
	got = relativeDir(fields, tArchMipsel, layout, tBranch24, "", tTargetMT7621, "")

	want = "24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-f31f6f85a36836e510d64a18a9a5f1bf"
	if got != want {
		t.Errorf("kmod default release: got %q, want %q", got, want)
	}
}

func TestDetectTarget(t *testing.T) {
	t.Parallel()

	cases := []struct{ file, arch, want string }{
		{
			"amneziawg-tools_v25.12.4_aarch64_cortex-a53_mediatek_mt7622.ipk",
			tArchA53, "mediatek/mt7622",
		},
		{
			"kmod-foo_1.0_aarch64_cortex-a53_bcm4908_generic.ipk",
			tArchA53, "bcm4908/generic",
		},
		{"luci-app-x_4.6.1_all.ipk", tArchAll, ""},
		// a lone trailing token cannot form target/subtarget — must fall back
		// to "" (branch feed) rather than produce an unreachable half-path
		{"pkg_1.0_mipsel_24kc_mt7621.ipk", tArchMipsel, ""},
	}
	for _, c := range cases {
		if got := detectTarget(c.file, c.arch); got != c.want {
			t.Errorf("detectTarget(%q, %q) = %q, want %q", c.file, c.arch, got, c.want)
		}
	}
}

func TestKernelDirName(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"6.6.141~f31f6f85a36836e510d64a18a9a5f1bf-r1", "6.6.141-1-f31f6f85a36836e510d64a18a9a5f1bf"},
		{"6.6.30-1-4a3c1cd3a4e65473dcdef01a32cbb597", "6.6.30-1-4a3c1cd3a4e65473dcdef01a32cbb597"},
	}
	for _, c := range cases {
		if got := kernelDirName(c.in); got != c.want {
			t.Errorf("kernelDirName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKmodVersionFromTag(t *testing.T) {
	t.Parallel()

	layout := officialLayout()

	cases := []struct{ tag, want string }{
		{"v24.10.7", tRel24107},
		{tRel24106, tRel24106},
		{"v24.10.8", ""}, // not declared in layout.version
		{"v1.4.2", ""},   // project version, not a release
		{"latest", ""},
	}
	for _, c := range cases {
		if got := kmodVersionFromTag(c.tag, layout); got != c.want {
			t.Errorf("kmodVersionFromTag(%q) = %q, want %q", c.tag, got, c.want)
		}
	}
}

func TestDefaultRelease(t *testing.T) {
	t.Parallel()

	layout := officialLayout()
	if got := layout.defaultRelease(tBranch24); got != tRel24107 {
		t.Errorf("defaultRelease() = %q, want newest listed 24.10.7", got)
	}

	layout.KmodVersion = tRel24106
	if got := layout.defaultRelease(tBranch24); got != tRel24106 {
		t.Errorf("defaultRelease() with explicit kmod_version = %q, want 24.10.6", got)
	}
}

func multiLayout() Layout {
	return Layout{
		Versions:    []string{tRel24107, tRel24108, tRel25124, tRel25125},
		DefaultFeed: tFeedPackages,
	}
}

func TestLayoutBranches(t *testing.T) {
	t.Parallel()

	layout := multiLayout()
	if got := strings.Join(layout.branches(), ","); got != "24.10,25.12" {
		t.Errorf("branches = %s", got)
	}

	if got := strings.Join(layout.formats(), ","); got != "apk,ipk" {
		t.Errorf("formats = %s", got)
	}

	for branch, want := range map[string]string{"23.05": "ipk", tBranch24: "ipk", tBranch25: "apk", "26.06": "apk"} {
		if got := branchFormat(branch); got != want {
			t.Errorf("branchFormat(%s) = %s, want %s", branch, got, want)
		}
	}

	if got := layout.defaultRelease(tBranch25); got != tRel25125 {
		t.Errorf("defaultRelease(25.12) = %s", got)
	}
	// kmod_version only overrides its own branch
	layout.KmodVersion = tRel24107
	if got := layout.defaultRelease(tBranch24); got != tRel24107 {
		t.Errorf("defaultRelease(24.10) with kmod_version = %s", got)
	}

	if got := layout.defaultRelease(tBranch25); got != tRel25125 {
		t.Errorf("defaultRelease(25.12) with a 24.10 kmod_version = %s", got)
	}
}

func TestRelativeDirAPK(t *testing.T) {
	t.Parallel()

	layout := multiLayout()
	kmod := map[string]string{
		tFieldPackage: "kmod-amneziawg",
		tFieldDepends: "kernel=6.12.94~5a6c1f71be683ae9980b15d3ce73e24d-r1, kmod-udptunnel4",
	}

	got := relativeDir(kmod, tArchA53, layout, tBranch25, "", tTargetFilogic, tRel25125)
	if want := "25.12.5/targets/mediatek/filogic/kmods/6.12.94-1-5a6c1f71be683ae9980b15d3ce73e24d"; got != want {
		t.Errorf("apk kmod: got %q, want %q", got, want)
	}

	user := map[string]string{tFieldPackage: "luci-app-x"}
	if got := relativeDir(user, tArchMipsel, layout, tBranch25, "", "", ""); got != "packages-25.12/mipsel_24kc/packages" {
		t.Errorf("apk userspace: got %q", got)
	}
	// an unbound kmod lands in the newest release of ITS branch
	got = relativeDir(kmod, tArchA53, layout, tBranch25, "", tTargetFilogic, "")
	if !strings.HasPrefix(got, "25.12.5/") {
		t.Errorf("unbound apk kmod: got %q", got)
	}
}

func TestKernelVermagicSpellings(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"kernel (= 6.6.141~abc-r1), kmod-foo": "6.6.141~abc-r1",
		"kernel=6.12.94~def-r1, kmod-foo":     "6.12.94~def-r1",
		"kmod-foo, kernel=6.12.94~def-r1":     "6.12.94~def-r1",
		"kmod-kernel-helper, libc":            "",
		"libkernel=1.0":                       "",
	}
	for deps, want := range cases {
		if got := kernelVermagic(map[string]string{tFieldDepends: deps}); got != want {
			t.Errorf("kernelVermagic(%q) = %q, want %q", deps, got, want)
		}
	}
}

func TestPackageBranches(t *testing.T) {
	t.Parallel()

	layout := multiLayout()

	cases := []struct {
		name   string
		c      collectedPkg
		format string
		want   string // comma-joined branches, "" = unroutable
	}{
		{"ipk by format", collectedPkg{}, formatIPK, tBranch24},
		{"apk by format", collectedPkg{}, formatAPK, tBranch25},
		{"release tag", collectedPkg{kmodVersion: tRel25124}, formatAPK, tBranch25},
		{"explicit branch", collectedPkg{branch: tBranch25}, formatAPK, tBranch25},
		{"format mismatch", collectedPkg{kmodVersion: tRel25124}, formatIPK, ""},
		{"unknown branch", collectedPkg{branch: "23.05"}, formatIPK, ""},
	}
	for _, tcase := range cases {
		got, why := packageBranches(layout, tcase.c, tcase.format)
		if strings.Join(got, ",") != tcase.want {
			t.Errorf("%s: got %v (%s), want %q", tcase.name, got, why, tcase.want)
		}

		if tcase.want == "" && why == "" {
			t.Errorf("%s: unroutable without a reason", tcase.name)
		}
	}
	// two opkg branches: an unpinned .ipk fans into both
	two := Layout{Versions: []string{"23.05.5", tRel24108}}
	if got, _ := packageBranches(two, collectedPkg{}, formatIPK); strings.Join(got, ",") != "23.05,24.10" {
		t.Errorf("fan-out over opkg branches: %v", got)
	}

	if _, why := packageBranches(two, collectedPkg{}, formatAPK); why == "" {
		t.Error("apk package without an apk branch must be unroutable")
	}
}

func TestDefaultPkgPattern(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		"*.ipk":    {tRel24108},
		"*.apk":    {tRel25125},
		"*.[ai]pk": {tRel24108, tRel25125},
	}
	for want, versions := range cases {
		if got := defaultPkgPattern(Layout{Versions: versions}); got != want {
			t.Errorf("defaultPkgPattern(%v) = %q, want %q", versions, got, want)
		}
	}
}

func TestFileNameAllowed(t *testing.T) {
	t.Parallel()

	arches := toSet([]string{tArchA53, tArchMipsel})
	targets := toSet([]string{tTargetMT7621, tTargetFilogic})
	both := toSet([]string{formatIPK, formatAPK})

	cases := []struct {
		name, knownArch, target string
		formats                 map[string]bool
		want                    bool
	}{
		{"kmod-amneziawg_v24.10.8_mipsel_24kc_ramips_mt7621.ipk", "", "", both, true},
		{"kmod-amneziawg_v24.10.8_aarch64_cortex-a53_mediatek_filogic.ipk", "", "", both, true},
		{"kmod-amneziawg_v24.10.8_aarch64_cortex-a53_mediatek_mt7622.ipk", "", "", both, false}, // target
		{"kmod-amneziawg_v24.10.8_x86_64_x86_64.ipk", "", "", both, false},                      // arch
		{"kmod-amneziawg_v24.10.8_mips_24kc_ath79_generic.ipk", "", "", both, false},            // mips != mipsel
		{"kmod-amneziawg_v24.10.8_mediatek_mt7622.ipk", "", tTargetMT7621, both, true},          // no arch in name
		{"beszel-agent-0.19.0-r1_aarch64_cortex-a53.apk", "", "", both, true},
		{"beszel-agent-0.19.0-r1_aarch64_cortex-a72.apk", "", "", both, false},
		{"beszel-agent-0.19.0-r1_arm_cortex-a7_neon-vfpv4.apk", "", "", both, false},
		{"luci-app-ssclash_4.7.1-r1_all.ipk", "", "", both, true},
		{"luci-app-ssclash-4.7.1-r1.apk", "", "", both, true}, // no arch: download and look
		{"luci-app-ssclash-4.7.1-r1.apk", "", "", toSet([]string{formatIPK}), false},
		{"luci-app-ssclash_4.7.1-r1_all.ipk", "", "", toSet([]string{formatAPK}), false},
		{"luci-app-ttyd-1.0.apk", tArchX86, "", both, false}, // arch from a feed index
		{"luci-app-ttyd-1.0.apk", tArchAll, "", both, true},
	}
	for _, c := range cases {
		if got := fileNameAllowed(c.name, c.knownArch, c.target, c.formats, arches, targets); got != c.want {
			t.Errorf("fileNameAllowed(%q, %q) = %v, want %v", c.name, c.knownArch, got, c.want)
		}
	}
	// no arch filter: only the format decides
	if !fileNameAllowed("foo_1_x86_64.ipk", "", "", both, nil, targets) {
		t.Error("without an architectures filter every arch passes")
	}
}
