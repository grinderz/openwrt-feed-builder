package feedbuilder

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestCachedMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pkg.ipk")
	data := []byte("package-bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	cases := []struct {
		name     string
		rf       remoteFile
		mismatch bool
	}{
		{"no metadata", remoteFile{url: "u"}, false},
		{"size match", remoteFile{url: "u", size: int64(len(data))}, false},
		{"size mismatch", remoteFile{url: "u", size: 999}, true},
		{"sha match", remoteFile{url: "u", sha256: hexSum}, false},
		{"sha match uppercase", remoteFile{url: "u", sha256: "ABC"}, true},
		{"sha mismatch", remoteFile{url: "u", sha256: "deadbeef"}, true},
		{"both match", remoteFile{url: "u", size: int64(len(data)), sha256: hexSum}, false},
	}
	for _, c := range cases {
		got, err := cachedMismatch(path, c.rf)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (got != "") != c.mismatch {
			t.Errorf("%s: mismatch=%q, want mismatch=%v", c.name, got, c.mismatch)
		}
	}
}

func TestOnlyMatch(t *testing.T) {
	cases := []struct {
		only      []string
		typ, name string
		want      bool
	}{
		{[]string{"sdk"}, "sdk", "amneziawg-src", true},
		{[]string{"sdk*"}, "github", "sdk-tools", true}, // name match counts too
		{[]string{"sdk"}, "github", "sdk-tools", false}, // no glob -> exact only
		{[]string{"amnezia*"}, "sdk", "amneziawg-src", true},
		{[]string{"github", "html"}, "feed", "upstream", false},
		{[]string{"ssclash"}, "github", "ssclash", true},
	}
	for _, c := range cases {
		if got := onlyMatch(c.only, c.typ, c.name); got != c.want {
			t.Errorf("onlyMatch(%v, %q, %q) = %v, want %v", c.only, c.typ, c.name, got, c.want)
		}
	}
}
