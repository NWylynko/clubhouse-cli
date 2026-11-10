package nextversion

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/internal/wirechanges"
)

const ChangelogPath = "CHANGELOG.md"

type Release struct {
	LastTag string
	Next    string
	Entries []wirechanges.Entry
}

type ConsumeResult struct {
	Removed  []string
	Appended bool
	Version  string
}

var classHeadings = map[string]string{
	wirechanges.ClassBreaking: "Breaking — an already-installed client stops working",
	wirechanges.ClassAdditive: "Additive — an already-installed client is unaffected",
	wirechanges.ClassPatch:    "Patch — nothing an already-installed client can observe changed",
}

var tagPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// Cross-repo contract: the services repo parses CHANGELOG bullets by this
// prefix (internal/wirefloor) and a guard there reads this exact line.
const FloorPrefix = "floor "

func floorMark(floor string) string { return FloorPrefix + floor + " —" }

func LastReleaseTag(ctx context.Context, root string) (string, error) {
	out, err := git.Output(ctx, root, "describe", "--tags", "--abbrev=0")
	if err != nil {
		return "", fmt.Errorf("no release tag is reachable from HEAD in %s: %w\n\n"+
			"The next version is derived by bumping the last one, so there has to be a last one.\n"+
			"Tag the first release by hand (`git tag -a v0.1.0 -m v0.1.0`); every release after it\n"+
			"is derived from the ledger.", root, err)
	}
	return strings.TrimSpace(out), nil
}

func DeriveNextVersion(lastTag string, entries []wirechanges.Entry) (string, error) {
	if lastTag == "" {
		return "", fmt.Errorf("no last tag to bump — the next version is derived from the one before it, written vX.Y.Z")
	}
	m := tagPattern.FindStringSubmatch(lastTag)
	if m == nil {
		return "", fmt.Errorf("the last tag %q is not a release number this tool can bump — releases are written vX.Y.Z (e.g. v0.2.0).\n\n"+
			"If a tag was cut by hand under another shape, retag it or pick the next number yourself:\n"+
			"the derivation is a suggestion, and the one after it reads whatever tag it finds.", lastTag)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])

	switch LargestClass(entries) {
	case wirechanges.ClassBreaking:
		return fmt.Sprintf("v%d.0.0", major+1), nil
	case wirechanges.ClassAdditive:
		return fmt.Sprintf("v%d.%d.0", major, minor+1), nil
	default:
		return fmt.Sprintf("v%d.%d.%d", major, minor, patch+1), nil
	}
}

func LargestClass(entries []wirechanges.Entry) string {
	for _, class := range wirechanges.Classes {
		if len(byClass(entries, class)) > 0 {
			return class
		}
	}
	return wirechanges.ClassPatch
}

func LoadRelease(ctx context.Context, root, lastTag string) (Release, error) {
	entries, err := wirechanges.LoadPending(root)
	if err != nil {
		return Release{}, err
	}
	next, err := DeriveNextVersion(lastTag, entries)
	if err != nil {
		return Release{}, err
	}
	return Release{LastTag: lastTag, Next: next, Entries: entries}, nil
}

func ChangelogBlock(version string, entries []wirechanges.Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", version)
	for _, class := range wirechanges.Classes {
		group := byClass(entries, class)
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n", classHeadings[class])
		for _, e := range group {
			if e.Floor != "" {
				fmt.Fprintf(&b, "- %s %s\n", floorMark(e.Floor), e.Sentence)
				continue
			}
			fmt.Fprintf(&b, "- %s\n", e.Sentence)
		}
	}
	return b.String()
}

func ConsumeLedger(root string, rel Release) (ConsumeResult, error) {
	result := ConsumeResult{Version: rel.Next}
	if len(rel.Entries) == 0 {
		return result, nil
	}

	block := ChangelogBlock(rel.Next, rel.Entries)
	path := filepath.Join(root, ChangelogPath)
	existing, err := os.ReadFile(path)
	if err != nil {
		return result, fmt.Errorf("%s: %w\n\nThe consumed sentences have nowhere to land. Create it with a header and re-run.", ChangelogPath, err)
	}
	if err := os.WriteFile(path, []byte(insertRelease(string(existing), block)), 0o644); err != nil {
		return result, err
	}
	result.Appended = true

	for _, e := range rel.Entries {
		if err := os.Remove(filepath.Join(root, wirechanges.Dir, e.Slug+".md")); err != nil {
			return result, fmt.Errorf("%s is in CHANGELOG.md but could not be deleted: %w\n\n"+
				"Delete it by hand before committing — an entry that survives its own release is folded in twice.", e.File, err)
		}
		result.Removed = append(result.Removed, e.File)
	}
	return result, nil
}

func insertRelease(existing, block string) string {
	head, rest := existing, ""
	switch {
	case strings.HasPrefix(existing, "## "):
		head, rest = "", existing
	default:
		if at := strings.Index(existing, "\n## "); at >= 0 {
			head, rest = existing[:at], existing[at+1:]
		}
	}
	out := block
	if head = strings.TrimRight(head, "\n"); head != "" {
		out = head + "\n\n" + block
	}
	if rest = strings.TrimLeft(rest, "\n"); rest != "" {
		out += "\n" + rest
	}
	return out
}

func byClass(entries []wirechanges.Entry, class string) []wirechanges.Entry {
	var out []wirechanges.Entry
	for _, e := range entries {
		if e.Class == class {
			out = append(out, e)
		}
	}
	return out
}
