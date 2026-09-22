package feedbuilder

import "testing"

func TestCompareVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int // sign of compareVersion(a, b)
	}{
		// numeric segments compare as numbers, not strings
		{"2.10", "2.3", 1},
		{"1.9", "1.10", -1},
		{"1.09", "1.9", 0}, // leading zeros are numerically equal
		{tVersion123, tVersion123, 0},

		// tilde sorts before everything, including end of string
		{"1.0~beta", tVersion10, -1},
		{"1.0~rc1", "1.0~rc2", -1},
		{"1.0~~", "1.0~", -1},

		// epoch dominates the rest
		{"1:0.1", "2.0", 1},
		{"1:1.0", "1:2.0", -1},
		{"0.9", "1:0.1", -1}, // missing epoch = 0

		// revision compared after the upstream version
		{"1.0-1", "1.0-2", -1},
		{tVersion10, "1.0-1", -1}, // absent revision sorts before -1
		{"1.0-r1", "1.0-r2", -1},

		// letters, then non-alphanumerics after letters (Debian ordering)
		{"1.0a", "1.0b", -1},
		{tVersion10, "1.0+git", -1},

		// real-world OpenWrt shapes
		{"6.6.141~f31f6f85a36836e510d64a18a9a5f1bf-r1", "6.6.141-r1", -1},
		{"2025.07.23~49056d17-r1", "2025.07.24~deadbeef-r1", -1},
		{"1.0.20260223-r1", "1.0.20260329-r1", -1},
	}

	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}

		return 0
	}
	for _, tcase := range cases {
		if got := sign(compareVersion(tcase.a, tcase.b)); got != tcase.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", tcase.a, tcase.b, got, tcase.want)
		}
		// antisymmetry
		if got := sign(compareVersion(tcase.b, tcase.a)); got != -tcase.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", tcase.b, tcase.a, got, -tcase.want)
		}
	}
}
