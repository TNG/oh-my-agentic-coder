package updater

import (
	"strconv"
	"strings"
)

// compareVersions compares two semver version strings, returning -1, 0, or 1
// as a is less than, equal to, or greater than b. A leading "v" and any
// "+build" metadata are ignored, and a pre-release ranks below its associated
// release (1.0.0-rc1 < 1.0.0), per semver precedence.
//
// ok is false when either version's core (MAJOR.MINOR.PATCH) is not three
// integers; callers must treat an unparseable pair conservatively and never
// downgrade on the strength of it.
func compareVersions(a, b string) (cmp int, ok bool) {
	aCore, aPre, aOK := splitVersion(a)
	bCore, bPre, bOK := splitVersion(b)
	if !aOK || !bOK {
		return 0, false
	}
	if c := compareCore(aCore, bCore); c != 0 {
		return c, true
	}
	return comparePrerelease(aPre, bPre), true
}

// splitVersion parses "MAJOR.MINOR.PATCH[-prerelease][+build]" into its three
// numeric core fields and the raw pre-release string, dropping build metadata.
func splitVersion(v string) (core [3]int, pre string, ok bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	coreStr := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		coreStr, pre = v[:i], v[i+1:]
	}
	parts := strings.Split(coreStr, ".")
	if len(parts) != 3 {
		return core, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return core, "", false
		}
		core[i] = n
	}
	return core, pre, true
}

func compareCore(a, b [3]int) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// comparePrerelease applies semver §11: a release (empty pre-release) outranks
// any pre-release; otherwise dot-separated identifiers are compared left to
// right, numeric identifiers below alphanumeric ones, and a longer set of
// identifiers wins when the shared prefix is equal.
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	default:
		return 0
	}
}

func compareIdent(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	aNum, bNum := aErr == nil, bErr == nil
	switch {
	case aNum && bNum:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	case aNum: // numeric identifiers have lower precedence than alphanumeric
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}
