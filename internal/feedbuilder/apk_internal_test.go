package feedbuilder

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testdata fixtures were built with apk-tools 3.0.8 (apk mkpkg / mkndx /
// adbsign): a kmod with an exact kernel dependency, an arch-independent
// package with a suffixed version, their index, and the same index signed.

func TestReadAPKFields(t *testing.T) {
	t.Parallel()

	fields, format, err := readPkgFields("testdata/kmod-test-6.12.94.1.0-r1.apk")
	if err != nil {
		t.Fatal(err)
	}

	if format != formatAPK {
		t.Errorf("format = %q, want apk", format)
	}

	want := map[string]string{
		tFieldPackage:  "kmod-test",
		"Version":      "6.12.94.1.0-r1",
		"Architecture": tArchA53,
		"Description":  "test kmod",
		tFieldDepends: "!conflicting, kernel=6.12.94~5a6c1f71be683ae9980b15d3ce73e24d-r1, " +
			"kmod-udptunnel4, libfoo>=1.0",
		"Provides": "kmod-test-any",
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("%s = %q, want %q", k, fields[k], v)
		}
	}

	if got := kernelVermagic(fields); got != "6.12.94~5a6c1f71be683ae9980b15d3ce73e24d-r1" {
		t.Errorf("kernelVermagic = %q", got)
	}

	// noarch is the builder's tArchAll
	fields, _, err = readPkgFields("testdata/demo-all-2.0_rc1~abc123-r3.apk")
	if err != nil {
		t.Fatal(err)
	}

	if fields["Architecture"] != tArchAll || fields["License"] != tLicenseMIT ||
		fields["Version"] != "2.0_rc1~abc123-r3" {
		t.Errorf("demo-all fields = %v", fields)
	}
}

func TestParseADBIndex(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"testdata/packages.adb", "testdata/packages-signed.adb"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}

		stanzas, err := parseADBIndex(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if len(stanzas) != 2 {
			t.Fatalf("%s: %d packages, want 2", name, len(stanzas))
		}

		got := map[string]map[string]string{}
		for _, st := range stanzas {
			got[st[tFieldPackage]] = st
		}

		k := got["kmod-test"]
		if k["Filename"] != "kmod-test-6.12.94.1.0-r1.apk" || k["Size"] != "437" {
			t.Errorf("%s: kmod-test Filename/Size = %q/%q", name, k["Filename"], k["Size"])
		}

		if d := got["demo-all"]; d["Filename"] != "demo-all-2.0_rc1~abc123-r3.apk" ||
			d["Architecture"] != tArchAll {
			t.Errorf("%s: demo-all = %v", name, d)
		}
	}

	if _, err := parseADBIndex([]byte("Package: foo\n")); err == nil {
		t.Error("an opkg index must not parse as ADB")
	}

	data, _ := os.ReadFile("testdata/kmod-test-6.12.94.1.0-r1.apk")
	if _, err := parseADBIndex(data); err == nil {
		t.Error("a package must not parse as an index")
	}
}

func TestADBSigned(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{"testdata/packages.adb": false, "testdata/packages-signed.adb": true}
	for name, want := range cases {
		got, err := adbSigned(name)
		if err != nil || got != want {
			t.Errorf("adbSigned(%s) = %v, %v; want %v", name, got, err, want)
		}
	}
}

func TestReadIndexFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	data, _ := os.ReadFile("testdata/packages.adb")
	if err := os.WriteFile(filepath.Join(dir, "packages.adb"), data, 0o600); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}

	opkg := []byte("Package: a\nVersion: 1\n\nPackage: b\nVersion: 2\n\n")
	if err := os.WriteFile(filepath.Join(dir, "Packages"), opkg, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{"packages.adb": 2, "Packages": 2} {
		st, err := readIndexFile(filepath.Join(dir, name))
		if err != nil || len(st) != want {
			t.Errorf("readIndexFile(%s) = %d stanzas, %v; want %d", name, len(st), err, want)
		}
	}
}

func TestPkgFileName(t *testing.T) {
	t.Parallel()

	got := pkgFileName(formatAPK, "luci-app-ttyd", "26.263.44884~0834d09", tArchAll)
	if got != "luci-app-ttyd-26.263.44884~0834d09.apk" {
		t.Errorf("apk name = %q", got)
	}

	if got := pkgFileName(formatIPK, "foo", "1.0-1", tArchAll); got != "foo_1.0-1_all.ipk" {
		t.Errorf("ipk name = %q", got)
	}
}

func TestAPKVersionRE(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"1.19.28-r1", "2.0_rc1~abc123-r3", "1.2a-r0", "6.12.94.3.1.20260906-r1", "5"} {
		if !apkVersionRE.MatchString(v) {
			t.Errorf("%q should be a valid apk version", v)
		}
	}

	for _, v := range []string{"v1.2.3-r1", "1.2.3-1", "1.2.3-beta-r1", "latest-r1"} {
		if apkVersionRE.MatchString(v) {
			t.Errorf("%q should not be a valid apk version", v)
		}
	}
}

func TestAPKDep(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		tDepLibc:       tDepLibc,
		"foo (>= 1.2)": "foo>=1.2",
		"foo (<< 2)":   "foo<2",
		"foo (>>2.1)":  "foo>2.1",
		"foo (= 1.0)":  "foo=1.0",
	}
	for in, want := range cases {
		if got := apkDep(in); got != want {
			t.Errorf("apkDep(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenAPKKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	secret, public := filepath.Join(dir, "priv.pem"), filepath.Join(dir, "pub.pem")
	if err := genAPKKey(secret, public); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(secret)

	block, _ := pem.Decode(data)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		t.Fatalf("secret key is not an EC PEM key")
	}

	if _, err := x509.ParseECPrivateKey(block.Bytes); err != nil {
		t.Fatal(err)
	}

	if st, _ := os.Stat(secret); st.Mode().Perm() != 0o600 {
		t.Errorf("secret key mode %v, want 0600", st.Mode().Perm())
	}

	data, _ = os.ReadFile(public)
	if block, _ = pem.Decode(data); block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("public key is not a PEM public key")
	}

	if err := genAPKKey(secret, public); err == nil {
		t.Error("genAPKKey must not overwrite existing keys")
	}
}

// requireAPK skips unless apk-tools v3 (and fakeroot, for mkpkg) are on PATH.
func requireAPK(t *testing.T, fakeroot bool) apkTool {
	t.Helper()

	tool := apkTool("apk")
	if err := tool.check(t.Context()); err != nil {
		t.Skipf("apk-tools v3 not available: %v", err)
	}

	if _, err := exec.LookPath("fakeroot"); fakeroot && err != nil && os.Geteuid() != 0 {
		t.Skip("fakeroot not available")
	}

	return tool
}

// TestAPKIndexSignVerify runs the whole apk index cycle through apk-tools:
// mkndx, adbsign with a key from genAPKKey, verify with the right key and
// rejection with a foreign one.
func TestAPKIndexSignVerify(t *testing.T) {
	t.Parallel()

	tool := requireAPK(t, false)

	dir := t.TempDir()
	for _, f := range []string{"kmod-test-6.12.94.1.0-r1.apk", "demo-all-2.0_rc1~abc123-r3.apk"} {
		if err := copyFile(filepath.Join("testdata", f), filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}

	n, err := writeIndex(t.Context(), dir, "", tool)
	if err != nil || n != 2 {
		t.Fatalf("writeIndex = %d, %v", n, err)
	}

	if leafFormat(dir) != formatAPK {
		t.Error("leafFormat should detect the apk dir")
	}

	if signed, _ := adbSigned(filepath.Join(dir, "packages.adb")); signed {
		t.Error("fresh index must be unsigned")
	}

	keys := t.TempDir()
	secret, public := filepath.Join(keys, "priv.pem"), filepath.Join(keys, "pub.pem")
	other := filepath.Join(keys, "other.pem")

	if err := genAPKKey(secret, public); err != nil {
		t.Fatal(err)
	}

	if err := genAPKKey(filepath.Join(keys, "other-priv.pem"), other); err != nil {
		t.Fatal(err)
	}

	if err := tool.sign(t.Context(), dir, secret); err != nil {
		t.Fatal(err)
	}

	if signed, _ := adbSigned(filepath.Join(dir, "packages.adb")); !signed {
		t.Error("index should be signed after sign")
	}

	if err := tool.verify(t.Context(), dir, public); err != nil {
		t.Errorf("verify with the signing key: %v", err)
	}

	if err := tool.verify(t.Context(), dir, other); err == nil {
		t.Error("verify with a foreign key must fail")
	}
}

// TestBinaryAPK builds an .apk with apk mkpkg and reads it back: metadata,
// root ownership and byte-identical rebuilds (the incremental build relies on
// it).
func TestBinaryAPK(t *testing.T) {
	t.Parallel()

	tool := requireAPK(t, true)
	dir := t.TempDir()
	spec := apkPkgSpec{
		name: "demo-bin", version: "1.2.3-r1", arch: tArchAll,
		description: "demo", depends: []string{tDepLibc, apkDep("foo (>= 1.0)")},
		info:        map[string]string{"license": tLicenseMIT},
		scripts:     map[string]string{"post-install": "#!/bin/sh\nexit 0\n"},
		installPath: "/opt/demo/bin/demo", mode: 0o755, payload: []byte("#!/bin/sh\necho hi\n"),
	}

	first, second := filepath.Join(dir, "a.apk"), filepath.Join(dir, "b.apk")
	if err := tool.mkpkg(t.Context(), first, spec); err != nil {
		t.Fatal(err)
	}

	if err := tool.mkpkg(t.Context(), second, spec); err != nil {
		t.Fatal(err)
	}

	da, _ := os.ReadFile(first)

	db, _ := os.ReadFile(second)
	if string(da) != string(db) {
		t.Error("rebuilding an unchanged package must be byte-identical")
	}

	fields, _, err := readPkgFields(first)
	if err != nil {
		t.Fatal(err)
	}

	if fields[tFieldPackage] != "demo-bin" || fields["Version"] != "1.2.3-r1" ||
		fields["Architecture"] != tArchAll || fields[tFieldDepends] != "foo>=1.0, libc" ||
		fields["License"] != tLicenseMIT {
		t.Errorf("fields = %v", fields)
	}

	out, err := command(t.Context(), string(tool), "adbdump", first).Output()
	if err != nil {
		t.Fatal(err)
	}

	dump := string(out)
	for _, want := range []string{"user: root", "name: demo", "mode: 0755", "post-install:", "name: demo-bin.list"} {
		if !strings.Contains(dump, want) {
			t.Errorf("adbdump lacks %q:\n%s", want, dump)
		}
	}
}
