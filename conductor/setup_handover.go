package conductor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nwylynko/clubhouse-cli/contracts"
)

const privateDir = ".club"

const incomingDir = ".clubhouse-incoming"

type heldFiles struct {
	dir       string
	private   bool
	contracts map[string]string
}

func setAside(work string) (*heldFiles, error) {
	hold, err := os.MkdirTemp("", "clubhouse-held-*")
	if err != nil {
		return nil, err
	}
	h := &heldFiles{dir: hold, contracts: map[string]string{}}
	if src := filepath.Join(work, privateDir); exists(src) {
		if err := os.Rename(src, filepath.Join(hold, privateDir)); err != nil {
			return nil, fmt.Errorf("set aside %s: %w", privateDir, err)
		}
		h.private = true
	}
	for _, rel := range contracts.Files {
		src := filepath.Join(work, filepath.FromSlash(rel))
		if !exists(src) {
			continue
		}
		dst := filepath.Join(hold, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(src, dst); err != nil {
			return nil, fmt.Errorf("set aside %s: %w", rel, err)
		}
		h.contracts[rel] = dst
		pruneEmptyDirs(work, filepath.Dir(src))
	}
	return h, nil
}

func (h *heldFiles) restore(work string) ([]string, error) {
	if h.private {
		dst := filepath.Join(work, privateDir)
		if err := os.RemoveAll(dst); err != nil {
			return nil, err
		}
		if err := os.Rename(filepath.Join(h.dir, privateDir), dst); err != nil {
			return nil, fmt.Errorf("restore %s: %w", privateDir, err)
		}
	}
	var collided []string
	for _, rel := range contracts.Files {
		held, ok := h.contracts[rel]
		if !ok {
			continue
		}
		dst := filepath.Join(work, filepath.FromSlash(rel))
		if exists(dst) {
			in := filepath.Join(work, incomingDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(in), 0o755); err != nil {
				return nil, err
			}
			if err := os.Rename(held, in); err != nil {
				return nil, fmt.Errorf("stage %s for merge: %w", rel, err)
			}
			collided = append(collided, rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(held, dst); err != nil {
			return nil, fmt.Errorf("restore %s: %w", rel, err)
		}
	}
	sort.Strings(collided)
	return collided, nil
}

func (h *heldFiles) cleanup() {
	if h != nil && h.dir != "" {
		_ = os.RemoveAll(h.dir)
	}
}

func (h *heldFiles) verify(work string) error {
	if exists(filepath.Join(work, incomingDir)) {
		return fmt.Errorf("%s still holds files the merge turn did not fold in", incomingDir)
	}
	for _, rel := range contracts.Files {
		if _, held := h.contracts[rel]; !held {
			continue
		}
		body, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("%s went missing during the merge: %w", rel, err)
		}
		if needle := contracts.Requires(rel); !strings.Contains(string(body), needle) {
			return fmt.Errorf("%s no longer contains %q — without it agents edit happily and never land anything",
				rel, needle)
		}
	}
	return nil
}

func mergePrompt(collided []string) string {
	var b strings.Builder
	b.WriteString(`You are club's setup role-agent, continuing from the project you just created.

You wrote your own versions of files that club also maintains. Both matter: yours carries what you know about the project, club's carries the protocol every agent in this session follows. Merge them — do not pick a side.

`)
	for _, rel := range collided {
		fmt.Fprintf(&b, "- %s — club's copy is at %s/%s, and the merged file MUST still contain %q\n",
			rel, incomingDir, rel, contracts.Requires(rel))
	}
	fmt.Fprintf(&b, `
For each file: keep your content, fold club's in, and write the result back to the original path.

Hard rules:
- Each file MUST still contain the text named beside it above. That is how work reaches the rest of the team; dropping it silently breaks the session.
- Delete the %s directory when you are done. It is scaffolding, not part of the project.
- Do not create, edit or delete anything under %s/, and NEVER run any git command — club makes the commit.
`, incomingDir, privateDir)
	return b.String()
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func pruneEmptyDirs(root, dir string) {
	root = filepath.Clean(root)
	for dir = filepath.Clean(dir); strings.HasPrefix(dir, root) && dir != root; dir = filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}

func (s *Setup) scaffoldHandover(runCtx context.Context, work, prompt string, onActivity func(string)) error {
	held, err := setAside(work)
	if err != nil {
		return err
	}
	defer held.cleanup()

	if err := s.Agent(runCtx, work, prompt, onActivity); err != nil {
		return err
	}
	collided, err := held.restore(work)
	if err != nil {
		return err
	}
	if len(collided) > 0 {
		setupLog.Infof("%s wrote its own %s — asking it to merge club's copy in",
			s.harnessName(), strings.Join(collided, ", "))
		if err := s.Agent(runCtx, work, mergePrompt(collided), onActivity); err != nil {
			return err
		}
	}
	return held.verify(work)
}
