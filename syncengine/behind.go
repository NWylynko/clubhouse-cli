package syncengine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	sbGit "github.com/nwylynko/clubhouse-cli/git"
)

func CommitsBehindMain(ctx context.Context, work, mainRepo string) (int, error) {
	if work == "" || mainRepo == "" {
		return 0, fmt.Errorf("commits behind main: need work tree and main repo")
	}
	if err := sbGit.Run(ctx, work, "fetch", mainRepo, "+refs/heads/main:refs/remotes/mirror/main"); err != nil {
		return 0, fmt.Errorf("fetch main for behind count: %w", err)
	}
	out, err := sbGit.Output(ctx, work, "rev-list", "--count", "--no-merges", "HEAD..refs/remotes/mirror/main")
	if err != nil {
		return 0, fmt.Errorf("count behind main: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse behind count %q: %w", strings.TrimSpace(out), err)
	}
	return n, nil
}
