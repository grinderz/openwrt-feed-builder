package feedbuilder

// Generate opkg `Packages` / `Packages.gz` indexes and sign them with usign;
// apk feed dirs (25.12+) get packages.adb via apk-tools instead (apk.go).

import (
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec // opkg index MD5Sum field
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func hashFile(path string, hasher hash.Hash) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer closeQuietly(file)

	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("copy: %w", err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ipkFiles returns the sorted list of *.ipk paths in dir.
func ipkFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.ipk"))
	if err != nil {
		return nil, fmt.Errorf("glob: %w", err)
	}

	sort.Strings(files)

	return files, nil
}

// strippedIndexFields are control fields dropped from the Packages index,
// matching the official buildbot indexes. opkg's index parser matches field
// names by prefix, so SourceName / SourceDateEpoch all parse as a repeated
// Source field; re-setting a field with a longer value overflows the slot in
// opkg's per-package blob buffer ("ERROR: truncating field ...") and corrupts
// the fields that follow it — Description then reads someone else's string.
func strippedIndexFields() []string {
	return []string{"Source:", "SourceName:", "SourceDateEpoch:", "Maintainer:"}
}

// stripIndexFields removes control fields that must not reach the index,
// including any continuation lines that belong to them.
func stripIndexFields(control string) string {
	var out []string

	skipping := false

	for line := range strings.SplitSeq(strings.TrimRight(control, "\n"), "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if !skipping {
				out = append(out, line)
			}

			continue
		}

		skipping = false

		for _, f := range strippedIndexFields() {
			if strings.HasPrefix(line, f) {
				skipping = true
				break
			}
		}

		if !skipping {
			out = append(out, line)
		}
	}

	return strings.Join(out, "\n") + "\n"
}

// insertIndexFields places the index-only fields before the Description
// field, matching the official ipkg-make-index.sh. opkg's index parser
// drops fields that follow a multi-line Description, so appending them at
// the end makes packages fail with "does not have a valid filename field".
func insertIndexFields(control, fields string) string {
	lines := strings.Split(strings.TrimRight(control, "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "Description:") {
			return strings.Join(lines[:i], "\n") + "\n" +
				strings.TrimRight(fields, "\n") + "\n" +
				strings.Join(lines[i:], "\n") + "\n"
		}
	}

	return strings.Join(lines, "\n") + "\n" + fields
}

// md5Hex is the MD5Sum field opkg indexes carry; SHA256sum carries integrity.
func md5Hex(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec // opkg format field, not a security check

	return hex.EncodeToString(sum[:])
}

// buildPackagesIndex builds the text of a `Packages` index for every .ipk in dir.
//
// For each package we keep its control block (minus strippedIndexFields, so
// multi-line Description fields and any custom fields survive) and insert the
// index-only fields opkg needs (Filename, Size, MD5Sum, SHA256sum) before
// Description.
func buildPackagesIndex(dir string) (string, int, error) {
	files, err := ipkFiles(dir)
	if err != nil {
		return "", 0, err
	}

	var stanzas []string

	for _, path := range files {
		// one read serves the control extraction, both checksums and the size
		data, err := os.ReadFile(path)
		if err != nil {
			return "", 0, fmt.Errorf("read: %w", err)
		}

		control, err := readControlBytes(data, path)
		if err != nil {
			return "", 0, err
		}

		fields := fmt.Sprintf("Filename: %s\n", filepath.Base(path)) +
			fmt.Sprintf("Size: %d\n", len(data)) +
			"MD5Sum: " + md5Hex(data) + "\n" +
			fmt.Sprintf("SHA256sum: %x\n", sha256.Sum256(data))
		stanzas = append(stanzas, insertIndexFields(stripIndexFields(control), fields))
	}
	// Every stanza — including the last — must be terminated by a blank line,
	// like the official indexes: opkg only commits the pending Description of
	// a stanza when it sees the blank line, so a file ending right after the
	// last Description leaks that description onto the first package of the
	// next feed parsed.
	content := strings.Join(stanzas, "\n")
	if content != "" {
		content += "\n"
	}

	return content, len(files), nil
}

// scriptShims prepares a directory with stand-ins for the OpenWrt buildroot
// host tools ipkg-make-index.sh depends on: an `mkhash` replacement (openssl
// based) and, on systems with a BSD stat, a GNU-style `stat -c%s` wrapper.
// Returns the shim dir; the caller adds it to PATH.
func scriptShims(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "feedbuilder-shims-")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	mkhash := `#!/bin/sh
# minimal mkhash stand-in for ipkg-make-index.sh: mkhash <sha256|md5> <file>
algo="$1"; file="$2"
case "$algo" in
sha256|md5) openssl dgst "-$algo" -r "$file" | cut -d' ' -f1 ;;
*) echo "mkhash shim: unsupported algo '$algo'" >&2; exit 1 ;;
esac
`
	if err := writeFile(filepath.Join(dir, "mkhash"), []byte(mkhash), execPerm); err != nil {
		removeQuietly(dir)
		return "", err
	}
	// the script hardcodes GNU `stat -L -c%s`; BSD stat (macOS) spells that
	// `stat -L -f%z`
	if command(ctx, "stat", "-L", "-c%s", os.DevNull).Run() != nil {
		stat := `#!/bin/sh
if [ "$1" = "-L" ] && [ "$2" = "-c%s" ]; then
	exec /usr/bin/stat -L -f%z "$3"
fi
exec /usr/bin/stat "$@"
`
		if err := writeFile(filepath.Join(dir, "stat"), []byte(stat), execPerm); err != nil {
			removeQuietly(dir)
			return "", err
		}
	}

	return dir, nil
}

// scriptPackagesIndex builds the `Packages` text for dir by running the
// official OpenWrt ipkg-make-index.sh — the reference to debug/compare the
// native indexer against. Note the script's known deviations from our native
// index: control fields pass through unstripped, MD5Sum is absent, and
// packages named kernel/libc are skipped.
func scriptPackagesIndex(ctx context.Context, dir, script string) (string, int, error) {
	absScript, err := filepath.Abs(script)
	if err != nil {
		return "", 0, fmt.Errorf("resolve path: %w", err)
	}

	shims, err := scriptShims(ctx)
	if err != nil {
		return "", 0, err
	}
	defer removeQuietly(shims)

	cmd := command(ctx, "bash", absScript, ".")
	cmd.Dir = dir

	cmd.Env = append(os.Environ(),
		"PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MKHASH="+filepath.Join(shims, "mkhash"))

	var out strings.Builder

	cmd.Stdout = &out

	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("%s %s: %w", filepath.Base(script), dir, err)
	}

	content := out.String()

	return content, strings.Count("\n"+content, "\nPackage:"), nil
}

// leafFormat reports the package format of a feed dir: apk when it holds a
// packages.adb index or .apk files, opkg otherwise. A dir never mixes the two
// (every branch has one format and dirs are per branch).
func leafFormat(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, apkIndexName)); err == nil {
		return formatAPK
	}

	if files, _ := apkFiles(dir); len(files) > 0 {
		return formatAPK
	}

	return formatIPK
}

// writeIndex writes the feed index into dir and returns the package count:
// Packages and Packages.gz for an opkg dir (with a non-empty indexScript the
// text comes from ipkg-make-index.sh instead of the native generator), an
// unsigned packages.adb from apk mkndx for an apk dir.
func writeIndex(ctx context.Context, dir, indexScript string, apk apkTool) (int, error) {
	if leafFormat(dir) == formatAPK {
		return apk.mkndx(ctx, dir)
	}

	var (
		content string
		count   int
		err     error
	)
	if indexScript != "" {
		content, count, err = scriptPackagesIndex(ctx, dir, indexScript)
	} else {
		content, count, err = buildPackagesIndex(dir)
	}

	if err != nil {
		return 0, err
	}

	packagesPath := filepath.Join(dir, opkgIndexName)
	if err := writeFile(packagesPath, []byte(content), filePerm); err != nil {
		return 0, err
	}

	gzPath := filepath.Join(dir, "Packages.gz")

	gzFile, err := os.Create(gzPath)
	if err != nil {
		return 0, fmt.Errorf("create: %w", err)
	}

	gzw := gzip.NewWriter(gzFile)
	if _, err := gzw.Write([]byte(content)); err != nil {
		closeQuietly(gzw)
		closeQuietly(gzFile)

		return 0, fmt.Errorf("gzip: %w", err)
	}

	if err := gzw.Close(); err != nil {
		closeQuietly(gzFile)
		return 0, fmt.Errorf("gzip: %w", err)
	}

	if err := gzFile.Close(); err != nil {
		return 0, fmt.Errorf("close: %w", err)
	}

	return count, nil
}

func usignAvailable() bool {
	_, err := exec.LookPath("usign")
	return err == nil
}

// signIndex signs the Packages file in dir, producing Packages.sig (usign).
func signIndex(ctx context.Context, dir, secretKey string) error {
	packagesPath := filepath.Join(dir, opkgIndexName)
	sigPath := filepath.Join(dir, "Packages.sig")
	cmd := command(ctx, "usign", "-S", "-m", packagesPath, "-s", secretKey, "-x", sigPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("usign -S: %w", err)
	}

	return nil
}

// verifyIndex checks dir's Packages against its Packages.sig with the given
// public key (usign -V).
func verifyIndex(ctx context.Context, dir, publicKey string) error {
	cmd := command(ctx, "usign", "-V",
		"-m", filepath.Join(dir, opkgIndexName),
		"-x", filepath.Join(dir, "Packages.sig"),
		"-p", publicKey)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %w: %s", errUsign, err, msg)
		}

		return fmt.Errorf("run: %w", err)
	}

	return nil
}

// pubkeyFingerprint computes the 16-hex-char key id OpenWrt uses as the
// /etc/opkg/keys filename.
//
// A usign public key is:
//
//	untrusted comment: ...
//	<base64>            # 2-byte alg + 8-byte keyid + 32-byte key
//
// The keyid is the filename opkg expects under /etc/opkg/keys/.
// A decoded usign key is a 2-byte algorithm, the 8-byte key id, the key.
const (
	usignKeyIDStart = 2
	usignKeyIDEnd   = 10
)

func pubkeyFingerprint(pubkeyText string) (string, error) {
	for line := range strings.SplitSeq(pubkeyText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}

		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return "", fmt.Errorf("decode base64: %w", err)
		}

		if len(raw) < usignKeyIDEnd {
			return "", fmt.Errorf("%w: usign public key too short", errKey)
		}

		return hex.EncodeToString(raw[usignKeyIDStart:usignKeyIDEnd]), nil
	}

	return "", fmt.Errorf("%w: no key material found in usign public key", errKey)
}

// signKeys is what signing needs, prepared once per command: the staged
// secret keys per format and the usign key id baked into add.sh.
type signKeys struct {
	usignSecret string // opkg: usign secret key path
	apkSecret   string // apk: EC secret key path (PEM)
	fingerprint string // usign key id ("" when sign.public_key is unset)
	cleanups    []func()
}

// keyID is the usign key id baked into add.sh ("" for an unsigned build).
func (k *signKeys) keyID() string {
	if k == nil {
		return ""
	}

	return k.fingerprint
}

func (k *signKeys) close() {
	for _, c := range k.cleanups {
		c()
	}
}

// signLeaf signs dir's index with the key of its format.
func (k *signKeys) signLeaf(ctx context.Context, cfg *Config, dir string) error {
	if leafFormat(dir) == formatAPK {
		return cfg.APKTool.sign(ctx, dir, k.apkSecret)
	}

	return signIndex(ctx, dir, k.usignSecret)
}

// prepareSignKeys checks the signing tools and stages the secret keys for the
// given package formats (a key command runs once, its output lands in a
// private temp file removed by close). The public keys of every format the
// layout carries are validated too, since add.sh / repo.pub / repo-apk.pem
// are written for all of them.
//
// An error wrapping errSignUnavailable is what a build can survive by writing
// an unsigned feed (tool missing, key command failed); any other error is a
// broken public key — a signed feed routers cannot verify must not be produced.
func prepareSignKeys(ctx context.Context, cfg *Config, formats []string) (*signKeys, error) {
	keys := &signKeys{}
	layoutFormats := toSet(cfg.Layout.formats())

	if layoutFormats[formatIPK] {
		if cfg.Sign.PublicKey == "" {
			warnf("%s", "sign.public_key not set; repo.pub and "+
				"the key install step in the opkg add.sh will be missing")
		} else {
			data, err := os.ReadFile(cfg.Sign.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("cannot read sign.public_key: %w", err)
			}

			if keys.fingerprint, err = pubkeyFingerprint(string(data)); err != nil {
				return nil, fmt.Errorf("sign.public_key: %w", err)
			}
		}
	}

	if layoutFormats[formatAPK] {
		if cfg.Sign.APKPublicKey == "" {
			warnf("%s", "sign.apk_public_key not set; "+apkPubName+
				" and the key install step in the apk add.sh will be missing")
		} else {
			data, err := os.ReadFile(cfg.Sign.APKPublicKey)
			if err != nil {
				return nil, fmt.Errorf("cannot read sign.apk_public_key: %w", err)
			}

			if b, _ := pem.Decode(data); b == nil || b.Type != "PUBLIC KEY" {
				return nil, fmt.Errorf("%w: sign.apk_public_key %s is not a PEM "+
					"public key", errKey, cfg.Sign.APKPublicKey)
			}
		}
	}

	stage := func(path, command string) (string, error) {
		if command == "" {
			return path, nil
		}

		staged, cleanup, err := stageSecretKey(ctx, command)
		if err != nil {
			return "", err
		}

		keys.cleanups = append(keys.cleanups, cleanup)

		return staged, nil
	}

	for _, format := range formats {
		var err error

		switch format {
		case formatIPK:
			if !usignAvailable() {
				keys.close()
				return nil, fmt.Errorf("%w: usign not found on PATH", errSignUnavailable)
			}

			keys.usignSecret, err = stage(cfg.Sign.SecretKey, cfg.Sign.SecretKeyCmd)
		case formatAPK:
			if err := cfg.APKTool.check(ctx); err != nil {
				keys.close()
				return nil, fmt.Errorf("%w: %w", errSignUnavailable, err)
			}

			keys.apkSecret, err = stage(cfg.Sign.APKSecretKey, cfg.Sign.APKSecretKeyCmd)
			if err != nil {
				err = fmt.Errorf("apk key: %w", err)
			}
		}

		if err != nil {
			keys.close()
			return nil, fmt.Errorf("%w: %w", errSignUnavailable, err)
		}
	}

	return keys, nil
}
