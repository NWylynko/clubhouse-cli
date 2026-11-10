package scripts_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type updateEnv struct {
	t    *testing.T
	dir  string
	site *httptest.Server
}

func newUpdateEnv(t *testing.T, installed string) *updateEnv {
	t.Helper()
	env := &updateEnv{t: t, dir: t.TempDir()}
	for _, sub := range []string{"stub", "bin"} {
		if err := os.MkdirAll(filepath.Join(env.dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(env.dir, "stub", "uname"), "#!/bin/sh\n"+
		"case \"$1\" in\n"+
		"  -s) echo Linux ;;\n"+
		"  -m) echo x86_64 ;;\n"+
		"  *) echo Linux ;;\n"+
		"esac\n")
	writeFakeCurl(t, filepath.Join(env.dir, "stub"), filepath.Join(env.dir, "asked"), "the new %s")
	if installed != "" {
		writeExec(t, filepath.Join(env.dir, "bin", "club"), installed)
	}

	root := repoRoot(t)
	env.site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		body, err := os.ReadFile(filepath.Join(root, "scripts", name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/x-shellscript")
		_, _ = w.Write(body)
	}))
	t.Cleanup(env.site.Close)
	return env
}

func (e *updateEnv) run(extraEnv ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("sh", filepath.Join(repoRoot(e.t), "scripts", "update.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(e.dir, "stub")+string(os.PathListSeparator)+
			filepath.Join(e.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CLUB_SITE="+e.site.URL,
		"HOME="+filepath.Join(e.dir, "home"),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *updateEnv) installedBytes() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "bin", "club"))
	if err != nil {
		e.t.Fatalf("nothing is installed at the sandbox's bin/club: %v", err)
	}
	return string(b)
}

func TestUpdateShReplacesTheClubAlreadyOnPath(t *testing.T) {
	requireSh(t)
	requireCurl(t)
	env := newUpdateEnv(t, "#!/bin/sh\necho the old one\n")
	out, err := env.run()
	if err != nil {
		t.Fatalf("update.sh failed: %v\n%s", err, out)
	}
	if got := env.installedBytes(); !strings.Contains(got, "the new club-linux-amd64") {
		t.Fatalf("the club on PATH was not replaced — it is still %q\n%s", got, out)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "home", ".local", "bin", "club")); err == nil {
		t.Fatalf("update.sh installed a SECOND club under ~/.local/bin instead of replacing the one on PATH\n%s", out)
	}
}

func TestUpdateShPrefersTheDirectoryItIsGiven(t *testing.T) {
	requireSh(t)
	requireCurl(t)
	env := newUpdateEnv(t, "#!/bin/sh\necho the old one\n")
	elsewhere := filepath.Join(env.dir, "opt")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := env.run("CLUB_INSTALL_DIR=" + elsewhere)
	if err != nil {
		t.Fatalf("update.sh failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(elsewhere, "club"))
	if err != nil {
		t.Fatalf("update.sh ignored the directory it was given: %v\n%s", err, out)
	}
	if !strings.Contains(string(body), "the new club-linux-amd64") {
		t.Fatalf("the given directory got %q", body)
	}
	if got := env.installedBytes(); strings.Contains(got, "the new") {
		t.Fatalf("update.sh replaced the club on PATH as well as the one it was told about")
	}
}

func TestUpdateShRefusesWhenThereIsNothingToUpdate(t *testing.T) {
	requireSh(t)
	requireCurl(t)
	env := newUpdateEnv(t, "")
	out, err := env.run()
	if err == nil {
		t.Fatalf("update.sh updated a machine with no club installed:\n%s", out)
	}
	if !strings.Contains(out, "install.sh") {
		t.Fatalf("the refusal must point at the installer instead:\n%s", out)
	}
}

func TestUpdateShFetchesTheInstallerRatherThanCopyingIt(t *testing.T) {
	body := readRepoFile(t, "scripts/update.sh")
	for _, forbidden := range []string{"darwin", "aarch64", "x86_64", "club-linux", "releases/latest"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("update.sh names %q — the platform mapping and the release lookup live in install.sh, and a second copy is what installer_test.go exists to prevent", forbidden)
		}
	}
	if !strings.Contains(body, "install.sh") {
		t.Fatalf("update.sh must run install.sh:\n%s", body)
	}
}

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on this machine")
	}
}
