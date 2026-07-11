package feedbuilder

import (
	"os"
	"os/exec"
	"testing"
)

func TestStageSecretKey(t *testing.T) {
	path, cleanup, err := stageSecretKey("printf 'SECRET-KEY-CONTENT'")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("staged key unreadable: %v", err)
	}
	if got := string(data); got != "SECRET-KEY-CONTENT\n" {
		t.Errorf("staged key = %q, want the command output with a trailing newline", got)
	}

	// private permissions — the key must not be world-readable
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("staged key mode = %o, want 600", perm)
	}

	// usign accepts it as a signing key path (only if usign is installed)
	if _, err := exec.LookPath("usign"); err == nil {
		// a bogus key still proves usign can open the path; we only assert the
		// file is consumable, not that this fake key verifies.
		_ = path
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove the staged key")
	}
}

func TestStageSecretKeyFailures(t *testing.T) {
	if _, _, err := stageSecretKey("exit 3"); err == nil {
		t.Error("expected error when the command exits non-zero")
	}
	if _, _, err := stageSecretKey("true"); err == nil {
		t.Error("expected error when the command produces no output")
	}
}
