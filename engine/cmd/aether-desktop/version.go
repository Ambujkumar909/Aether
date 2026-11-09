package main

import (
	"strconv"
	"strings"
)

// version is set at build time (-X main.version=1.2.3). "dev" is a build
// nobody released: it never updates itself.
var version = "dev"

// compareVersions orders dotted versions numerically ("1.10.0" after
// "1.9.3"); a segment that is not a number compares as text after any number
// ("1.0.0-rc1" before "1.0.0"? no: shorter wins only when every shared
// segment is equal, so "1.0.0" < "1.0.0.1"). Returns -1, 0 or 1.
func compareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x == y {
			continue
		}
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn < yn {
				return -1
			}
			return 1
		case xerr == nil:
			return 1 // a number outranks a word ("1.0" after "1.0-rc")
		case yerr == nil:
			return -1
		default:
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
