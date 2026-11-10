package repoguard

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/nwylynko/clubhouse-cli"

const privateModule = "github.com/nwylynko/clubhouse"

func TestNoTestInThisModuleImportsAnythingOutsideIt(t *testing.T) {
	root := repoRoot(t)
	allowed := thirdPartyRequirements(t, root)

	type offence struct{ file, imported string }
	var offences []offence

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "bundled", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			switch {
			case imported == modulePath || strings.HasPrefix(imported, modulePath+"/"):
				continue
			case imported == privateModule || strings.HasPrefix(imported, privateModule+"/"):
			case allowed[moduleOf(imported, allowed)]:
				continue
			case !strings.Contains(strings.SplitN(imported, "/", 2)[0], "."):
				continue
			}
			offences = append(offences, offence{rel, imported})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offences) == 0 {
		return
	}
	sort.Slice(offences, func(i, j int) bool { return offences[i].file < offences[j].file })
	var lines []string
	for _, o := range offences {
		lines = append(lines, "  "+o.file+" imports "+o.imported)
	}
	t.Fatalf("these tests import source that is not in this module:\n%s\n\n"+
		"A test here may import only %s, the standard library, and this module's own go.mod\n"+
		"requirements. Anything else makes this repo unbuildable for everybody outside it — the\n"+
		"private module cannot be resolved without credentials, and public CI discovers it on a\n"+
		"clean runner as \"no required module provides package\".\n\n"+
		"The fix is almost never to add the dependency. It is to move the test to the private\n"+
		"services repo as an external test package (`package foo_test` importing %s/foo), beside\n"+
		"the harness it actually needs. See AGENTS.md, \"The one rule about tests here\".",
		strings.Join(lines, "\n"), modulePath, modulePath)
}

func TestEveryPackageUnderInternalIsRepoToolingAndNotClientCode(t *testing.T) {
	root := repoRoot(t)
	var linked []string
	err := filepath.WalkDir(filepath.Join(root, "cmd"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return err
		}
		if !strings.HasPrefix(path, filepath.Join(root, "cmd", "club")+string(filepath.Separator)) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if strings.HasPrefix(imported, modulePath+"/internal/") {
				rel, _ := filepath.Rel(root, path)
				linked = append(linked, rel+" imports "+imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(linked) > 0 {
		t.Fatalf("the installed binary links a package under internal/:\n  %s\n\n"+
			"internal/ here is repo tooling — the wire guards and the release derivation — and it is\n"+
			"invisible to the private services repo. Client code that a service also needs cannot\n"+
			"live there. Move the package to the top level and accept that it is public API.",
			strings.Join(linked, "\n  "))
	}
}

func thirdPartyRequirements(t *testing.T, root string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		line, _, _ = strings.Cut(line, "//")
		line = strings.TrimPrefix(strings.TrimSpace(line), "require ")
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.Contains(fields[0], ".") && strings.HasPrefix(fields[1], "v") {
			out[fields[0]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("go.mod parsed to zero requirements — this guard would allow nothing and blame every test")
	}
	return out
}

func moduleOf(imported string, requirements map[string]bool) string {
	best := ""
	for module := range requirements {
		if imported != module && !strings.HasPrefix(imported, module+"/") {
			continue
		}
		if len(module) > len(best) {
			best = module
		}
	}
	return best
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
