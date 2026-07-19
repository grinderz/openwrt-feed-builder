package feedbuilder

// Generate opkg `Packages` / `Packages.gz` indexes and sign them with usign.

import (
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func hashFile(path string, h hash.Hash) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ipkFiles returns the sorted list of *.ipk paths in dir.
func ipkFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.ipk"))
	if err != nil {
		return nil, err
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
var strippedIndexFields = []string{"Source:", "SourceName:", "SourceDateEpoch:", "Maintainer:"}

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
		for _, f := range strippedIndexFields {
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
			return "", 0, err
		}
		control, err := readControlBytes(data, path)
		if err != nil {
			return "", 0, err
		}
		fields := fmt.Sprintf("Filename: %s\n", filepath.Base(path)) +
			fmt.Sprintf("Size: %d\n", len(data)) +
			fmt.Sprintf("MD5Sum: %x\n", md5.Sum(data)) +
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

// writeIndex writes Packages and Packages.gz into dir. Returns package count.
func writeIndex(dir string) (int, error) {
	content, count, err := buildPackagesIndex(dir)
	if err != nil {
		return 0, err
	}

	packagesPath := filepath.Join(dir, "Packages")
	if err := os.WriteFile(packagesPath, []byte(content), 0o644); err != nil {
		return 0, err
	}

	gzPath := filepath.Join(dir, "Packages.gz")
	gzFile, err := os.Create(gzPath)
	if err != nil {
		return 0, err
	}
	gw := gzip.NewWriter(gzFile)
	if _, err := gw.Write([]byte(content)); err != nil {
		gw.Close()
		gzFile.Close()
		return 0, err
	}
	if err := gw.Close(); err != nil {
		gzFile.Close()
		return 0, err
	}
	if err := gzFile.Close(); err != nil {
		return 0, err
	}
	return count, nil
}

func usignAvailable() bool {
	_, err := exec.LookPath("usign")
	return err == nil
}

// signIndex signs the Packages file in dir, producing Packages.sig (usign).
func signIndex(dir, secretKey string) error {
	packagesPath := filepath.Join(dir, "Packages")
	sigPath := filepath.Join(dir, "Packages.sig")
	cmd := exec.Command("usign", "-S", "-m", packagesPath, "-s", secretKey, "-x", sigPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// verifyIndex checks dir's Packages against its Packages.sig with the given
// public key (usign -V).
func verifyIndex(dir, publicKey string) error {
	cmd := exec.Command("usign", "-V",
		"-m", filepath.Join(dir, "Packages"),
		"-x", filepath.Join(dir, "Packages.sig"),
		"-p", publicKey)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
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
func pubkeyFingerprint(pubkeyText string) (string, error) {
	for _, line := range strings.Split(pubkeyText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return "", err
		}
		if len(raw) < 10 {
			return "", fmt.Errorf("public key too short")
		}
		return hex.EncodeToString(raw[2:10]), nil
	}
	return "", fmt.Errorf("no key material found in public key")
}
