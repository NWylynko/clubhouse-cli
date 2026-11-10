package wirechanges

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const Dir = ".wire-changes"

const FormatDoc = ".wire-changes/README.md"

const FloorConstantRef = "ClientVersionFloor, in the private services repo (internal/controlplane/service/floor.go)"

const Playbook = `the expand/contract playbook — "Expand/contract: shipping a wire change" in .wire-changes/README.md`

const (
	ClassBreaking = "breaking"
	ClassAdditive = "additive"
	ClassPatch    = "patch"
)

var Classes = []string{ClassBreaking, ClassAdditive, ClassPatch}

type Entry struct {
	File     string
	Slug     string
	Class    string
	Floor    string
	Sentence string
}

var (
	slugPattern  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	floorPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

func LoadPending(root string) ([]Entry, error) {
	dir := filepath.Join(root, Dir)
	items, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", Dir, err)
	}
	var entries []Entry
	for _, item := range items {
		name := item.Name()
		if item.IsDir() || name == "README.md" || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		entry, err := ParseEntry(name, string(data))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	return entries, nil
}

func ParseEntry(name, body string) (Entry, error) {
	path := Dir + "/" + name
	slug := strings.TrimSuffix(name, ".md")
	if !strings.HasSuffix(name, ".md") {
		return Entry{}, fmt.Errorf("%s: a wire-change entry must be a .md file — rename it %s.md. See %s", path, slug, FormatDoc)
	}
	if !slugPattern.MatchString(slug) {
		return Entry{}, fmt.Errorf("%s: %q is not a slug — use lowercase-words-with-dashes, e.g. member-version-field. See %s", path, slug, FormatDoc)
	}

	entry := Entry{File: path, Slug: slug}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	i := 0
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Entry{}, fmt.Errorf("%s: %q is not a `key: value` header — the entry starts with `class: breaking | additive | patch`, then a blank line, then the sentence. See %s", path, line, FormatDoc)
		}
		key = strings.TrimSpace(strings.ToLower(key))
		value = strings.TrimSpace(value)
		switch key {
		case "class":
			entry.Class = value
		case "floor":
			entry.Floor = value
		default:
			return Entry{}, fmt.Errorf("%s: unknown header %q — an entry carries `class:` and, when breaking, `floor:`. Everything else belongs in the sentence. See %s", path, key, FormatDoc)
		}
	}

	var prose []string
	for ; i < len(lines); i++ {
		if line := strings.TrimSpace(lines[i]); line != "" {
			prose = append(prose, line)
		}
	}
	entry.Sentence = strings.Join(prose, " ")

	if err := entry.Validate(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func (e Entry) Validate() error {
	switch e.Class {
	case "":
		return fmt.Errorf("%s: no `class:` line — add one naming exactly one of %s, from the point of view of a client installed BEFORE this change. See %s",
			e.File, strings.Join(Classes, " | "), FormatDoc)
	case ClassBreaking, ClassAdditive, ClassPatch:
	default:
		return fmt.Errorf("%s: class %q is not one of %s — `breaking` means an already-installed client stops working, `additive` means it is unaffected, `patch` means nothing it can observe changed. See %s",
			e.File, e.Class, strings.Join(Classes, " | "), FormatDoc)
	}

	if e.Sentence == "" {
		return fmt.Errorf("%s: no sentence — after the header lines and a blank line, write ONE sentence from an old client's point of view. It is copied verbatim into CHANGELOG.md at tag time, so write it for a reader. See %s",
			e.File, FormatDoc)
	}

	switch {
	case e.Class == ClassBreaking && e.Floor == "":
		return fmt.Errorf("%s: class: breaking with no `floor:` — add `floor: vX.Y.Z` naming the OLDEST client that still works after this change.\n\n"+
			"The floor is not decoration: it is copied into CHANGELOG.md when this entry is consumed, and %s reads it from there when the services repo bumps its pin. Without it the deployment garbles the clients this entry says are refused instead of refusing them with a sentence. See %s",
			e.File, FloorConstantRef, FormatDoc)
	case e.Class != ClassBreaking && e.Floor != "":
		return fmt.Errorf("%s: `floor:` on a %s entry means nothing — only a breaking change moves the floor. Drop the line, or reclassify the entry as breaking if an already-installed client really does stop working. See %s",
			e.File, e.Class, FormatDoc)
	case e.Floor != "" && !floorPattern.MatchString(e.Floor):
		return fmt.Errorf("%s: floor %q is not a release tag — write it as vX.Y.Z, the tag whose clients still work. See %s",
			e.File, e.Floor, FormatDoc)
	}
	return nil
}

func Breaking(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Class == ClassBreaking {
			out = append(out, e)
		}
	}
	return out
}
