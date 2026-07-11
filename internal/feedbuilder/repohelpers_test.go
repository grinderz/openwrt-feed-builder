package feedbuilder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRepoHelpers(t *testing.T) {
	dir := t.TempDir()
	layout := Layout{
		Versions:    []string{"24.10.6", "24.10.7"},
		Branch:      "24.10",
		DefaultFeed: "packages",
	}

	scripts, err := writeRepoHelpers(dir, layout, []string{"packages", "grinderz"},
		"http://feed.example/openwrt", "myfeed", false, "", "")
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
			`for FEED in packages grinderz; do`,
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
		if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n failed: %v\n%s", release, err, out)
		}
	}
}
