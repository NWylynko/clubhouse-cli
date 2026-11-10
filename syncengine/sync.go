package syncengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
)

const intentTrailer = "Clubhouse-Intent"

type Base struct {
	Ref          string
	SHA          string
	Age          time.Duration
	HubSHA       string
	HubReachable bool
}

func (b Base) Stale() bool {
	if !b.HubReachable {
		return true
	}
	return b.HubSHA != "" && b.HubSHA != b.SHA
}

func (b Base) Report() string {
	if !b.Stale() {
		return ""
	}
	if !b.HubReachable {
		return fmt.Sprintf("resolved against a %s old main (%s) — the hub was unreachable, so this base may be behind", humanAge(b.Age), shortSHA(b.SHA))
	}
	return fmt.Sprintf("resolved against a %s old main (%s) — the hub has moved on to %s, so this base is behind", humanAge(b.Age), shortSHA(b.SHA), shortSHA(b.HubSHA))
}

func (b Base) note() string {
	if !b.Stale() {
		return ""
	}
	why := "hub unreachable"
	if b.HubReachable {
		why = "hub at " + shortSHA(b.HubSHA)
	}
	return fmt.Sprintf("[base: main %s, %s old, %s]", shortSHA(b.SHA), humanAge(b.Age), why)
}

func (b Base) annotate(intent string) string {
	if intent == "" {
		return ""
	}
	if n := b.note(); n != "" {
		return intent + " " + n
	}
	return intent
}

func Push(ctx context.Context, work, branch, remote, intent string, id sbGit.Identity) error {
	if intent == "" {
		return fmt.Errorf("push requires an intent note")
	}
	c := &sbGit.Cmd{Dir: work, Env: id.EnvVars()}

	if err := commitLocal(ctx, c, intent); err != nil {
		return err
	}
	if remote == "" {
		remote = "origin"
	}
	if err := c.Run(ctx, "push", remote, "HEAD:"+branch); err != nil {
		return fmt.Errorf("could not publish %s to the hub at %s: %w\n"+
			"your work is committed locally and main is already integrated, so nothing is lost — "+
			"re-run `club sync` once the hub is reachable again", branch, remoteURL(ctx, c, remote), err)
	}
	return nil
}

func Pull(ctx context.Context, work, remote, mirrorDir, intent string, id sbGit.Identity) (Base, error) {
	c := &sbGit.Cmd{Dir: work, Env: id.EnvVars()}
	if remote == "" {
		remote = "origin"
	}

	base, err := resolveBase(ctx, c, remote, mirrorDir)
	if err != nil {
		return base, err
	}

	if err := commitLocal(ctx, c, base.annotate(intent)); err != nil {
		return base, err
	}

	if err := c.Run(ctx, "merge", "--no-edit", base.Ref); err != nil {
		return base, fmt.Errorf("pull merge conflict (resolve locally then re-sync): %w", err)
	}
	return base, nil
}

func resolveBase(ctx context.Context, c *sbGit.Cmd, remote, mirrorDir string) (Base, error) {
	if mirrorDir == "" {
		if err := c.Run(ctx, "fetch", remote, "main"); err != nil {
			return Base{}, fmt.Errorf("cannot reach the hub at %s to fetch main, and this machine keeps no local mirror of main to fall back on: %w\n"+
				"run `club join <pin>` — its daemon holds the session connection and keeps a background-fresh mirror, "+
				"which is what lets a client keep working while the hub is away", remoteURL(ctx, c, remote), err)
		}
		base := Base{Ref: remote + "/main", HubReachable: true}
		base.SHA = revParse(ctx, c, base.Ref)
		base.HubSHA = base.SHA
		base.Age = commitAge(ctx, c, base.SHA)
		return base, nil
	}

	_ = c.Run(ctx, "remote", "remove", "mirror")
	if err := c.Run(ctx, "remote", "add", "mirror", mirrorDir); err != nil {
		return Base{}, err
	}
	if err := c.Run(ctx, "fetch", "mirror", "main"); err != nil {
		return Base{}, fmt.Errorf("local main mirror at %s is unusable: %w", mirrorDir, err)
	}
	base := Base{Ref: "mirror/main"}
	base.SHA = revParse(ctx, c, base.Ref)
	base.Age = commitAge(ctx, c, base.SHA)
	if hub, err := c.Output(ctx, "ls-remote", remote, "refs/heads/main"); err == nil {
		base.HubReachable = true
		base.HubSHA = firstField(hub)
	}
	return base, nil
}

func Sync(ctx context.Context, work, branch, remote, mirrorDir, intent string, id sbGit.Identity) (Base, error) {
	if intent == "" {
		return Base{}, fmt.Errorf("sync requires an intent note")
	}
	base, err := Pull(ctx, work, remote, mirrorDir, intent, id)
	if err != nil {
		return base, fmt.Errorf("pull: %w", err)
	}
	if err := Push(ctx, work, branch, remote, base.annotate(intent), id); err != nil {
		return base, fmt.Errorf("push: %w", err)
	}
	return base, nil
}

func AnnouncePushed(ctx context.Context, client *controlplane.Client, pin, member, branch, work, intent string) {
	if client == nil || pin == "" {
		return
	}
	sha, _ := sbGit.Output(ctx, work, "rev-parse", "HEAD")
	client.PublishEventBestEffort(ctx, pin, controlplane.EventSyncPushed, map[string]any{
		"member": member, "branch": branch, "intent": intent, "sha": strings.TrimSpace(sha),
	})
}

func CloneClient(ctx context.Context, remote, work, branch string, id sbGit.Identity) error {
	if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
		return err
	}
	if err := sbGit.Run(ctx, "", "clone", remote, work); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: work, Env: id.EnvVars()}
	if err := c.Run(ctx, "checkout", "-B", branch, "origin/"+branch); err != nil {
		if err := c.Run(ctx, "checkout", "-B", branch, "origin/main"); err != nil {
			return err
		}
	}
	return c.Run(ctx, "push", "-u", "origin", branch)
}

func commitLocal(ctx context.Context, c *sbGit.Cmd, intent string) error {
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	if marked := conflictMarkerFiles(ctx, c); len(marked) > 0 {
		return fmt.Errorf("unresolved conflict markers in %s — resolve the conflict(s), then re-run sync", strings.Join(marked, ", "))
	}
	status, err := c.Output(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}
	msg := "club: local work before pull"
	if intent != "" {
		msg = fmt.Sprintf("club: %s\n\n%s: %s\n", shortIntent(intent), intentTrailer, flatten(intent))
	}
	return c.Run(ctx, "commit", "-m", msg)
}

func conflictMarkerFiles(ctx context.Context, c *sbGit.Cmd) []string {
	out, _ := c.Output(ctx, "grep", "-lI", "--no-color", "-e", "^<<<<<<< ", "-e", "^>>>>>>> ")
	seen := map[string]bool{}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			files = append(files, line)
		}
	}
	return files
}

func remoteURL(ctx context.Context, c *sbGit.Cmd, remote string) string {
	out, err := c.Output(ctx, "remote", "get-url", remote)
	if err != nil || strings.TrimSpace(out) == "" {
		return remote
	}
	return strings.TrimSpace(out)
}

func revParse(ctx context.Context, c *sbGit.Cmd, ref string) string {
	out, err := c.Output(ctx, "rev-parse", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func commitAge(ctx context.Context, c *sbGit.Cmd, sha string) time.Duration {
	if sha == "" {
		return 0
	}
	out, err := c.Output(ctx, "show", "-s", "--format=%ct", sha)
	if err != nil {
		return 0
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	age := time.Since(time.Unix(secs, 0))
	if age < 0 {
		return 0
	}
	return age
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func shortIntent(s string) string {
	s = flatten(s)
	if len(s) > 72 {
		return s[:72]
	}
	return s
}

func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func ReadIntent(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, intentTrailer+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, intentTrailer+":"))
		}
	}
	return ""
}
