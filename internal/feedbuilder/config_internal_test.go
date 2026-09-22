package feedbuilder

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// yaml.v3 decodes only true/false as bool; YAML 1.1 spellings (no/yes/on/off)
// arrive as strings and must still be honored — `enabled: no` silently staying
// enabled was a real footgun.
func TestBoolOrYAML11(t *testing.T) {
	t.Parallel()

	var src Source
	if err := yaml.Unmarshal([]byte("enabled: no\nrecursive: yes\nplain: false\n"), &src); err != nil {
		t.Fatal(err)
	}

	if src.boolOr("enabled", true) {
		t.Error("enabled: no should disable")
	}

	if !src.boolOr("recursive", false) {
		t.Error("recursive: yes should enable")
	}

	if src.boolOr("plain", true) {
		t.Error("plain: false should disable")
	}

	if !src.boolOr("missing", true) {
		t.Error("missing key should fall back to default")
	}

	if src.boolOr("garbage", false) {
		t.Error("non-bool value should fall back to default")
	}
}
