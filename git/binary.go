package git

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	resolveMu      sync.Mutex
	resolvedPrefix string
	resolvedBin    string
	resolveErr     error
)

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

// cmd/git is the linux git-minimal layout; bin/git is the darwin build from
// .github/workflows/bundled-git-darwin.yml.
func findLauncher(prefix string) string {
	for _, p := range []string{filepath.Join(prefix, "cmd", "git"), filepath.Join(prefix, "bin", "git")} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func ResetResolveForTest() {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	resolvedPrefix = ""
	resolvedBin = ""
	resolveErr = nil
}
