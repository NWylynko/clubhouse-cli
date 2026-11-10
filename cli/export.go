package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/spf13/cobra"
)

func newExportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "export <path>",
		Short: "Copy this session's workspace out to a standalone git repository you own",
		Long: "Writes the session's workspace to <path> as a normal git repository: the full\n" +
			"session history, your branch checked out exactly as it stands (uncommitted and\n" +
			"untracked work included), `main` beside it as the merged product, and no remote\n" +
			"at all — nothing in it points back at a session that stops existing.\n\n" +
			"Files git was told to ignore stay behind, so node_modules does not come with it.\n" +
			"The session is only read: nothing is committed, checked out or pushed.",
		Args: cobra.ExactArgs(1),
		RunE: runExport,
	}
	addPinFlag(c)
	return c
}

func runExport(cmd *cobra.Command, args []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	src := workDir(p)
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		return fmt.Errorf("no work tree to export at %s — join the session first with `club join %s`", src, s.PIN)
	}
	dest, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	if dirInside(resolvePath(src), resolvePath(dest)) {
		return fmt.Errorf("%s is inside the session — export copies your work OUT of it, so pick a path of your own (e.g. ~/dev/my-project)", dest)
	}
	if err := requireEmptyExportDir(dest); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if err := sbGit.Run(ctx, "", "clone", "--no-hardlinks", "-q", src, dest); err != nil {
		return fmt.Errorf("export %s: %w", src, err)
	}
	hasMain, err := exportFreshestMain(ctx, p, src, dest)
	if err != nil {
		return err
	}
	if err := sbGit.Run(ctx, dest, "remote", "remove", "origin"); err != nil {
		return err
	}
	if err := exportDirtyState(ctx, src, dest); err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout(), dest)
	head, err := sbGit.Output(ctx, dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	errW := cmd.ErrOrStderr()
	fmt.Fprintf(errW, "exported %s → %s\n", s.PIN, dest)
	if hasMain {
		fmt.Fprintf(errW, "on %s (your work, uncommitted bits and all), with main beside it — `git checkout main` for the merged product\n", strings.TrimSpace(head))
	} else {
		fmt.Fprintf(errW, "on %s (your work, uncommitted bits and all)\n"+
			"warning: no main came across — this machine has neither a fresh mirror of main nor an origin/main\n"+
			"         to read it from, so the export holds this branch only, not everyone's merged work\n",
			strings.TrimSpace(head))
	}
	fmt.Fprintln(errW, "a plain git repo with no remote: `git remote add origin <url>` if and when you pick one")
	return nil
}

func requireEmptyExportDir(dest string) error {
	st, err := os.Stat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is a file — export writes a new repository directory, so pick a path that is free", dest)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty — export writes a fresh repository and will not write over one, so pick a path that is free", dest)
	}
	return nil
}

func exportFreshestMain(ctx context.Context, p session.Paths, src, dest string) (bool, error) {
	head, err := sbGit.Output(ctx, dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(head) == "main" {
		return true, nil
	}
	repo, ref := freshestMainRef(ctx, p, src)
	if repo == "" {
		return false, nil
	}
	if err := sbGit.Run(ctx, dest, "fetch", "--no-tags", "-q", repo, "+"+ref+":refs/heads/main"); err != nil {
		return false, err
	}
	return true, nil
}

func freshestMainRef(ctx context.Context, p session.Paths, src string) (repo, ref string) {
	if m := mirrorDir(p); m != "" {
		if _, err := sbGit.Output(ctx, m, "rev-parse", "--verify", "-q", "refs/heads/main"); err == nil {
			return m, "refs/heads/main"
		}
	}
	if _, err := sbGit.Output(ctx, src, "rev-parse", "--verify", "-q", "refs/remotes/origin/main"); err == nil {
		return src, "refs/remotes/origin/main"
	}
	return "", ""
}

func exportDirtyState(ctx context.Context, src, dest string) error {
	patch, err := sbGit.Output(ctx, src, "diff", "HEAD", "--binary")
	if err != nil {
		return fmt.Errorf("read uncommitted work in %s: %w", src, err)
	}
	if strings.TrimSpace(patch) != "" {
		c := &sbGit.Cmd{Dir: dest, Stdin: []byte(patch)}
		if err := c.Run(ctx, "apply", "--binary", "-"); err != nil {
			return fmt.Errorf("carry uncommitted work into %s: %w", dest, err)
		}
	}
	return exportUntracked(ctx, src, dest)
}

func exportUntracked(ctx context.Context, src, dest string) error {
	out, err := sbGit.Output(ctx, src, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return fmt.Errorf("list untracked work in %s: %w", src, err)
	}
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" {
			continue
		}
		if err := copyExportFile(filepath.Join(src, rel), filepath.Join(dest, rel)); err != nil {
			return fmt.Errorf("export untracked %s: %w", rel, err)
		}
	}
	return nil
}

func copyExportFile(from, to string) error {
	st, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	w, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, in); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}
