package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestInstallShFetchesTheAssetForThisPlatform(t *testing.T) {
	requireSh(t)
	for _, tc := range []struct {
		unameS, unameM, want string
	}{
		{"Darwin", "arm64", "club-darwin-arm64"},
		{"Darwin", "x86_64", "club-darwin-amd64"},
		{"Linux", "aarch64", "club-linux-arm64"},
		{"Linux", "x86_64", "club-linux-amd64"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			env := newInstallEnv(t, tc.unameS, tc.unameM)
			out, err := env.run()
			if err != nil {
				t.Fatalf("install.sh failed: %v\n%s", err, out)
			}
			if got := env.asked(); got != tc.want {
				t.Fatalf("installer asked the release for %q, want %q\n%s", got, tc.want, out)
			}
			bin := filepath.Join(env.dir, "bin", "club")
			info, err := os.Stat(bin)
			if err != nil {
				t.Fatalf("nothing was installed: %v\n%s", err, out)
			}
			if info.Mode()&0o111 == 0 {
				t.Fatalf("installed binary is not executable: %v", info.Mode())
			}
			body, _ := os.ReadFile(bin)
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("installed the wrong asset: %q", body)
			}
		})
	}
}

func TestInstallShRefusesAPlatformThatHasNoAsset(t *testing.T) {
	requireSh(t)
	env := newInstallEnv(t, "Linux", "riscv64")
	out, err := env.run()
	if err == nil {
		t.Fatalf("install.sh accepted an unreleased architecture:\n%s", out)
	}
	if !strings.Contains(out, "riscv64") {
		t.Fatalf("the refusal must name the architecture it cannot serve:\n%s", out)
	}
}

func TestTheInstallerNamesTheFilesMakeReleaseWrites(t *testing.T) {
	requireSh(t)
	built := map[string]bool{}
	for _, line := range strings.Split(readRepoFile(t, "Makefile"), "\n") {
		if !strings.HasPrefix(line, "PLATFORMS ") && !strings.HasPrefix(line, "PLATFORMS:=") && !strings.HasPrefix(line, "PLATFORMS :=") {
			continue
		}
		_, rhs, _ := strings.Cut(line, ":=")
		for _, p := range strings.Fields(rhs) {
			os, arch, _ := strings.Cut(p, "/")
			built["club-"+os+"-"+arch] = true
		}
	}
	if len(built) != 4 {
		t.Fatalf("expected four released platforms from the Makefile, got %v", built)
	}
	for _, tc := range []struct{ unameS, unameM string }{
		{"Darwin", "arm64"}, {"Darwin", "x86_64"}, {"Linux", "aarch64"}, {"Linux", "x86_64"},
	} {
		env := newInstallEnv(t, tc.unameS, tc.unameM)
		if out, err := env.run(); err != nil {
			t.Fatalf("install.sh failed: %v\n%s", err, out)
		}
		asked := env.asked()
		if !built[asked] {
			t.Fatalf("the installer asks for %q, which `make release` never writes (it writes %v)", asked, built)
		}
		delete(built, asked)
	}
	if len(built) != 0 {
		t.Fatalf("released platforms no uname maps to: %v", built)
	}
}

func TestTheReleaseWorkflowPublishesTheMatrixOnATag(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release-cli.yml")
	for _, want := range []string{
		`- "v*"`,
		"fetch-depth: 0",
		"contents: write",
		"CONTROL_URL=",
		"dist/club-*",
		"softprops/action-gh-release",
	} {
		if !strings.Contains(wf, want) {
			t.Fatalf("release-cli.yml is missing %q:\n%s", want, wf)
		}
	}
	if !strings.Contains(wf, "startsWith(github.ref, 'refs/tags/v')") {
		t.Fatalf("the release step must be gated on a tag ref:\n%s", wf)
	}
}

func TestPublishingWaitsForAHuman(t *testing.T) {
	for _, wf := range []string{"release-cli.yml", "box-image.yml"} {
		body := readRepoFile(t, ".github/workflows/"+wf)
		if !strings.Contains(body, "environment: release") {
			t.Errorf("%s publishes without `environment: release` — a tag would put artifacts in front of "+
				"strangers with nobody asked. The required reviewer itself is a repository setting "+
				"(Settings → Environments → release); this line is what routes the job through it.\n%s", wf, body)
		}
	}
	wf := readRepoFile(t, ".github/workflows/release-cli.yml")
	env := strings.Index(wf, "environment: release")
	upload := strings.Index(wf, "actions/upload-artifact")
	download := strings.Index(wf, "actions/download-artifact")
	publish := strings.Index(wf, "softprops/action-gh-release")
	if env < 0 || upload < 0 || download < 0 || publish < 0 || !(upload < env && env < download && download < publish) {
		t.Fatalf("release-cli.yml must build and upload the matrix BEFORE the gated job downloads and publishes it — "+
			"otherwise the approver is blessing a build that has not run yet:\n%s", wf)
	}
}

func TestTheBoxImageWorkflowFiresOnTheSameTag(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/box-image.yml")
	if !strings.Contains(wf, `- "v*"`) {
		t.Fatalf("box-image.yml must fire on the same v* tag:\n%s", wf)
	}
}

func TestInstallShWithAPinJoinsTheSession(t *testing.T) {
	requireSh(t)
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("no util-linux script(1) to give the installer a terminal")
	}
	env := newInstallEnv(t, "Linux", "x86_64")
	ran := env.fakeReleaseRecordsItsRuns()
	root := repoRoot(t)
	cmd := exec.Command("script", "-qec", "echo | sh "+filepath.Join(root, "scripts", "install.sh")+" abc123", "/dev/null")
	out, err := env.runCmd(cmd)
	if err != nil {
		t.Fatalf("install.sh with a pin failed: %v\n%s", err, out)
	}
	runs := readFile(t, ran)
	if !strings.Contains(runs, "args=join abc123 stdin=tty") {
		t.Fatalf("the installed club was not run as `join abc123` on the terminal; runs:\n%s\noutput:\n%s", runs, out)
	}
}

func TestInstallShWithAPinAndNoTerminalSaysSo(t *testing.T) {
	requireSh(t)
	env := newInstallEnv(t, "Linux", "x86_64")
	ran := env.fakeReleaseRecordsItsRuns()
	cmd := exec.Command("setsid", "-w", "sh", filepath.Join(repoRoot(t), "scripts", "install.sh"), "abc123")
	out, err := env.runCmd(cmd)
	if err == nil {
		t.Fatalf("install.sh with a pin and no terminal exited 0:\n%s", out)
	}
	if !strings.Contains(out, "club join abc123") {
		t.Fatalf("the refusal must name the join command to run:\n%s", out)
	}
	if strings.Contains(readFile(t, ran), "join") {
		t.Fatalf("club join ran with no terminal:\n%s", readFile(t, ran))
	}
	if _, err := os.Stat(filepath.Join(env.dir, "bin", "club")); err != nil {
		t.Fatalf("the binary must still be installed: %v", err)
	}
}

func TestInstallShPutsClubOnThePathOfTheUsersShell(t *testing.T) {
	requireSh(t)
	for _, tc := range []struct {
		name, unameS, shell, rc, want string
	}{
		{"zsh", "Darwin", "/bin/zsh", ".zshrc", `export PATH="%s:$PATH"`},
		{"bash-linux", "Linux", "/bin/bash", ".bashrc", `export PATH="%s:$PATH"`},
		{"bash-darwin", "Darwin", "/bin/bash", ".bash_profile", `export PATH="%s:$PATH"`},
		{"fish", "Linux", "/usr/bin/fish", ".config/fish/config.fish", `fish_add_path %s`},
		{"other", "Linux", "/bin/dash", ".profile", `export PATH="%s:$PATH"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newInstallEnv(t, tc.unameS, "arm64")
			env.shell = tc.shell
			for i := 0; i < 2; i++ {
				if out, err := env.run(); err != nil {
					t.Fatalf("install.sh failed: %v\n%s", err, out)
				}
			}
			body := readFile(t, filepath.Join(env.dir, tc.rc))
			line := fmt.Sprintf(tc.want, filepath.Join(env.dir, "bin"))
			if n := strings.Count(body, line); n != 1 {
				t.Fatalf("%s holds the PATH line %d times, want once:\n%s", tc.rc, n, body)
			}
		})
	}
}

func TestInstallShLeavesRcAloneWhenClubIsAlreadyOnPath(t *testing.T) {
	requireSh(t)
	env := newInstallEnv(t, "Darwin", "arm64")
	env.shell = "/bin/zsh"
	env.destOnPath = true
	if out, err := env.run(); err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(env.dir, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf(".zshrc was touched though the install dir is already on PATH")
	}
}

func (e *installEnv) fakeReleaseRecordsItsRuns() string {
	e.t.Helper()
	ran := filepath.Join(e.dir, "ran")
	writeFakeCurl(e.t, filepath.Join(e.dir, "stub"), filepath.Join(e.dir, "asked"),
		"#!/bin/sh\\n# %s\\nif [ -t 0 ]; then in=tty; else in=pipe; fi\\necho \"args=$* stdin=$in\" >> "+ran)
	if err := os.WriteFile(ran, nil, 0o644); err != nil {
		e.t.Fatal(err)
	}
	return ran
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type installEnv struct {
	t          *testing.T
	dir        string
	shell      string
	destOnPath bool
}

func newInstallEnv(t *testing.T, unameS, unameM string) *installEnv {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeExec(t, filepath.Join(stub, "uname"), "#!/bin/sh\n"+
		"case \"$1\" in\n"+
		"  -s) echo "+unameS+" ;;\n"+
		"  -m) echo "+unameM+" ;;\n"+
		"  *) echo "+unameS+" ;;\n"+
		"esac\n")
	writeFakeCurl(t, stub, filepath.Join(dir, "asked"), "%s")
	writeExec(t, filepath.Join(stub, "gh"), "#!/bin/sh\necho 'fake gh: the installer must not use gh' >&2; exit 2\n")
	return &installEnv{t: t, dir: dir}
}

func (e *installEnv) run(args ...string) (string, error) {
	e.t.Helper()
	root := repoRoot(e.t)
	return e.runCmd(exec.Command("sh", append([]string{filepath.Join(root, "scripts", "install.sh")}, args...)...))
}

func (e *installEnv) runCmd(cmd *exec.Cmd) (string, error) {
	e.t.Helper()
	path := filepath.Join(e.dir, "stub") + string(os.PathListSeparator) + os.Getenv("PATH")
	if e.destOnPath {
		path = filepath.Join(e.dir, "bin") + string(os.PathListSeparator) + path
	}
	shell := e.shell
	if shell == "" {
		shell = "/bin/sh"
	}
	env := []string{
		"PATH=" + path,
		"SHELL=" + shell,
		"CLUB_INSTALL_DIR=" + filepath.Join(e.dir, "bin"),
		"HOME=" + e.dir,
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "GH_TOKEN=") || strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "SHELL=") || strings.HasPrefix(kv, "ZDOTDIR=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *installEnv) asked() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "asked"))
	if err != nil {
		e.t.Fatalf("the installer never asked the release for anything: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func writeFakeCurl(t *testing.T, stubDir, askedFile, bodyFmt string) {
	t.Helper()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl on this machine")
	}
	writeExec(t, filepath.Join(stubDir, "curl"), "#!/bin/sh\n"+
		"out=; url=\n"+
		"for a in \"$@\"; do\n"+
		"  case \"$prev\" in -o) out=$a ;; esac\n"+
		"  case \"$a\" in -*) ;; *) url=$a ;; esac\n"+
		"  prev=$a\n"+
		"done\n"+
		"case \"$url\" in\n"+
		"  https://github.com/*/releases/*)\n"+
		"    [ -n \"$out\" ] || { echo 'fake curl: release fetched without -o' >&2; exit 2; }\n"+
		"    asset=${url##*/}\n"+
		"    printf '%s\\n' \"$asset\" > "+askedFile+"\n"+
		"    printf '"+bodyFmt+"\\n' \"$asset\" > \"$out\" ;;\n"+
		"  *) exec "+real+" \"$@\" ;;\n"+
		"esac\n")
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh on this machine")
	}
}

func TestTheBoxImageStampsTheVersionItPutsOnTheWire(t *testing.T) {
	df := readRepoFile(t, "box/Dockerfile.ci")
	const stamp = "-X github.com/nwylynko/clubhouse-cli/controlplane.ClientVersion=${VERSION}"
	if !strings.Contains(df, stamp) {
		t.Fatalf("box/Dockerfile.ci never stamps ClientVersion, so every published box image presents "+
			"0.0.0-dev on the wire — below every floor, and invisible until a floor exists.\n"+
			"\twant a link flag containing %s\n%s", stamp, df)
	}
}
