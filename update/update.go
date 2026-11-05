// Package update is the client half of club's own distribution: asking the
// site what the latest version is, saying so once a session is over, and
// replacing this binary when the user asks for it.
//
// **This is the advisory half only.** A client too old to speak the wire is a
// different mechanism entirely — the control plane's version floor, which
// answers HTTP 426 pre-auth on every route (plan 48). That one refuses; this one
// only ever prints a line. Keeping them apart is deliberate: the floor is a code
// constant that ships with a deploy and is enforced by the server, and folding
// "you should update" into it would make an advisory into something a session
// could die of.
//
// Everything here is allowed to fail silently EXCEPT Apply. The check runs
// behind a session on a hackathon network; a version check that printed an
// error at the end of every offline session would be the thing people ask us to
// remove. Apply is different — the user typed a verb, so a failure has to be
// loud or they will believe they updated.
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

// SiteURL is where club asks about itself: the version API and the two
// shell scripts that install and update it.
//
// A var, not a const, for exactly one reason — the tests that drive this
// package live in another module and stand up a real HTTP server in place of
// the site (plan 49). There is deliberately no flag and no environment
// variable: this address is the same on every machine club runs on, which
// makes it a constant rather than deployment config (ADR 0006 question 1), and
// a second door onto it would be one more way for a client to be pointed at a
// version number nobody controls.
var SiteURL = "https://clubhouse.sh"

// versionAnswer is the /version body. One field, because every extra field is
// something every club ever installed has to keep parsing.
type versionAnswer struct {
	Latest string `json:"latest"`
}

// Latest asks the site for the newest released version.
//
// The site resolves this from the same GitHub release install.sh downloads
// from, so a version this returns is always one Apply can actually install.
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

// Behind reports whether `current` is a released version older than `latest`.
//
// The ordering is controlplane.ClientMeetsFloor — the SAME comparison the
// version floor is enforced with, reused rather than re-derived. A second
// ordering is a second set of rules that can disagree with the one the door
// enforces, and the disagreement would be a client told it is fine by one and
// refused by the other.
//
// Two silences are deliberate. An unparseable `latest` means the site said
// something we cannot order, and guessing is worse than saying nothing. An
// unparseable `current` — `0.0.0-dev`, an unstamped `go build`, a test binary —
// has no version to be behind: nagging every developer running their own build
// is how a line like this gets ignored by the people it is aimed at.
func Behind(current, latest string) bool {
	if latest == "" || !isReleasedVersion(latest) || !isReleasedVersion(current) {
		return false
	}
	return !controlplane.ClientMeetsFloor(current, latest)
}

// isReleasedVersion asks whether a string is a version the floor comparison can
// order at all, by asking it: everything real is at or above 0.0.0, and
// everything ClientMeetsFloor cannot parse fails closed and answers false.
func isReleasedVersion(v string) bool {
	return controlplane.ClientMeetsFloor(v, "0.0.0")
}

// Check is the whole advisory, performed: ask the site, compare, and return the
// line to print — or "" for "say nothing", which covers every failure as well
// as the ordinary case of being up to date.
func Check(ctx context.Context, current string) string {
	latest, err := Latest(ctx)
	if err != nil {
		// Silent to the user, not to a debug run: "why did it never tell me to
		// update" is a real question, and the answer is usually a network this
		// trace names. `CLUB_LOG=debug` is where it shows up.
		log.Debugf("version check: %v", err)
		return ""
	}
	if !Behind(current, latest) {
		return ""
	}
	return fmt.Sprintf("a newer club is out: %s (you are on %s) — run `club update` when you are done", latest, current)
}

// StartCheck runs Check behind whatever the caller is doing and hands back a
// reader for the answer.
//
// The shape is the point. The check starts when a session starts and is read
// when it ends, so the line lands after the console has given the screen back —
// a startup line would be cleared by the TUI a moment later, and a line printed
// mid-session would be scribbled over a dashboard. The reader NEVER blocks: an
// answer that has not arrived by the time the session ends is not worth holding
// a person's terminal for, and the request dies with the session's context.
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

// BinaryDir is the directory holding the running club — where an update has
// to land.
//
// Symlinks are resolved because installing over a symlink replaces the link and
// leaves the real binary behind: `club --version` would then report the new
// version while every other path to the same install still runs the old one.
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

// Apply replaces the club in `dir` with the latest release, by fetching the
// site's update.sh and piping it into sh.
//
// Fetching the script rather than reimplementing it is the whole design: the
// platform mapping and the release lookup live in install.sh, held to
// `make release` and the release workflow by a three-way agreement test. A Go
// reimplementation here would be a fourth party to that agreement that nothing
// checks — and the symptom of drift is a downloaded file that will not exec.
//
// It also means an update can fix the updater, which matters for the one thing
// this binary cannot do: change how it is delivered.
func Apply(ctx context.Context, out io.Writer, dir string) error {
	script, err := fetchScript(ctx, SiteURL+"/update.sh")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", "-s")
	cmd.Stdin = script
	cmd.Stdout, cmd.Stderr = out, out
	// CLUB_INSTALL_DIR aims the script at THIS binary — a club invoked
	// by an absolute path is not on PATH, so the script's own fallback would
	// replace some other club or refuse. CLUB_SITE keeps the second hop
	// (update.sh fetching install.sh) on the site this one came from, so an
	// update is one place's answer rather than two.
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
	// Read it whole before running any of it: a truncated download piped into
	// sh executes the prefix, and the prefix of an installer is the part that
	// deletes things.
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch the updater from %s: %w", SiteURL, err)
	}
	return bytes.NewReader(body), nil
}
