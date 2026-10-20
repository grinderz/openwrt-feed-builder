package feedbuilder

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStaleTopDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, dir := range []string{
		"24.10.8", tRel25125, "25.12.6", "25.12.0-rc1",
		"packages-23.05", "packages-24.10", "packages-25.12", "keys",
	} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, file := range []string{"repo.pub", "24.10.7"} { // a file is never a release dir
		if err := os.WriteFile(filepath.Join(root, file), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	layout := Layout{Versions: []string{"24.10.8", "25.12.6"}}

	got := layout.staleTopDirs(root)
	want := []string{"25.12.0-rc1", tRel25125, "packages-23.05"}

	if !slices.Equal(got, want) {
		t.Fatalf("staleTopDirs = %v, want %v", got, want)
	}

	if got := layout.staleTopDirs(filepath.Join(root, "missing")); got != nil {
		t.Fatalf("missing root: %v, want nil", got)
	}
}

func TestWithSnapshotDirsSkipsPruned(t *testing.T) {
	t.Parallel()

	old := map[string]string{
		"25.12.5/targets/ramips/mt7621/packages|amneziawg-tools": "1",
		"25.12.6/targets/ramips/mt7621/packages|amneziawg-tools": "1",
		"packages-25.12/mipsel_24kc/packages|ssclash":            "1",
	}

	got := withSnapshotDirs([]string{tFeedMipsel2512}, old, []string{tRel25125})
	want := []string{"25.12.6/targets/ramips/mt7621/packages", tFeedMipsel2512}

	if !slices.Equal(got, want) {
		t.Fatalf("withSnapshotDirs = %v, want %v", got, want)
	}
}
