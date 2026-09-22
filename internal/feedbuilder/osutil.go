package feedbuilder

// Small OS helpers shared by the builder: external commands, world-readable
// files, best-effort cleanup.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Permissions of what the builder writes: the feed tree is served by a web
// server (world-readable), secrets are private.
const (
	dirPerm    = 0o755
	filePerm   = 0o644
	execPerm   = 0o755
	secretPerm = 0o600
)

// command runs one of the external tools the builder drives (usign, apk,
// make, upx, docker via the buildroot, sh for key commands). Their names and
// arguments come from the builder or the user's own config by design.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// writeFile writes data with perm. The feed tree (packages, indexes, add.sh,
// public keys) is served by a web server and must stay world-readable, and
// package payloads carry their own modes — so perm is the caller's choice.
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil { //nolint:gosec // G306: see above
		return fmt.Errorf("write: %w", err)
	}

	return nil
}

// readAll is io.ReadAll with the error wrapped.
func readAll(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	return data, nil
}

// closeQuietly closes c, dropping the error: for readers and files whose
// close cannot lose data (read-only, or on an error path that already failed).
func closeQuietly(c io.Closer) { _ = c.Close() }

// removeQuietly removes a temp file or dir best-effort: cleanup that fails
// leaves debris, not a wrong result.
func removeQuietly(path string) {
	_ = os.RemoveAll(path) //nolint:gosec // G703: only paths the builder itself created
}
