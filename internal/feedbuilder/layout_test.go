package feedbuilder

import "testing"

func officialLayout() Layout {
	return Layout{
		Versions:    []string{"24.10.6", "24.10.7"},
		Branch:      "24.10",
		DefaultFeed: "packages",
	}
}

func TestOfficialDirUserspace(t *testing.T) {
	layout := officialLayout()
	fields := map[string]string{"Package": "amneziawg-tools", "Architecture": "mipsel_24kc"}

	cases := []struct {
		name, target, sourceRelease, want string
	}{
		// release-bound with a target -> the release's target packages dir,
		// like downloads.openwrt.org/releases/<release>/targets/<t>/<st>/packages/
		{"release-bound", "ramips/mt7621", "24.10.7", "24.10.7/targets/ramips/mt7621/packages"},
		{"unbound", "ramips/mt7621", "", "packages-24.10/mipsel_24kc/packages"},
		// release-bound but no detectable target -> branch-wide arch feed
		{"release-bound no target", "", "24.10.7", "packages-24.10/mipsel_24kc/packages"},
	}
	for _, c := range cases {
		got := relativeDir(fields, "mipsel_24kc", layout, "", c.target, c.sourceRelease)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOfficialDirKmod(t *testing.T) {
	layout := officialLayout()
	fields := map[string]string{
		"Package": "kmod-amneziawg",
		"Depends": "kernel (=6.6.141~f31f6f85a36836e510d64a18a9a5f1bf-r1), kmod-udptunnel4",
	}

	got := relativeDir(fields, "mipsel_24kc", layout, "", "ramips/mt7621", "24.10.7")
	want := "24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-f31f6f85a36836e510d64a18a9a5f1bf"
	if got != want {
		t.Errorf("kmod with vermagic: got %q, want %q", got, want)
	}

	// no kernel dependency -> the release's target packages feed
	noDep := map[string]string{"Package": "kmod-foo"}
	got = relativeDir(noDep, "mipsel_24kc", layout, "", "ramips/mt7621", "24.10.6")
	if want := "24.10.6/targets/ramips/mt7621/packages"; got != want {
		t.Errorf("kmod without vermagic: got %q, want %q", got, want)
	}

	// no target -> falls back to the branch-wide arch feed
	got = relativeDir(noDep, "mipsel_24kc", layout, "", "", "24.10.6")
	if want := "packages-24.10/mipsel_24kc/packages"; got != want {
		t.Errorf("kmod without target: got %q, want %q", got, want)
	}

	// unbound kmod -> default release (newest listed)
	got = relativeDir(fields, "mipsel_24kc", layout, "", "ramips/mt7621", "")
	want = "24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-f31f6f85a36836e510d64a18a9a5f1bf"
	if got != want {
		t.Errorf("kmod default release: got %q, want %q", got, want)
	}
}

func TestDetectTarget(t *testing.T) {
	cases := []struct{ file, arch, want string }{
		{"amneziawg-tools_v25.12.4_aarch64_cortex-a53_mediatek_mt7622.ipk",
			"aarch64_cortex-a53", "mediatek/mt7622"},
		{"kmod-foo_1.0_aarch64_cortex-a53_bcm4908_generic.ipk",
			"aarch64_cortex-a53", "bcm4908/generic"},
		{"luci-app-x_4.6.1_all.ipk", "all", ""},
		// a lone trailing token cannot form target/subtarget — must fall back
		// to "" (branch feed) rather than produce an unreachable half-path
		{"pkg_1.0_mipsel_24kc_mt7621.ipk", "mipsel_24kc", ""},
	}
	for _, c := range cases {
		if got := detectTarget(c.file, c.arch); got != c.want {
			t.Errorf("detectTarget(%q, %q) = %q, want %q", c.file, c.arch, got, c.want)
		}
	}
}

func TestKernelDirName(t *testing.T) {
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
	layout := officialLayout()
	cases := []struct{ tag, want string }{
		{"v24.10.7", "24.10.7"},
		{"24.10.6", "24.10.6"},
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
	layout := officialLayout()
	if got := layout.defaultRelease(); got != "24.10.7" {
		t.Errorf("defaultRelease() = %q, want newest listed 24.10.7", got)
	}
	layout.KmodVersion = "24.10.6"
	if got := layout.defaultRelease(); got != "24.10.6" {
		t.Errorf("defaultRelease() with explicit kmod_version = %q, want 24.10.6", got)
	}
}
