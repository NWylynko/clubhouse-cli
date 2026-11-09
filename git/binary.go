package git

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Package git is the *only* way the rest of club touches git. Every call
// goes through a hermetic wrapper around a pinned bundled binary — never the
// machine's system git — so identical merges produce identical results on every
// machine (plans/01, MASTERPLAN §9.1).

var (
	resolveMu      sync.Mutex
	resolvedPrefix string
	resolvedBin    string
	resolveErr     error
)

// Prefix returns the extracted git distribution root (contains cmd/, bin/,
// libexec/, share/).
func Prefix() (string, error) {
	home := os.Getenv("CLUB_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".club")
	}
	return filepath.Join(home, "git", Version+"-"+runtime.GOOS+"-"+runtime.GOARCH), nil
}

// Bin returns the absolute path to the bundled git launcher (cmd/git). Extracts
// the embedded archive into Prefix() on first use.
//
// The resolution is memoized per-prefix, not globally: CLUB_HOME does not
// move mid-process in production (so this is one resolve, as before), but in
// tests it moves constantly, and a globally-memoized answer meant a later test
// got a path inside an earlier test's already-deleted TempDir.
func Bin() (string, error) {
	prefix, err := Prefix()
	if err != nil {
		return "", err
	}

	resolveMu.Lock()
	defer resolveMu.Unlock()
	if resolvedPrefix == prefix && (resolvedBin != "" || resolveErr != nil) {
		return resolvedBin, resolveErr
	}
	resolvedBin, resolveErr = resolveBin()
	resolvedPrefix = prefix
	return resolvedBin, resolveErr
}

func resolveBin() (string, error) {
	prefix, err := Prefix()
	if err != nil {
		return "", err
	}
	if bin := findLauncher(prefix); bin != "" {
		return bin, nil
	}
	if len(embeddedArchive) == 0 {
		return "", fmt.Errorf("no bundled git %s is built for %s/%s", Version, runtime.GOOS, runtime.GOARCH)
	}
	if err := ExtractArchive(embeddedArchive, prefix); err != nil {
		return "", fmt.Errorf("extract bundled git: %w", err)
	}
	if bin := findLauncher(prefix); bin != "" {
		return bin, nil
	}
	return "", fmt.Errorf("bundled git launcher missing after extract into %s", prefix)
}

// findLauncher returns the git entry point inside an extracted prefix: cmd/git
// for the linux git-minimal archives, bin/git for the darwin ones we build
// (.github/workflows/bundled-git-darwin.yml). Empty when neither exists.
func findLauncher(prefix string) string {
	for _, p := range []string{filepath.Join(prefix, "cmd", "git"), filepath.Join(prefix, "bin", "git")} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// ResetResolveForTest clears the cached binary path so tests can point at a
// fresh prefix. Not for production use.
func ResetResolveForTest() {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	resolvedPrefix = ""
	resolvedBin = ""
	resolveErr = nil
}
