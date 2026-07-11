package feedbuilder

// Debian/opkg-style version comparison.
//
// opkg uses the Debian version comparison algorithm. This implements it closely
// enough to order real-world OpenWrt package versions correctly, including:
//
//   - numeric segments compared as numbers ("2.10" > "2.3")
//   - a leading epoch ("1:..") that dominates the rest
//   - the "~" character sorting before everything, even end-of-string
//     ("1.0~beta" < "1.0")
//   - a "-revision" suffix compared after the upstream version

import (
	"regexp"
	"strconv"
	"strings"
)

// ipkRE matches package_version_arch.ipk.
// Package name and version never contain '_'; the architecture may
// (e.g. x86_64, aarch64_cortex-a53, arm_cortex-a7_neon-vfpv4).
var ipkRE = regexp.MustCompile(`^(?P<name>[^_]+)_(?P<version>[^_]+)_(?P<arch>.+)\.ipk$`)

// ipkMatch parses an .ipk filename, returning (name, version, arch, ok).
func ipkMatch(name string) (string, string, string, bool) {
	m := ipkRE.FindStringSubmatch(name)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isAlpha(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// order returns the sort weight for one character in the non-digit comparison.
// An empty character is represented by -1 here (end of string).
func order(c int) int {
	switch {
	case c < 0:
		return 0
	case c == '~':
		return -1
	case isAlpha(byte(c)):
		return c
	default:
		return c + 256 // non-alphanumerics sort after letters
	}
}

func splitVersion(version string) (epoch int, upstream, revision string) {
	if i := strings.Index(version, ":"); i >= 0 {
		head := version[:i]
		version = version[i+1:]
		if n, err := strconv.Atoi(head); err == nil {
			epoch = n
		}
	}
	if i := strings.LastIndex(version, "-"); i >= 0 {
		upstream = version[:i]
		revision = version[i+1:]
	} else {
		upstream = version
		revision = ""
	}
	return epoch, upstream, revision
}

func cmpPart(a, b string) int {
	i, j := 0, 0
	la, lb := len(a), len(b)
	for i < la || j < lb {
		// 1) compare a run of non-digit characters
		for (i < la && !isDigit(a[i])) || (j < lb && !isDigit(b[j])) {
			ca, cb := -1, -1
			if i < la && !isDigit(a[i]) {
				ca = int(a[i])
			}
			if j < lb && !isDigit(b[j]) {
				cb = int(b[j])
			}
			oa, ob := order(ca), order(cb)
			if oa != ob {
				if oa < ob {
					return -1
				}
				return 1
			}
			if ca >= 0 {
				i++
			}
			if cb >= 0 {
				j++
			}
		}
		// 2) compare a run of digits numerically
		startA := i
		for i < la && isDigit(a[i]) {
			i++
		}
		startB := j
		for j < lb && isDigit(b[j]) {
			j++
		}
		na := atoiZero(a[startA:i])
		nb := atoiZero(b[startB:j])
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return 0
}

func atoiZero(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// compareVersion returns -1, 0, or 1 for version a < b, a == b, a > b.
func compareVersion(a, b string) int {
	ea, ua, ra := splitVersion(a)
	eb, ub, rb := splitVersion(b)
	if ea != eb {
		if ea < eb {
			return -1
		}
		return 1
	}
	if c := cmpPart(ua, ub); c != 0 {
		return c
	}
	return cmpPart(ra, rb)
}
