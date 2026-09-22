package feedbuilder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRepoHelpers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layout := Layout{
		Versions:    []string{tRel24106, tRel24107},
		DefaultFeed: tFeedPackages,
	}
	cfg := &Config{Layout: layout, BaseURL: "http://feed.example/openwrt", FeedPrefix: tFeedPrefix}

	scripts, err := writeRepoHelpers(dir, cfg, map[string][]string{tBranch24: {tFeedPackages, tFeedPrefix}},
		false, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(scripts) != 2 {
		t.Fatalf("expected one script per release, got %d", len(scripts))
	}

	for _, release := range layout.Versions {
		path := filepath.Join(dir, release, "add.sh")

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}

		script := string(data)

		// pinned to its own release, branch substituted, no leftover placeholders
		if !strings.Contains(script, `RELEASE="`+release+`"`) {
			t.Errorf("%s: not pinned to release %s", release, release)
		}

		if !strings.Contains(script, `BRANCH="24.10"`) {
			t.Errorf("%s: branch not substituted", release)
		}

		if strings.Contains(script, "@") {
			t.Errorf("%s: unsubstituted @PLACEHOLDER@ remains", release)
		}
		// feeds it wires up, labelled with the configured prefix; the branch
		// feed dirs come from the build (default + override), probed in a loop
		// with the all/ dir as a fallback
		for _, want := range []string{
			`for FEED in packages myfeed; do`,
			`add_feed "myfeed_${ARCH}_${FEED}"`,
			`add_feed "myfeed_all_${FEED}"`,
			"packages-$BRANCH/$ARCH/$FEED",
			"packages-$BRANCH/all/$FEED",
			`add_feed "myfeed_${ARCH}_kmods"`,
			`add_feed "myfeed_${ARCH}_targets"`,
			"$RELEASE/targets/$DISTRIB_TARGET/kmods/$KERNEL",
			"$RELEASE/targets/$DISTRIB_TARGET/packages",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: missing feed line %q", release, want)
			}
		}

		if strings.Contains(script, "custom_") {
			t.Errorf("%s: default 'custom' prefix leaked despite feed_prefix override", release)
		}
		// src/gz lines must be clean — a trailing comment is rejected by opkg
		// as garbage. The marker lives on its own BEGIN/END comment lines.
		if strings.Contains(script, `>> "$feeds"`) && strings.Contains(script, `src/gz $1 $2 #`) {
			t.Errorf("%s: src/gz line has a trailing comment (opkg garbage)", release)
		}

		for _, want := range []string{
			"# openwrt-feed-builder BEGIN",
			"# openwrt-feed-builder END",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: missing managed-block marker %q", release, want)
			}
		}
		// the removed per-release userspace feed must be gone
		if strings.Contains(script, "packages-$RELEASE") {
			t.Errorf("%s: still references dropped packages-$RELEASE feed", release)
		}

		// valid POSIX shell
		if out, err := command(t.Context(), "sh", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n failed: %v\n%s", release, err, out)
		}
	}
}

func TestWriteRepoHelpersAPK(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keys := t.TempDir()

	apkPub := filepath.Join(keys, "pub.pem")
	if err := genAPKKey(filepath.Join(keys, "priv.pem"), apkPub); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Layout:     Layout{Versions: []string{tRel24108, tRel25125}, DefaultFeed: tFeedPackages},
		BaseURL:    "http://feed.example/openwrt",
		FeedPrefix: tFeedPrefix,
		Sign:       SignConfig{APKPublicKey: apkPub},
	}

	_, err := writeRepoHelpers(dir, cfg, map[string][]string{
		tBranch24: {tFeedPackages}, tBranch25: {tFeedPackages, "extra"},
	}, true, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, apkPubName)); err != nil {
		t.Errorf("signed apk tree must serve %s: %v", apkPubName, err)
	}

	opkg, _ := os.ReadFile(filepath.Join(dir, tRel24108, "add.sh"))
	if !strings.Contains(string(opkg), "/etc/opkg/customfeeds.conf") ||
		!strings.Contains(string(opkg), `for FEED in packages; do`) {
		t.Error("24.10 add.sh must be the opkg flavor with its own branch feeds")
	}

	path := filepath.Join(dir, tRel25125, "add.sh")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	script := string(data)
	for _, want := range []string{
		`RELEASE="25.12.5"`,
		`BRANCH="25.12"`,
		`KEYNAME="myfeed.pem"`,
		`SIGNED="1"`,
		"/etc/apk/repositories.d/customfeeds.list",
		"/etc/apk/keys/$KEYNAME",
		"$BASE_URL/" + apkPubName,
		"/lib/apk/db/installed",
		`for FEED in packages extra; do`,
		`echo "$1/packages.adb" >> "$feeds"`,
		"$RELEASE/targets/$DISTRIB_TARGET/kmods/$KERNEL",
		"apk update",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("apk add.sh lacks %q", want)
		}
	}

	for _, bad := range []string{"@", "opkg", "src/gz"} {
		if strings.Contains(script, bad) {
			t.Errorf("apk add.sh contains %q", bad)
		}
	}

	if out, err := command(t.Context(), "sh", "-n", path).CombinedOutput(); err != nil {
		t.Errorf("sh -n failed: %v\n%s", err, out)
	}
}

// Both add.sh flavors must pass shellcheck as POSIX sh (skipped without it).
func TestAddSHShellcheck(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not installed")
	}

	dir := t.TempDir()
	cfg := &Config{Layout: Layout{Versions: []string{tRel24108, tRel25125}, DefaultFeed: tFeedPackages}}

	for _, feeds := range [][]string{{tFeedPackages}, {tFeedPackages, "extra"}} {
		for _, signed := range []bool{false, true} {
			scripts, err := writeRepoHelpers(dir, cfg,
				map[string][]string{tBranch24: feeds, tBranch25: feeds}, signed, "0123456789abcdef")
			if err != nil {
				t.Fatal(err)
			}

			for _, s := range scripts {
				out, err := command(t.Context(), "shellcheck", "-s", "sh", "-f", "gcc", s).CombinedOutput()
				if err != nil {
					t.Errorf("shellcheck %s (feeds %v, signed %v):\n%s", s, feeds, signed, out)
				}
			}
		}
	}
}
