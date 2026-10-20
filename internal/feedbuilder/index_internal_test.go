package feedbuilder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readJSONIndex parses dir's index.json.
func readJSONIndex(t *testing.T, dir string) jsonIndex {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, jsonIndexName))
	if err != nil {
		t.Fatal(err)
	}

	var index jsonIndex
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("index.json: %v\n%s", err, data)
	}

	return index
}

func TestPathArch(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"out/packages-24.10/mipsel_24kc/packages":      "mipsel_24kc",
		"out/packages-25.12/aarch64_cortex-a53/custom": "aarch64_cortex-a53",
		"out/24.10.8/targets/ramips/mt7621/packages":   archAll,
		"out/24.10.8/targets/ramips/mt7621/kmods/6.6":  archAll,
	}
	for dir, want := range cases {
		if got := pathArch(filepath.FromSlash(dir)); got != want {
			t.Errorf("pathArch(%s) = %q, want %q", dir, got, want)
		}
	}
}
