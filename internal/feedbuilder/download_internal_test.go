package feedbuilder

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCachedMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pkg.ipk")

	data := []byte("package-bytes")
	if err := os.WriteFile(path, data, 0o600); err != nil {
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
	for _, tcase := range cases {
		got, err := cachedMismatch(path, tcase.rf)
		if err != nil {
			t.Fatalf("%s: %v", tcase.name, err)
		}

		if (got != "") != tcase.mismatch {
			t.Errorf("%s: mismatch=%q, want mismatch=%v", tcase.name, got, tcase.mismatch)
		}
	}
}

func TestOnlyMatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		only      []string
		typ, name string
		want      bool
	}{
		{[]string{tTypeSDK}, tTypeSDK, "amneziawg-src", true},
		{[]string{"sdk*"}, tTypeGithub, "sdk-tools", true},    // name match counts too
		{[]string{tTypeSDK}, tTypeGithub, "sdk-tools", false}, // no glob -> exact only
		{[]string{"amnezia*"}, tTypeSDK, "amneziawg-src", true},
		{[]string{tTypeGithub, "html"}, "feed", "upstream", false},
		{[]string{"ssclash"}, tTypeGithub, "ssclash", true},
	}
	for _, c := range cases {
		if got := onlyMatch(c.only, c.typ, c.name); got != c.want {
			t.Errorf("onlyMatch(%v, %q, %q) = %v, want %v", c.only, c.typ, c.name, got, c.want)
		}
	}
}

func TestCacheGC(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cache, err := newCache(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	write := func(rel string) string {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}
	usedDL := cache.pathFor("https://example.com/new.ipk")
	write(filepath.Base(usedDL))
	staleDL := write(filepath.Base(cache.pathFor("https://example.com/old.ipk")))
	usedBuilt := write("built/mihomo_1.2-1_all.ipk")
	staleBuilt := write("built/mihomo_1.1-1_all.apk")
	staleUpx := write("upx/" + strings.Repeat("a", 64))
	foreign := write("notes.txt") // not a cache entry: never touched
	usedSDK := filepath.Join(dir, tTypeSDK, "awg", "official", tRel25125, "mediatek-filogic")

	write("sdk/awg/official/25.12.5/mediatek-filogic/kmod.apk")
	write("sdk/awg/official/24.10.6/ramips-mt7621/kmod.ipk")

	cache.mark(usedDL)
	cache.mark(usedBuilt)
	cache.mark(usedSDK)

	removed, freed, err := cache.gc()
	if err != nil {
		t.Fatal(err)
	}

	if removed != 4 || freed != 4 {
		t.Errorf("gc removed %d entries / %d bytes, want 4 / 4", removed, freed)
	}

	for _, p := range []string{usedDL, usedBuilt, foreign, filepath.Join(usedSDK, "kmod.apk")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should survive: %v", p, err)
		}
	}

	for _, p := range []string{staleDL, staleBuilt, staleUpx, filepath.Join(dir, tTypeSDK, "awg", "official", tRel24106)} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s should be removed", p)
		}
	}
}
