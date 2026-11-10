package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/logx"
)

var log = logx.New("update")

var SiteURL = "https://clubhouse.sh"

type versionAnswer struct {
	Latest string `json:"latest"`
}

func Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SiteURL+"/version", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask %s for the latest version: %w", SiteURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s asking for the latest version", SiteURL, res.Status)
	}
	var answer versionAnswer
	if err := json.NewDecoder(res.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("decode the latest version from %s: %w", SiteURL, err)
	}
	return answer.Latest, nil
}

func Behind(current, latest string) bool {
	if latest == "" || !isReleasedVersion(latest) || !isReleasedVersion(current) {
		return false
	}
	return !controlplane.ClientMeetsFloor(current, latest)
}

func isReleasedVersion(v string) bool {
	return controlplane.ClientMeetsFloor(v, "0.0.0")
}

func Check(ctx context.Context, current string) string {
	latest, err := Latest(ctx)
	if err != nil {
		log.Debugf("version check: %v", err)
		return ""
	}
	if !Behind(current, latest) {
		return ""
	}
	return fmt.Sprintf("a newer club is out: %s (you are on %s) — run `club update` when you are done", latest, current)
}

func StartCheck(ctx context.Context, current string) func() string {
	answer := make(chan string, 1)
	go func() { answer <- Check(ctx, current) }()
	return func() string {
		select {
		case line := <-answer:
			return line
		default:
			return ""
		}
	}
}

func BinaryDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the running club: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// Runs the site's update.sh rather than reimplementing it: the platform
// mapping lives in install.sh, held to the Makefile and release workflow by
// scripts/installer_test.go.
func Apply(ctx context.Context, out io.Writer, dir string) error {
	script, err := fetchScript(ctx, SiteURL+"/update.sh")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", "-s")
	cmd.Stdin = script
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(),
		"CLUB_INSTALL_DIR="+dir,
		"CLUB_SITE="+SiteURL,
	)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run the updater from %s: %w", SiteURL, err)
	}
	return nil
}

func fetchScript(ctx context.Context, url string) (io.Reader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch the updater from %s: %w", SiteURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch the updater from %s: %s", SiteURL, res.Status)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch the updater from %s: %w", SiteURL, err)
	}
	return bytes.NewReader(body), nil
}
