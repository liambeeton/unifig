package compat

import (
	"strconv"
	"strings"
)

// release is a Controller version as this package compares them: dotted
// decimals, nothing else. A version that is not that shape is not compared at
// all rather than guessed at — the answer would decide whether an operator is
// told they are ahead of the matrix or behind it, and a wrong answer there is
// worse than no answer.
type release []int

func parseRelease(version string) (release, bool) {
	fields := strings.Split(version, ".")
	parsed := make(release, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return nil, false
		}
		parsed = append(parsed, n)
	}
	if len(parsed) == 0 {
		return nil, false
	}
	return parsed, true
}

// compare orders two releases, reading a component the shorter one does not
// have as zero — so 10.0 is the floor 10.0.162 sits on rather than a version
// above it.
func (r release) compare(other release) int {
	for i := 0; i < len(r) || i < len(other); i++ {
		mine, theirs := r.at(i), other.at(i)
		if mine != theirs {
			if mine < theirs {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (r release) at(i int) int {
	if i < len(r) {
		return r[i]
	}
	return 0
}
