package feedbuilder

// OpenWrt 25.12+ packages: apk-tools v3 (.apk files in the ADB format, one
// signed packages.adb index per feed dir).
//
// Package metadata is read natively (a small ADB reader, enough for the
// pkginfo object of a package and the package list of an index), so routing,
// pruning and change reports need no external tool. Everything that writes
// ADB — the index (apk mkndx), its signature (apk adbsign), verification
// (apk verify) and repacked binaries (apk mkpkg) — shells out to apk-tools v3,
// just like opkg feeds shell out to usign.
//
// ADB layout (apk-tools src/adb.h, src/apk_adb.h):
//
//	file    := "ADB." schema:u32 block*          (uncompressed)
//	         | "ADBd" deflate(file[4:])           (raw deflate)
//	block   := type_size:u32 payload pad8         (type = top 2 bits)
//	ADB blk := hdr{compat:u8 ver:u8 rsvd:u16 root:val} values...
//	val     := u32, type in the top 4 bits, offset/value in the low 28;
//	           offsets are relative to the ADB block payload
//	object/array at offset := count:u32 (incl. itself) val[count-1]

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Package formats. A branch's format follows the OpenWrt release: opkg (.ipk,
// Packages index) up to 24.10, apk (.apk, packages.adb index) from 25.12 on.
const (
	formatIPK = "ipk"
	formatAPK = "apk"
)

// Index file names and architecture markers. apk spells the
// arch-independent marker "noarch", opkg (and the rest of the builder) "all".
const (
	opkgIndexName = "Packages"
	apkIndexName  = "packages.adb"
	archAll       = "all"
	archNoarch    = "noarch"
)

// indexFileName is the index each feed dir of the given format carries.
func indexFileName(format string) string {
	if format == formatAPK {
		return apkIndexName
	}

	return opkgIndexName
}

// isIndexFile reports whether a file name is a feed index of either format.
func isIndexFile(name string) bool {
	return name == opkgIndexName || name == apkIndexName
}

// ADB file magics: uncompressed, raw deflate, other compression.
const (
	adbMagic         = "ADB."
	adbDeflateMagic  = "ADBd"
	adbCompressMagic = "ADBc"
)

// ADB framing (apk-tools src/adb.h) and version match bits (apk_version.h).
const (
	adbFileHdrSize     = 8 // magic + schema
	adbBlockHdrSize    = 4 // type_size
	adbExtBlockHdrSize = 16
	adbBlockTypeShift  = 30
	adbBlockSizeMask   = 0x3fffffff
	adbBlockAlign      = 8
	adbHdrSize         = 8 // adb_hdr: compat, version, reserved, root
	adbValSize         = 4
	adbInt64Size       = 8

	apkVerEqual    = 1
	apkVerLess     = 2
	apkVerGreater  = 4
	apkVerFuzzy    = 8
	apkVerConflict = 16
)

// adbMaxBlock caps the ADB (metadata) block size accepted from a file: real
// ones are kilobytes (a package) to a few megabytes (a big index).
const adbMaxBlock = 256 << 20

const (
	adbSchemaIndex   = 0x78646e69 // "indx"
	adbSchemaPackage = 0x676b6370 // "pckg"

	adbTypeMask  = 0xf0000000
	adbValueMask = 0x0fffffff
	adbTypeInt   = 0x10000000
	adbTypeInt32 = 0x20000000
	adbTypeInt64 = 0x30000000
	adbTypeBlob8 = 0x80000000
	adbTypeBlob6 = 0x90000000 // BLOB_16
	adbTypeBlob2 = 0xa0000000 // BLOB_32
	adbTypeArray = 0xd0000000
	adbTypeObj   = 0xe0000000

	adbBlockADB = 0
	adbBlockExt = 3

	// package root object / index root object / pkginfo / dependency fields.
	adbiPkgInfo     = 1
	adbiNdxPackages = 2
	adbiNdxNameSpec = 3
	adbiDepName     = 1
	adbiDepVersion  = 2
	adbiDepMatch    = 3
)

// pkginfo field ids -> the opkg control field names the rest of the builder
// works with (strings; depends/provides & co are dependency arrays).
type adbPkginfoField struct {
	id    int
	field string
	deps  bool
	num   bool
}

func adbPkginfoFields() []adbPkginfoField {
	return []adbPkginfoField{
		{1, "Package", false, false},
		{2, "Version", false, false},
		{4, "Description", false, false},
		{5, "Architecture", false, false},
		{6, "License", false, false},
		{7, "Origin", false, false},
		{8, "Maintainer", false, false},
		{9, "URL", false, false},
		{12, "Installed-Size", false, true},
		{13, "File-Size", false, true},
		{15, "Depends", true, false},
		{16, "Provides", true, false},
		{17, "Replaces", true, false},
		{18, "Install-If", true, false},
		{19, "Recommends", true, false},
	}
}

// isADB reports whether data looks like an apk v3 file (package or index).
func isADB(data []byte) bool {
	return hasMagic(data, adbMagic) || hasMagic(data, adbDeflateMagic) || hasMagic(data, adbCompressMagic)
}

// adbDB is the ADB block of a decoded file: the payload values point into.
type adbDB struct {
	schema uint32
	adb    []byte
}

// decodeADB decompresses an ADB file and returns its schema and ADB block.
// Only the leading ADB block is decoded; signature and data blocks follow it
// and are not needed for metadata. For a compressed package only a prefix is
// inflated — the metadata block always comes first.
func decodeADB(data []byte) (*adbDB, error) {
	var raw []byte

	switch {
	case hasMagic(data, adbMagic):
		raw = data
	case hasMagic(data, adbDeflateMagic):
		inflater := flate.NewReader(bytes.NewReader(data[len(adbDeflateMagic):]))
		defer closeQuietly(inflater)
		// the inflated stream is the whole uncompressed file ("ADB." included):
		// file header + block header first, then exactly the ADB block
		var buf bytes.Buffer
		if _, err := io.CopyN(&buf, inflater, adbFileHdrSize+adbBlockHdrSize); err != nil {
			return nil, fmt.Errorf("%w: inflate: %w", errADB, err)
		}

		size, ext, err := adbBlockSize(buf.Bytes()[adbFileHdrSize:])
		if err != nil {
			return nil, err
		}

		if ext {
			if _, err := io.CopyN(&buf, inflater, adbExtBlockHdrSize-adbBlockHdrSize); err != nil {
				return nil, fmt.Errorf("%w: inflate: %w", errADB, err)
			}

			size, _, err = adbBlockSize(buf.Bytes()[adbFileHdrSize:])
			if err != nil {
				return nil, err
			}
		}

		if size > adbMaxBlock {
			return nil, fmt.Errorf("%w: ADB block of %d bytes is implausibly large", errADB, size)
		}

		if _, err := io.CopyN(&buf, inflater, int64(size)-int64(buf.Len()-adbFileHdrSize)); err != nil {
			return nil, fmt.Errorf("%w: inflate: %w", errADB, err)
		}

		raw = buf.Bytes()
	case hasMagic(data, adbCompressMagic):
		alg := -1
		if len(data) > len(adbCompressMagic) {
			alg = int(data[len(adbCompressMagic)])
		}

		return nil, fmt.Errorf("%w: unsupported compression (ADBc, algorithm %d)", errADB, alg)
	default:
		return nil, fmt.Errorf("%w: not an apk v3 (ADB) file", errADB)
	}

	if len(raw) < adbFileHdrSize+adbBlockHdrSize {
		return nil, fmt.Errorf("%w: truncated header", errADB)
	}

	schema := binary.LittleEndian.Uint32(raw[len(adbMagic):adbFileHdrSize])
	block := raw[adbFileHdrSize:]

	size, ext, err := adbBlockSize(block)
	if err != nil {
		return nil, err
	}

	hdr := adbBlockHdrSize
	if ext {
		hdr = adbExtBlockHdrSize
	}

	typ := adbBlockType(block)

	if typ != adbBlockADB {
		return nil, fmt.Errorf("%w: first block is type %d, want ADB", errADB, typ)
	}

	if uint64(len(block)) < size || size < uint64(hdr)+adbHdrSize {
		return nil, fmt.Errorf("%w: truncated ADB block", errADB)
	}

	return &adbDB{schema: schema, adb: block[hdr:size]}, nil
}

// adbBlockSize returns the raw size (header included, padding excluded) of
// the block starting at b, and whether it uses the extended header.
func adbBlockSize(block []byte) (uint64, bool, error) {
	if len(block) < adbBlockHdrSize {
		return 0, false, fmt.Errorf("%w: truncated block header", errADB)
	}

	ts := binary.LittleEndian.Uint32(block[:adbBlockHdrSize])
	if ts>>adbBlockTypeShift != adbBlockExt {
		return uint64(ts & adbBlockSizeMask), false, nil
	}

	if len(block) < adbExtBlockHdrSize {
		return 0, true, fmt.Errorf("%w: truncated extended block header", errADB)
	}

	return binary.LittleEndian.Uint64(block[adbExtBlockHdrSize-adbInt64Size : adbExtBlockHdrSize]), true, nil
}

// adbBlockType returns the type of the block starting at b (the extended
// header keeps the real type in the low bits).
func adbBlockType(b []byte) uint32 {
	ts := binary.LittleEndian.Uint32(b[:adbBlockHdrSize])
	if ts>>adbBlockTypeShift == adbBlockExt {
		return ts & adbBlockSizeMask
	}

	return ts >> adbBlockTypeShift
}

func (db *adbDB) root() uint32 {
	return binary.LittleEndian.Uint32(db.adb[adbHdrSize-adbValSize : adbHdrSize]) // adb_hdr.root
}

func (db *adbDB) deref(v uint32, off, size int) ([]byte, bool) {
	start := int(v&adbValueMask) + off
	if start < 0 || size < 0 || start+size > len(db.adb) {
		return nil, false
	}

	return db.adb[start : start+size], true
}

// list returns the entries of an object or array value, indexed like apk
// does: entries[i] is field i (entries[0] is the count slot, unused).
func (db *adbDB) list(val uint32) []uint32 {
	t := val & adbTypeMask
	if t != adbTypeObj && t != adbTypeArray {
		return nil
	}

	head, ok := db.deref(val, 0, adbValSize)
	if !ok {
		return nil
	}

	num := int(binary.LittleEndian.Uint32(head))

	body, ok := db.deref(val, 0, adbValSize*num)
	if !ok || num == 0 {
		return nil
	}

	out := make([]uint32, num)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(body[4*i:])
	}

	return out
}

func (db *adbDB) field(entries []uint32, i int) uint32 {
	if i <= 0 || i >= len(entries) {
		return 0
	}

	return entries[i]
}

func (db *adbDB) blob(val uint32) (string, bool) {
	var lenSize int

	switch val & adbTypeMask {
	case adbTypeBlob8:
		lenSize = 1
	case adbTypeBlob6:
		lenSize = 2
	case adbTypeBlob2:
		lenSize = 4
	default:
		return "", false
	}

	head, ok := db.deref(val, 0, lenSize)
	if !ok {
		return "", false
	}

	var length int

	switch val & adbTypeMask {
	case adbTypeBlob8:
		length = int(head[0])
	case adbTypeBlob6:
		length = int(binary.LittleEndian.Uint16(head))
	default:
		length = int(binary.LittleEndian.Uint32(head))
	}

	b, ok := db.deref(val, lenSize, length)
	if !ok {
		return "", false
	}

	return string(b), true
}

func (db *adbDB) int(val uint32) (uint64, bool) {
	switch val & adbTypeMask {
	case adbTypeInt:
		return uint64(val & adbValueMask), true
	case adbTypeInt32:
		b, ok := db.deref(val, 0, adbValSize)
		if !ok {
			return 0, false
		}

		return uint64(binary.LittleEndian.Uint32(b)), true
	case adbTypeInt64:
		b, ok := db.deref(val, 0, adbInt64Size)
		if !ok {
			return 0, false
		}

		return binary.LittleEndian.Uint64(b), true
	}

	return 0, false
}

// apk version match bits (apk_version.h) -> operator, as apk prints them.
func adbDepOp(match uint64) string {
	var operator string

	switch match &^ apkVerConflict {
	case apkVerLess:
		operator = "<"
	case apkVerLess | apkVerEqual:
		operator = "<="
	case apkVerLess | apkVerEqual | apkVerFuzzy:
		operator = "<~"
	case apkVerEqual | apkVerFuzzy, apkVerFuzzy:
		operator = "~"
	case apkVerGreater:
		operator = ">"
	case apkVerGreater | apkVerEqual:
		operator = ">="
	case apkVerGreater | apkVerEqual | apkVerFuzzy:
		operator = ">~"
	case apkVerLess | apkVerGreater:
		operator = "><"
	default:
		operator = "="
	}

	return operator
}

// dependency renders one dependency object the way apk prints it
// ("name", "name=ver", "!name", "name>=ver").
func (db *adbDB) dependency(v uint32) string {
	entries := db.list(v)

	name, ok := db.blob(db.field(entries, adbiDepName))
	if !ok {
		return ""
	}

	match, _ := db.int(db.field(entries, adbiDepMatch))
	if match == 0 {
		match = apkVerEqual
	}

	neg := ""
	if match&apkVerConflict != 0 {
		neg = "!"
	}

	ver, ok := db.blob(db.field(entries, adbiDepVersion))
	if !ok {
		return neg + name
	}

	return neg + name + adbDepOp(match) + ver
}

// pkginfo converts a pkginfo object into opkg-style control fields.
// Architecture "noarch" (apk's arch-independent marker) becomes "all", the
// value the rest of the builder uses.
func (db *adbDB) pkginfo(v uint32) map[string]string {
	e := db.list(v)
	fields := map[string]string{}

	for _, spec := range adbPkginfoFields() {
		fieldVal := db.field(e, spec.id)
		if fieldVal == 0 {
			continue
		}

		switch {
		case spec.deps:
			var deps []string

			items := db.list(fieldVal)
			for i := 1; i < len(items); i++ {
				if s := db.dependency(items[i]); s != "" {
					deps = append(deps, s)
				}
			}

			if len(deps) > 0 {
				fields[spec.field] = strings.Join(deps, ", ")
			}
		case spec.num:
			if n, ok := db.int(fieldVal); ok {
				fields[spec.field] = strconv.FormatUint(n, 10)
			}
		default:
			if s, ok := db.blob(fieldVal); ok {
				fields[spec.field] = s
			}
		}
	}

	if fields["Architecture"] == archNoarch {
		fields["Architecture"] = archAll
	}

	return fields
}

// readAPKFields returns the package metadata of an .apk (v3) as opkg-style
// control fields (Package, Version, Architecture, Depends, ...).
func readAPKFields(data []byte) (map[string]string, error) {
	decoded, err := decodeADB(data)
	if err != nil {
		return nil, err
	}

	if decoded.schema != adbSchemaPackage {
		return nil, fmt.Errorf("%w: schema %08x is not a package", errADB, decoded.schema)
	}

	info := decoded.field(decoded.list(decoded.root()), adbiPkgInfo)

	fields := decoded.pkginfo(info)
	if fields["Package"] == "" {
		return nil, fmt.Errorf("%w: package has no name", errADB)
	}

	return fields, nil
}

// parseADBIndex returns every package of a packages.adb index as opkg-style
// fields, with Filename set from the index's package name spec (default
// "${name}-${version}.apk", as apk fetches them).
func parseADBIndex(data []byte) ([]map[string]string, error) {
	decoded, err := decodeADB(data)
	if err != nil {
		return nil, err
	}

	if decoded.schema != adbSchemaIndex {
		return nil, fmt.Errorf("%w: schema %08x is not an index", errADB, decoded.schema)
	}

	root := decoded.list(decoded.root())

	spec, ok := decoded.blob(decoded.field(root, adbiNdxNameSpec))
	if !ok || spec == "" {
		spec = "${name}-${version}.apk"
	}

	var out []map[string]string

	pkgs := decoded.list(decoded.field(root, adbiNdxPackages))
	for i := 1; i < len(pkgs); i++ {
		fields := decoded.pkginfo(pkgs[i])
		if fields["Package"] == "" {
			continue
		}

		arch := fields["Architecture"]
		if arch == archAll {
			arch = archNoarch
		}

		fields["Filename"] = strings.NewReplacer(
			"${name}", fields["Package"], "${version}", fields["Version"],
			"${arch}", arch).Replace(spec)
		fields["Size"] = fields["File-Size"]
		out = append(out, fields)
	}

	return out, nil
}

// readPkgFields reads the metadata of a package file of either format and
// returns it as opkg-style control fields plus the detected format.
func readPkgFields(path string) (map[string]string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read: %w", err)
	}

	return readPkgFieldsBytes(data, path)
}

func readPkgFieldsBytes(data []byte, name string) (map[string]string, string, error) {
	if isADB(data) {
		fields, err := readAPKFields(data)
		return fields, formatAPK, err
	}

	control, err := readControlBytes(data, name)
	if err != nil {
		return nil, "", err
	}

	return parseFields(control), formatIPK, nil
}

// readIndexFile parses a feed index of either format (Packages or
// packages.adb) into stanzas.
func readIndexFile(path string) ([]map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	if filepath.Base(path) == apkIndexName {
		return parseADBIndex(data)
	}

	return parseIndex(string(data)), nil
}

// apkFileName is the file name apk expects a package under in a repository
// dir (the default pkgname spec "${name}-${version}.apk").
func apkFileName(pkg, version string) string {
	return safeRE.ReplaceAllString(pkg+"-"+version, "_") + ".apk"
}

// pkgFileName is the feed file name for a package of the given format.
func pkgFileName(format, pkg, version, arch string) string {
	if format == formatAPK {
		return apkFileName(pkg, version)
	}

	return safeName(pkg, version, arch)
}

// apkVersionRE is apk's version grammar (apk_version_validate): numbers,
// an optional letter, optional _suffix[N] parts, an optional ~hash and an
// optional -rN revision.
var apkVersionRE = regexp.MustCompile(
	`^[0-9]+(\.[0-9]+)*[a-z]?(_(alpha|beta|pre|rc|cvs|svn|git|hg|p)[0-9]*)*(~[0-9a-f]+)?(-r[0-9]+)?$`)

// ---- apk-tools v3 wrappers ----

var apkToolVersionRE = regexp.MustCompile(`apk-tools (\d+)\.`)

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}

	return def
}

// apkTool is the apk-tools v3 binary (config `apk_tool`, default "apk").
type apkTool string

func (a apkTool) bin() string { return orDefault(string(a), "apk") }

// check verifies the binary exists and is apk-tools 3.x (v2 cannot write or
// read the ADB format).
func (a apkTool) check(ctx context.Context) error {
	out, err := command(ctx, a.bin(), "--version").Output()
	if err != nil {
		return fmt.Errorf("%w v3 (%s) not usable: %w — install apk-tools >= 3 "+
			"(Arch: pacman -S apk-tools; Alpine >= 3.23) or point `apk_tool:` at one "+
			"(e.g. an OpenWrt 25.12 SDK's staging_dir/host/bin/apk)", errAPKTool, a.bin(), err)
	}

	if m := apkToolVersionRE.FindSubmatch(out); m == nil || atoiOr(string(m[1]), 0) < 3 {
		return fmt.Errorf("%s is %q; %w >= 3 is required for OpenWrt 25.12+ feeds",
			a.bin(), strings.TrimSpace(string(out)), errAPKTool)
	}

	return nil
}

func (a apkTool) run(ctx context.Context, dir string, args ...string) error {
	cmd := command(ctx, a.bin(), args...)

	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s %s: %w: %s", a.bin(), args[0], err, msg)
		}

		return fmt.Errorf("%s %s: %w", a.bin(), args[0], err)
	}

	return nil
}

// apkFiles returns the sorted list of *.apk paths in dir.
func apkFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.apk"))
	if err != nil {
		return nil, fmt.Errorf("glob: %w", err)
	}

	sort.Strings(files)

	return files, nil
}

// mkndx (re)writes dir/packages.adb from every .apk in dir, unsigned — the
// signature is added separately (sign), like the opkg Packages.sig. The
// index is written to a temp name first so a failure never leaves a
// truncated index behind. Returns the package count.
func (a apkTool) mkndx(ctx context.Context, dir string) (int, error) {
	files, err := apkFiles(dir)
	if err != nil {
		return 0, err
	}

	if len(files) == 0 {
		return 0, fmt.Errorf("%w: no .apk files in %s", errAPKTool, dir)
	}

	args := []string{"mkndx", "--allow-untrusted", "--output", apkIndexName + ".tmp"}
	for _, f := range files {
		args = append(args, filepath.Base(f))
	}

	if err := a.run(ctx, dir, args...); err != nil {
		removeQuietly(filepath.Join(dir, apkIndexName+".tmp"))
		return 0, err
	}

	if err := os.Rename(filepath.Join(dir, apkIndexName+".tmp"), filepath.Join(dir, apkIndexName)); err != nil {
		return 0, fmt.Errorf("rename: %w", err)
	}

	return len(files), nil
}

// sign adds (or replaces) the signature inside dir/packages.adb.
func (a apkTool) sign(ctx context.Context, dir, secretKey string) error {
	return a.run(ctx, dir, "--allow-untrusted", "adbsign", "--sign-key", secretKey, apkIndexName)
}

// verify checks dir/packages.adb's signature against publicKey (apk only
// trusts keys from a keys dir, so the key is staged in a private one).
func (a apkTool) verify(ctx context.Context, dir, publicKey string) error {
	keys, err := os.MkdirTemp("", "feedbuilder-apk-keys-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer removeQuietly(keys)

	if err := copyFile(publicKey, filepath.Join(keys, "feed.pem")); err != nil {
		return err
	}

	return a.run(ctx, dir, "--keys-dir", keys, "verify", apkIndexName)
}

// apkPkgSpec describes one package apk mkpkg should build.
type apkPkgSpec struct {
	name, version, arch string
	description         string
	depends, provides   []string
	info                map[string]string // extra --info key:value (license, url, ...)
	scripts             map[string]string // apk script type -> body
	installPath         string
	mode                int64
	payload             []byte
}

// mkpkg builds an .apk with apk mkpkg. File ownership is recorded from the
// staging tree, so the tool runs under fakeroot (like the OpenWrt build)
// unless the builder already runs as root; mtimes are pinned to the epoch so
// rebuilding an unchanged package yields byte-identical output.
func (a apkTool) mkpkg(ctx context.Context, dest string, spec apkPkgSpec) error {
	stage, err := os.MkdirTemp("", "feedbuilder-apk-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer removeQuietly(stage)

	root := filepath.Join(stage, "root")

	install := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(spec.installPath, "/")))
	if err := os.MkdirAll(filepath.Dir(install), dirPerm); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	if err := writeFile(install, spec.payload, filePerm); err != nil {
		return err
	}

	if err := os.Chmod(install, os.FileMode(spec.mode)&os.ModePerm); err != nil { //nolint:gosec // mode <= modeMax
		return fmt.Errorf("chmod: %w", err)
	}
	// OpenWrt packages carry their file list at /lib/apk/packages/<name>.list
	// (default_prerm and friends read it)
	listDir := filepath.Join(root, "lib", "apk", "packages")
	if err := os.MkdirAll(listDir, dirPerm); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	list := "/" + strings.TrimPrefix(spec.installPath, "/") + "\n"
	if err := writeFile(filepath.Join(listDir, spec.name+".list"), []byte(list), filePerm); err != nil {
		return err
	}

	if err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		return os.Chtimes(p, ipkEpoch(), ipkEpoch()) //nolint:gosec // our own private staging dir
	}); err != nil {
		return fmt.Errorf("walk: %w", err)
	}

	arch := spec.arch
	if arch == archAll {
		arch = archNoarch
	}

	const infoFlag = "--info"

	args := []string{
		"mkpkg",
		infoFlag, "name:" + spec.name,
		infoFlag, "version:" + spec.version,
		infoFlag, "arch:" + arch,
		infoFlag, "description:" + spec.description,
	}
	if len(spec.depends) > 0 {
		args = append(args, infoFlag, "depends:"+strings.Join(spec.depends, " "))
	}

	if len(spec.provides) > 0 {
		args = append(args, infoFlag, "provides:"+strings.Join(spec.provides, " "))
	}

	for _, k := range sortedStringKeys(spec.info) {
		args = append(args, infoFlag, k+":"+spec.info[k])
	}

	for _, typ := range sortedStringKeys(spec.scripts) {
		p := filepath.Join(stage, typ)
		if err := writeFile(p, []byte(spec.scripts[typ]), execPerm); err != nil {
			return err
		}

		args = append(args, "--script", typ+":"+p)
	}

	tmp := dest + ".tmp"
	args = append(args, "--files", root, "--output", tmp)

	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = command(ctx, a.bin(), args...)
	} else {
		fakeroot, err := exec.LookPath("fakeroot")
		if err != nil {
			return fmt.Errorf("%w: building an .apk needs fakeroot (files must be owned "+
				"by root in the package); install it or run as root", errAPKTool)
		}

		cmd = command(ctx, fakeroot, append([]string{a.bin()}, args...)...)
	}

	cmd.Env = append(os.Environ(), "SOURCE_DATE_EPOCH=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		removeQuietly(tmp)
		return fmt.Errorf("apk mkpkg: %w: %s", err, strings.TrimSpace(string(out)))
	}

	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// genAPKKey writes a new ECDSA P-256 keypair in the PEM forms apk-tools and
// the OpenWrt build use (private-key.pem / public-key.pem).
func genAPKKey(secret, public string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode private key: %w", err)
	}

	pubDer, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("encode public key: %w", err)
	}

	for _, p := range []string{secret, public} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%w: %s already exists; not overwriting", errKey, p)
		}
	}

	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := writeFile(secret, privPEM, secretPerm); err != nil {
		return err
	}

	return writeFile(public, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDer}), filePerm)
}

// adbSigned reports whether an ADB file (e.g. packages.adb) carries a
// signature block. It does not check the signature — that is apk verify's job.
func adbSigned(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}

	raw := data
	switch {
	case hasMagic(data, adbDeflateMagic):
		r := flate.NewReader(bytes.NewReader(data[len(adbDeflateMagic):]))
		defer closeQuietly(r)

		if raw, err = io.ReadAll(r); err != nil {
			return false, fmt.Errorf("adb: inflate: %w", err)
		}
	case !hasMagic(data, adbMagic):
		return false, fmt.Errorf("%w: not an uncompressed or deflate ADB file", errADB)
	}

	const adbBlockSig = 1

	for off := adbFileHdrSize; off+adbBlockHdrSize <= len(raw); {
		size, _, err := adbBlockSize(raw[off:])
		if err != nil {
			return false, err
		}

		typ := adbBlockType(raw[off:])

		if typ == adbBlockSig {
			return true, nil
		}

		if size < 4 || size > uint64(len(raw)-off) { //nolint:gosec // off < len(raw): loop condition
			break
		}

		off += int((size + adbBlockAlign - 1) &^ (adbBlockAlign - 1)) //nolint:gosec // size bounded by len(raw)
	}

	return false, nil
}
