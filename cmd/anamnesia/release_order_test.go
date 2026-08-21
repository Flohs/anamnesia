package main

import "testing"

// TestRcTenIsNewerThanRcNine. The prerelease part was compared as a
// plain string, so "rc10" < "rc9" because '1' < '9'. Tagging rc10 built
// and published fine, and then every install on rc9 reported "up to
// date" forever: the release existed and was simply unreachable.
//
// Semver says an alphanumeric identifier is compared in ASCII order,
// which is where the string compare came from. But this comparator only
// ever orders this project's own tags, and in this project rc10 follows
// rc9. Digits inside an identifier are compared as numbers here.
func TestRcTenIsNewerThanRcNine(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0-rc10", "v0.1.0-rc9", 1},
		{"v0.1.0-rc9", "v0.1.0-rc10", -1},
		{"v0.1.0-rc10", "v0.1.0-rc2", 1},
		{"v0.1.0-rc11", "v0.1.0-rc10", 1},
		{"v0.1.0-rc100", "v0.1.0-rc99", 1},
		// The hop that gets an install off rc9: greater under the old
		// string compare AND under this one, so a build that still has
		// the bug can still find it.
		{"v0.1.0-rc9.1", "v0.1.0-rc9", 1},
		{"v0.1.0-rc11", "v0.1.0-rc9.1", 1},
		// Unchanged behaviour.
		{"v0.1.0-rc2", "v0.1.0-rc1", 1},
		{"v0.1.0-rc1", "v0.1.0-rc1", 0},
		{"v0.1.0", "v0.1.0-rc9", 1},
		{"v0.1.0-rc9", "v0.1.0", -1},
		{"v0.2.0-rc1", "v0.1.0-rc9", 1},
		{"v1.0.0", "v0.9.9", 1},
		{"v0.1.0-alpha", "v0.1.0-rc1", -1},
	}
	for _, c := range cases {
		av, ok := parseVersion(c.a)
		if !ok {
			t.Fatalf("parseVersion(%q) failed", c.a)
		}
		bv, ok := parseVersion(c.b)
		if !ok {
			t.Fatalf("parseVersion(%q) failed", c.b)
		}
		if got := compareVersions(av, bv); got != c.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestOrderingIsAntisymmetric: newestOfAllReleases keeps a running best,
// so an ordering that says both a>b and b>a would make the winner depend
// on the order GitHub happened to list them in.
func TestOrderingIsAntisymmetric(t *testing.T) {
	tags := []string{
		"v0.1.0-rc1", "v0.1.0-rc2", "v0.1.0-rc9", "v0.1.0-rc9.1",
		"v0.1.0-rc10", "v0.1.0-rc11", "v0.1.0", "v0.2.0-rc1", "v1.0.0",
	}
	for _, a := range tags {
		for _, b := range tags {
			av, _ := parseVersion(a)
			bv, _ := parseVersion(b)
			if got, rev := compareVersions(av, bv), compareVersions(bv, av); got != -rev {
				t.Errorf("compare(%s,%s)=%d but compare(%s,%s)=%d", a, b, got, b, a, rev)
			}
		}
	}
}
