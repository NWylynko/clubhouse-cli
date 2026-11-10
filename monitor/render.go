package monitor

import (
	"fmt"
	"io"
	"strings"
)

func Render(w io.Writer, header string, all []Status) {
	if header != "" {
		fmt.Fprintln(w, header)
	}
	if len(all) == 0 {
		fmt.Fprintln(w, "no sessions found under this CLUB_HOME — start a host or join one")
		return
	}
	fmt.Fprintf(w, "%-8s %-7s %-14s %-9s %-9s %-6s %-4s %-4s\n",
		"PIN", "ROLE", "BRANCH", "MAIN", "HEAD", "AHEAD", "GIT", "DEV")
	for _, s := range all {
		fmt.Fprintf(w, "%-8s %-7s %-14s %-9s %-9s %-6s %-4s %-4s\n",
			s.PIN, dash(s.Role), branchCol(s), dash(s.Main), dash(s.Head),
			aheadCol(s), upCol(s.GitURL, s.GitUp), upCol(s.DevURL, s.DevUp))
	}
	for _, s := range all {
		if len(s.Services) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n  %s services\n", s.PIN)
		for _, svc := range []string{"git", "dev", "conductor"} {
			if where, ok := s.Services[svc]; ok {
				fmt.Fprintf(w, "    %-10s %s\n", svc, where)
			}
		}
	}

	for _, s := range all {
		if len(s.Members) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n  %s members\n", s.PIN)
		fmt.Fprintf(w, "    %-12s %-8s %-12s %s\n", "NAME", "ROLE", "VERSION", "SEEN")
		for _, m := range s.Members {
			fmt.Fprintf(w, "    %-12s %-8s %-12s %s\n",
				dash(m.Name), dash(m.Role), dash(m.Version), seenCol(m.Online))
		}
	}

	for _, s := range all {
		var notes []string
		if len(s.AheadNames) > 0 {
			notes = append(notes, "ahead: "+strings.Join(s.AheadNames, ", "))
		}
		if s.Remote != "" {
			notes = append(notes, s.Remote)
		}
		if s.DevURL != "" {
			notes = append(notes, "dev "+s.DevURL)
		}
		if s.GitURL != "" {
			notes = append(notes, "git "+s.GitURL)
		}
		if s.GitPath != "" {
			notes = append(notes, "via "+s.GitPath)
		}
		if s.Box != "" {
			notes = append(notes, "box "+s.Box)
		}
		if s.Note != "" {
			notes = append(notes, "! "+s.Note)
		}
		if len(notes) > 0 {
			fmt.Fprintf(w, "  %-6s %s\n", s.PIN, strings.Join(notes, "  |  "))
		}
	}
}

func branchCol(s Status) string {
	if s.Branch != "" {
		return trunc(s.Branch, 14)
	}
	if s.Role == "host" {
		return "(canonical)"
	}
	return "-"
}

func aheadCol(s Status) string {
	if s.Role != "host" {
		return "-"
	}
	return fmt.Sprintf("%d", s.Ahead)
}

func upCol(url string, up bool) string {
	if url == "" {
		return "-"
	}
	if up {
		return "ok"
	}
	return "DOWN"
}

func seenCol(online bool) string {
	if online {
		return "online"
	}
	return "quiet"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
