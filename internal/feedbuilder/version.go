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
// nonAlphaWeight lifts non-letters above every letter in the ordering.
const nonAlphaWeight = 256

func order(char int) int {
	switch {
	case char < 0:
		return 0
	case char == '~':
		return -1
	case char < nonAlphaWeight && isAlpha(byte(char)):
		return char
	default:
		return char + nonAlphaWeight // non-alphanumerics sort after letters
	}
}

func splitVersion(version string) (int, string, string) {
	epoch := 0

	if i := strings.Index(version, ":"); i >= 0 {
		head := version[:i]
		version = version[i+1:]

		if n, err := strconv.Atoi(head); err == nil {
			epoch = n
		}
	}

	if i := strings.LastIndex(version, "-"); i >= 0 {
		return epoch, version[:i], version[i+1:]
	}

	return epoch, version, ""
}

func cmpPart(left, right string) int {
	posA, posB := 0, 0

	lenA, lenB := len(left), len(right)
	for posA < lenA || posB < lenB {
		// 1) compare a run of non-digit characters
		for (posA < lenA && !isDigit(left[posA])) || (posB < lenB && !isDigit(right[posB])) {
			charA, charB := -1, -1
			if posA < lenA && !isDigit(left[posA]) {
				charA = int(left[posA])
			}

			if posB < lenB && !isDigit(right[posB]) {
				charB = int(right[posB])
			}

			oa, ob := order(charA), order(charB)
			if oa != ob {
				if oa < ob {
					return -1
				}

				return 1
			}

			if charA >= 0 {
				posA++
			}

			if charB >= 0 {
				posB++
			}
		}
		// 2) compare a run of digits numerically
		startA := posA
		for posA < lenA && isDigit(left[posA]) {
			posA++
		}

		startB := posB
		for posB < lenB && isDigit(right[posB]) {
			posB++
		}

		na := atoiZero(left[startA:posA])

		nb := atoiZero(right[startB:posB])
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
	epochA, upA, revA := splitVersion(a)

	epochB, upB, revB := splitVersion(b)
	if epochA != epochB {
		if epochA < epochB {
			return -1
		}

		return 1
	}

	if c := cmpPart(upA, upB); c != 0 {
		return c
	}

	return cmpPart(revA, revB)
}
