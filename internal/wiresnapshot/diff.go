package wiresnapshot

import (
	"fmt"
	"strings"
)

const maxDiffLines = 60

const diffContext = 3

func WireSurfaceDiff(committed, current string) string {
	old := strings.Split(strings.TrimRight(committed, "\n"), "\n")
	new := strings.Split(strings.TrimRight(current, "\n"), "\n")
	common := longestCommonLines(old, new)

	var script []string
	changed := map[int]bool{}
	mark := func(prefix, line string) {
		if prefix != "  " {
			changed[len(script)] = true
		}
		script = append(script, prefix+line)
	}
	i, j := 0, 0
	for _, line := range common {
		for i < len(old) && old[i] != line {
			mark("- ", old[i])
			i++
		}
		for j < len(new) && new[j] != line {
			mark("+ ", new[j])
			j++
		}
		mark("  ", line)
		i++
		j++
	}
	for ; i < len(old); i++ {
		mark("- ", old[i])
	}
	for ; j < len(new); j++ {
		mark("+ ", new[j])
	}

	keep := map[int]bool{}
	for idx := range changed {
		for k := idx - diffContext; k <= idx+diffContext; k++ {
			if k >= 0 && k < len(script) {
				keep[k] = true
			}
		}
	}
	var out []string
	gap := true
	anchor := ""
	for idx, line := range script {
		content := line[2:]
		isAnchor := strings.HasPrefix(content, "type ") || strings.HasPrefix(content, "## ")
		if isAnchor {
			anchor = content
		}
		if !keep[idx] {
			gap = true
			continue
		}
		if gap && !isAnchor && anchor != "" {
			out = append(out, "  … in "+anchor)
		} else if gap && len(out) > 0 {
			out = append(out, "  …")
		}
		gap = false
		out = append(out, line)
	}

	if len(out) > maxDiffLines {
		hidden := len(out) - maxDiffLines
		out = append(out[:maxDiffLines], fmt.Sprintf(
			"  … and %d more changed lines. For the whole picture: run `%s`, then `git diff %s` — but classify what you MEANT to change, not what the diff shows.",
			hidden, RegenerateCommand, SnapshotPath))
	}
	return strings.Join(out, "\n")
}

func longestCommonLines(a, b []string) []string {
	table := make([][]int, len(a)+1)
	for i := range table {
		table[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
				continue
			}
			table[i][j] = max(table[i+1][j], table[i][j+1])
		}
	}
	var out []string
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}
