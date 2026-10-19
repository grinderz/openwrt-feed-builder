package feedbuilder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteFileIfChanged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "add.sh")

	if err := writeFileIfChanged(path, []byte("one"), execPerm); err != nil {
		t.Fatal(err)
	}

	// an old mtime shows whether the second write touched the file
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}

	if err := writeFileIfChanged(path, []byte("one"), execPerm); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if !info.ModTime().Equal(past) {
		t.Fatalf("same bytes touched the file: mtime %v, want %v", info.ModTime(), past)
	}

	if err := writeFileIfChanged(path, []byte("two"), execPerm); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("content %q, err %v", got, err)
	}
}
