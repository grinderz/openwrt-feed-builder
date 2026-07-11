package feedbuilder

import (
	"strings"
	"testing"
)

func TestRouterBlocks(t *testing.T) {
	relPaths := []string{
		"24.10.7/targets/mediatek/filogic/kmods/6.6.141-1-abc",
		"24.10.7/targets/mediatek/filogic/packages",
		"24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-def",
		"24.10.7/targets/ramips/mt7621/packages",
		"packages-24.10/aarch64_cortex-a53/packages",
		"packages-24.10/mipsel_24kc/packages",
		"packages-24.10/x86_64/packages", // no target tree ships this arch
	}
	feedArches := map[string]map[string]bool{
		"24.10.7/targets/mediatek/filogic/kmods/6.6.141-1-abc": {"aarch64_cortex-a53": true},
		"24.10.7/targets/mediatek/filogic/packages":            {"aarch64_cortex-a53": true},
		"24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-def":    {"mipsel_24kc": true},
		"24.10.7/targets/ramips/mt7621/packages":               {"mipsel_24kc": true},
		"packages-24.10/aarch64_cortex-a53/packages":           {"aarch64_cortex-a53": true},
		"packages-24.10/mipsel_24kc/packages":                  {"mipsel_24kc": true},
		"packages-24.10/x86_64/packages":                       {"x86_64": true},
	}

	blocks := routerBlocks(relPaths, feedArches, "24.10")
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d: %+v", len(blocks), blocks)
	}

	filogic := blocks[0]
	if !strings.Contains(filogic.title, "mediatek/filogic") ||
		!strings.Contains(filogic.title, "aarch64_cortex-a53") {
		t.Errorf("bad filogic title: %q", filogic.title)
	}
	want := []string{
		"packages-24.10/aarch64_cortex-a53/packages", // branch feed comes first
		"24.10.7/targets/mediatek/filogic/packages",
		"24.10.7/targets/mediatek/filogic/kmods/6.6.141-1-abc",
	}
	if len(filogic.rels) != len(want) {
		t.Fatalf("filogic rels = %v, want %v", filogic.rels, want)
	}
	for i, rel := range want {
		if filogic.rels[i] != rel {
			t.Errorf("filogic.rels[%d] = %q, want %q", i, filogic.rels[i], rel)
		}
	}

	mt7621 := blocks[1]
	if mt7621.rels[0] != "packages-24.10/mipsel_24kc/packages" {
		t.Errorf("mt7621 block must start with its branch feed, got %v", mt7621.rels)
	}

	// arch not covered by any target tree still gets its own block
	leftover := blocks[2]
	if !strings.Contains(leftover.title, "x86_64") ||
		len(leftover.rels) != 1 || leftover.rels[0] != "packages-24.10/x86_64/packages" {
		t.Errorf("bad leftover block: %+v", leftover)
	}
}
