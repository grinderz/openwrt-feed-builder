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

// buildPackagesIndex builds the text of a `Packages` index for every .ipk in dir.
//
// For each package we keep its original control block verbatim (so multi-line
// Description fields and any custom fields survive) and append the index-only
// fields opkg needs: Filename, Size, MD5Sum and SHA256sum.
func buildPackagesIndex(dir string) (string, error) {
	files, err := ipkFiles(dir)
	if err != nil {
		return "", err
	}
	var stanzas []string
	for _, path := range files {
		control, err := readControl(path)
		if err != nil {
			return "", err
		}
		control = strings.TrimRight(control, "\n")
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		md5sum, err := hashFile(path, md5.New())
		if err != nil {
			return "", err
		}
		sha, err := hashFile(path, sha256.New())
		if err != nil {
			return "", err
		}
		stanza := control + "\n" +
			fmt.Sprintf("Filename: %s\n", filepath.Base(path)) +
			fmt.Sprintf("Size: %d\n", info.Size()) +
			fmt.Sprintf("MD5Sum: %s\n", md5sum) +
			fmt.Sprintf("SHA256sum: %s\n", sha)
		stanzas = append(stanzas, stanza)
	}
	content := strings.Join(stanzas, "\n") // blank line between stanzas
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content, nil
}

// writeIndex writes Packages and Packages.gz into dir. Returns package count.
func writeIndex(dir string) (int, error) {
	content, err := buildPackagesIndex(dir)
	if err != nil {
		return 0, err
	}
	files, err := ipkFiles(dir)
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
	return len(files), nil
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
