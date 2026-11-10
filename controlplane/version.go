package controlplane

import (
	"strconv"
	"strings"
)

// Stamped via -ldflags by the Makefile here and by the services repo's
// Dockerfile. Unstamped builds report 0.0.0-dev, which any real floor refuses.
var ClientVersion = "0.0.0-dev"

const VersionHeader = "X-Club-Version"

const MaxClientVersionLen = 64

func ClientMeetsFloor(client, floor string) bool {
	if floor == "" {
		return true
	}
	cMaj, cMin, cPat, cOK := parseDescribeVersion(client)
	fMaj, fMin, fPat, fOK := parseDescribeVersion(floor)
	if !cOK || !fOK {
		return false
	}
	switch {
	case cMaj != fMaj:
		return cMaj > fMaj
	case cMin != fMin:
		return cMin > fMin
	default:
		return cPat >= fPat
	}
}

func OldestClientVersion(versions []string) string {
	oldest := ""
	for i, v := range versions {
		if i == 0 || !ClientMeetsFloor(v, oldest) {
			oldest = v
		}
	}
	return oldest
}

func parseDescribeVersion(v string) (maj, min, pat int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimSuffix(v, "-dirty")
	base := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		base = v[:i]
		if !isDescribeDistance(v[i+1:]) {
			return 0, 0, 0, false
		}
	}
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

func isDescribeDistance(s string) bool {
	i := strings.IndexByte(s, '-')
	if i <= 0 {
		return false
	}
	if _, err := strconv.Atoi(s[:i]); err != nil {
		return false
	}
	rest := s[i+1:]
	if len(rest) < 2 || rest[0] != 'g' {
		return false
	}
	for _, r := range rest[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
