package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Problem is one thing wrong with a config file, located as precisely as the
// stage that found it can manage.
//
// The whole point of validate is that an operator can fix the file without
// guessing, so a Problem says where (Line, Path) and what to do about it
// (Message), never just that something is wrong.
type Problem struct {
	// Line is the 1-based line in the file, or 0 when the problem belongs to
	// the document as a whole rather than to one place in it.
	Line int
	// Path is the operator-facing location in unifig.yaml's own terms, e.g.
	// `networks[1].subnet`. Empty at the document level.
	Path string
	// Message reads as advice, not as a status code.
	Message string
}

// Error is the failure of a whole config file, carrying every problem found
// rather than only the first: an operator fixing a file wants the list, not a
// dozen re-runs.
type Error struct {
	// File is the config file's path — as distinct from Problem.Path, which
	// locates a field inside it.
	File     string
	Problems []Problem
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	b.WriteString(" is not valid:")
	for _, p := range sortedProblems(e.Problems) {
		b.WriteString("\n  ")
		if p.Line > 0 {
			b.WriteString("line ")
			b.WriteString(strconv.Itoa(p.Line))
			b.WriteString(": ")
		}
		if p.Path != "" {
			b.WriteString(p.Path)
			b.WriteString(": ")
		}
		b.WriteString(p.Message)
	}
	return b.String()
}

// sortedProblems puts problems in file order so the same broken file always
// produces byte-identical output — schema validation reports its causes in
// map iteration order, which is not stable across runs.
func sortedProblems(problems []Problem) []Problem {
	sorted := make([]Problem, len(problems))
	copy(sorted, problems)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Line != sorted[j].Line {
			return sorted[i].Line < sorted[j].Line
		}
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		return sorted[i].Message < sorted[j].Message
	})
	return sorted
}

func quoteAll(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}
