package feedbuilder

import (
	"bytes"
	"fmt"
	"testing"
)

// arEntry renders one ar archive member (60-byte header + padded content).
func arEntry(name string, content []byte) []byte {
	header := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(content))

	b := append([]byte(header), content...)
	if len(content)%2 == 1 {
		b = append(b, '\n')
	}

	return b
}

func arArchive(entries ...[]byte) []byte {
	out := []byte("!<arch>\n")
	for _, e := range entries {
		out = append(out, e...)
	}

	return out
}

func TestReadArMembers(t *testing.T) {
	t.Parallel()

	data := arArchive(
		arEntry("debian-binary", []byte("2.0\n")),
		arEntry("control.tar.gz/", []byte("odd")), // GNU '/' suffix + odd size padding
	)

	members, err := readArMembers(data)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(members["debian-binary"]); got != "2.0\n" {
		t.Errorf("debian-binary = %q", got)
	}

	if got := string(members["control.tar.gz"]); got != "odd" {
		t.Errorf("control.tar.gz = %q (GNU name suffix not stripped?)", got)
	}
}

// A malformed member with a negative size must not panic (it used to slice
// data[pos:pos+size] backwards) — parsing just stops at the bad header.
func TestReadArMembersNegativeSize(t *testing.T) {
	t.Parallel()

	good := arEntry("debian-binary", []byte("2.0\n"))
	bad := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10s`\n", "evil", "0", "0", "0", "100644", "-4")
	data := arArchive(good, []byte(bad+"XXXX"))

	members, err := readArMembers(data)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := members["evil"]; ok {
		t.Error("member with negative size must be rejected")
	}

	if _, ok := members["debian-binary"]; !ok {
		t.Error("members before the malformed one should survive")
	}
}

func TestReadArMembersTruncated(t *testing.T) {
	t.Parallel()

	entry := arEntry("debian-binary", []byte("2.0\n"))
	for cut := range entry { // every truncation point: no panic
		data := arArchive(entry[:cut])
		if _, err := readArMembers(data); err != nil {
			t.Fatalf("cut=%d: unexpected error %v", cut, err)
		}
	}

	if _, err := readArMembers([]byte("nope")); err == nil {
		t.Error("non-ar data must be rejected")
	}
}

func TestExtractMemberFromTarMissing(t *testing.T) {
	t.Parallel()

	blob, err := tarGz([]tarEntry{{name: "./other", mode: 0o644, data: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := extractMemberFromTar(blob, "control"); err == nil {
		t.Error("missing member must be an error")
	}

	got, err := extractMemberFromTar(blob, "other")
	if err != nil || !bytes.Equal(got, []byte("x")) {
		t.Errorf("member other = %q, err %v", got, err)
	}
}
