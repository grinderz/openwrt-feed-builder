package feedbuilder

// Parsing helpers for .ipk packages and opkg `Packages` indexes.
//
// An OpenWrt .ipk is normally an `ar` archive containing:
//
//	debian-binary
//	control.tar.gz   (-> ./control with the package metadata)
//	data.tar.gz      (the installed files)
//
// Some older/foreign builds ship the whole .ipk as a single gzipped tar that
// contains control.tar.gz and data.tar.gz as members. Both layouts are handled
// here. Everything in this file is pure stdlib.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/ulikunitz/xz"
)

var (
	arMagic   = []byte("!<arch>\n")
	gzipMagic = []byte{0x1f, 0x8b}
	xzMagic   = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}
)

// readArMembers parses a (simple, BSD/GNU-short-name) ar archive into name -> bytes.
func readArMembers(data []byte) (map[string][]byte, error) {
	if len(data) < 8 || !bytes.Equal(data[:8], arMagic) {
		return nil, fmt.Errorf("not an ar archive")
	}
	members := map[string][]byte{}
	pos := 8
	n := len(data)
	for pos+60 <= n {
		header := data[pos : pos+60]
		pos += 60
		name := strings.TrimSpace(string(header[0:16]))
		sizeField := strings.TrimSpace(string(header[48:58]))
		size, err := strconv.Atoi(sizeField)
		if err != nil || size < 0 { // a negative size would slice data[pos:pos+size] backwards
			break
		}
		if pos+size > n {
			break
		}
		content := data[pos : pos+size]
		pos += size
		if size%2 == 1 { // ar pads each member to an even offset
			pos++
		}
		name = strings.TrimRight(name, "/") // GNU ar appends '/' to short names
		if name != "" {
			members[name] = content
		}
	}
	return members, nil
}

// openTar returns a tar reader over a (gzip/xz-compressed or plain) blob.
func openTar(blob []byte) (*tar.Reader, error) {
	switch {
	case len(blob) >= 2 && bytes.Equal(blob[:2], gzipMagic):
		gz, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			return nil, err
		}
		return tar.NewReader(gz), nil
	case len(blob) >= 6 && bytes.Equal(blob[:6], xzMagic):
		xr, err := xz.NewReader(bytes.NewReader(blob))
		if err != nil {
			return nil, err
		}
		return tar.NewReader(xr), nil
	default:
		return tar.NewReader(bytes.NewReader(blob)), nil
	}
}

// extractMemberFromTar returns the contents of `wanted` from a tar blob.
func extractMemberFromTar(blob []byte, wanted string) ([]byte, error) {
	tr, err := openTar(blob)
	if err != nil {
		return nil, err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		normalized := strings.TrimLeft(hdr.Name, "./")
		if normalized == wanted {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%q not found in tar archive", wanted)
}

// readControl returns the raw text of the `control` file inside an .ipk.
func readControl(ipkPath string) (string, error) {
	data, err := os.ReadFile(ipkPath)
	if err != nil {
		return "", err
	}
	return readControlBytes(data, ipkPath)
}

// readControlBytes is readControl over an already-loaded package (callers that
// also hash the file read it once and share the bytes). name is only for errors.
func readControlBytes(data []byte, name string) (string, error) {
	if len(data) >= 8 && bytes.Equal(data[:8], arMagic) {
		members, err := readArMembers(data)
		if err != nil {
			return "", err
		}
		var controlTar []byte
		for _, candidate := range []string{"control.tar.gz", "control.tar.xz", "control.tar"} {
			if m, ok := members[candidate]; ok {
				controlTar = m
				break
			}
		}
		if controlTar == nil {
			return "", fmt.Errorf("no control.tar.* inside %s", name)
		}
		ctrl, err := extractMemberFromTar(controlTar, "control")
		if err != nil {
			return "", err
		}
		return string(ctrl), nil
	}

	if len(data) >= 2 && bytes.Equal(data[:2], gzipMagic) {
		// Whole-file gzipped tar holding control.tar.gz as a member.
		controlTar, err := extractMemberFromTar(data, "control.tar.gz")
		if err != nil {
			return "", err
		}
		ctrl, err := extractMemberFromTar(controlTar, "control")
		if err != nil {
			return "", err
		}
		return string(ctrl), nil
	}

	return "", fmt.Errorf("unrecognized .ipk format: %s", name)
}

// parseFields parses a Debian-style control block into a field->value map.
// Continuation lines (leading space/tab) are folded into the previous field.
func parseFields(controlText string) map[string]string {
	fields := map[string]string{}
	key := ""
	for _, line := range strings.Split(controlText, "\n") {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if key != "" {
				fields[key] += "\n" + line
			}
			continue
		}
		if i := strings.Index(line, ":"); i >= 0 {
			key = strings.TrimSpace(line[:i])
			fields[key] = strings.TrimSpace(line[i+1:])
		}
	}
	return fields
}

// parseIndex parses an opkg `Packages` index into a list of field maps.
func parseIndex(text string) []map[string]string {
	var stanzas []map[string]string
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, block := range strings.Split(text, "\n\n") {
		block = strings.Trim(block, "\n")
		if block == "" {
			continue
		}
		stanzas = append(stanzas, parseFields(block))
	}
	return stanzas
}
