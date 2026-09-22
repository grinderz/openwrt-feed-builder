package feedbuilder

import (
	"strings"
	"testing"
)

func TestRouterBlocks(t *testing.T) {
	t.Parallel()

	relPaths := []string{
		tTreeFilogicKmods,
		tTreeFilogicPkgs,
		"24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-def",
		tTreeMT7621Pkgs,
		tFeedA53,
		tFeedMipsel,
		tFeedX86, // no target tree ships this arch
	}
	feedArches := map[string]map[string]bool{
		tTreeFilogicKmods: {tArchA53: true},
		tTreeFilogicPkgs:  {tArchA53: true},
		"24.10.7/targets/ramips/mt7621/kmods/6.6.141-1-def": {tArchMipsel: true},
		tTreeMT7621Pkgs: {tArchMipsel: true},
		tFeedA53:        {tArchA53: true},
		tFeedMipsel:     {tArchMipsel: true},
		tFeedX86:        {tArchX86: true},
	}

	blocks := routerBlocks(relPaths, feedArches, tBranch24)
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d: %+v", len(blocks), blocks)
	}

	filogic := blocks[0]
	if !strings.Contains(filogic.title, tTargetFilogic) ||
		!strings.Contains(filogic.title, tArchA53) {
		t.Errorf("bad filogic title: %q", filogic.title)
	}

	want := []string{
		tFeedA53, // branch feed comes first
		tTreeFilogicPkgs,
		tTreeFilogicKmods,
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
	if mt7621.rels[0] != tFeedMipsel {
		t.Errorf("mt7621 block must start with its branch feed, got %v", mt7621.rels)
	}

	// arch not covered by any target tree still gets its own block
	leftover := blocks[2]
	if !strings.Contains(leftover.title, tArchX86) ||
		len(leftover.rels) != 1 || leftover.rels[0] != tFeedX86 {
		t.Errorf("bad leftover block: %+v", leftover)
	}
}

func TestRelBranch(t *testing.T) {
	t.Parallel()

	layout := Layout{Versions: []string{tRel24108, tRel25125}}

	cases := map[string]string{
		"packages-25.12/mipsel_24kc/packages":         tBranch25,
		"25.12.5/targets/mediatek/filogic/packages":   tBranch25,
		"24.10.8/targets/ramips/mt7621/kmods/6.6.1-1": tBranch24,
		"packages-23.05/mipsel_24kc/packages":         "",
		"something/else":                              "",
	}
	for rel, want := range cases {
		if got := relBranch(layout, rel); got != want {
			t.Errorf("relBranch(%q) = %q, want %q", rel, got, want)
		}
	}
}
