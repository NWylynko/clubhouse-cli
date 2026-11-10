package box

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/telemetry"
)

//go:embed Dockerfile
var dockerfile string

var log = logx.New("box")

const buildDir = "/tmp/clubhouse-box"

const DefaultRegistry = "ghcr.io/nwylynko/clubhouse-box"

const defaultWaitTimeout = 30 * time.Second

const (
	PullAlways  = "always"
	PullMissing = "missing"
)

type Runner interface {
	Run(ctx context.Context, command string) (string, error)
	Stream(ctx context.Context, command string, in io.Reader, out, errW io.Writer) error
	Put(ctx context.Context, data []byte, remotePath string) error
	PutMode(ctx context.Context, data []byte, remotePath string, mode os.FileMode) error
	Target() string
}

type Options struct {
	Image        string
	ImageRef     string
	BuildLocal   bool
	PullPolicy   string
	Binary       []byte
	PIN          string
	SeedURL      string
	Network      string
	Labels       map[string]string
	AdvertiseIP  string
	ServeOnly    bool
	Volume       string
	Dev          string
	Install      string
	Brief        string
	MemberID     string
	MemberSecret string
	envFilePath  string
	Telemetry    string
	Takeover     bool
	ExtraArgs    []string
	Container    string

	Control             *controlplane.Client
	ControlURL          string
	ContainerControlURL string
	WaitTimeout         time.Duration

	CLIVersion          string
	RequireVersionMatch bool
}

const devContainerPort = 3000

type Box struct {
	Target      string
	Container   string
	Image       string
	JoinPIN     string
	GitURL      string
	Generation  int
	ControlURL  string
	Rendezvous  string
	RawJoinInfo string
}

func DefaultImageRef(version string) string {
	tag := "latest"
	if !unpublishedVersion(version) {
		tag = strings.TrimSpace(version)
	}
	return DefaultRegistry + ":" + tag
}

func unpublishedVersion(version string) bool {
	v := strings.TrimSpace(version)
	return v == "" || strings.HasSuffix(v, "-dev") || strings.HasSuffix(v, "-dirty")
}

func (o *Options) defaults() {
	if o.BuildLocal {
		if o.Image == "" {
			o.Image = "clubhouse-box"
		}
	} else {
		if o.ImageRef == "" {
			if o.Image != "" {
				o.ImageRef = o.Image
			} else {
				o.ImageRef = DefaultImageRef(o.CLIVersion)
			}
		}
		o.PullPolicy = strings.ToLower(strings.TrimSpace(o.PullPolicy))
		if o.PullPolicy == "" {
			o.PullPolicy = PullAlways
		}
	}
	if o.Container == "" {
		o.Container = "clubhouse-" + o.PIN
	}
	if o.Network == "" {
		o.Network = "club"
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = defaultWaitTimeout
	}
}

func (o Options) runImage() string {
	if o.BuildLocal {
		return o.Image
	}
	return o.ImageRef
}

func (o Options) pullingLatestFallback() bool {
	return !o.BuildLocal && unpublishedVersion(o.CLIVersion) && o.ImageRef == DefaultImageRef(o.CLIVersion)
}

func Provision(ctx context.Context, r Runner, opt Options) (*Box, error) {
	opt.defaults()
	if opt.PIN == "" {
		return nil, fmt.Errorf("box: PIN required")
	}
	if err := opt.validate(); err != nil {
		return nil, err
	}
	if err := preflight(ctx, r, opt.BuildLocal); err != nil {
		return nil, err
	}
	if err := ensureNetwork(ctx, r, opt.Network); err != nil {
		return nil, err
	}

	cp, err := opt.controlClient()
	if err != nil {
		return nil, err
	}

	if opt.BuildLocal {
		if err := buildLocal(ctx, r, opt); err != nil {
			return nil, err
		}
	} else {
		if err := pullImage(ctx, r, opt); err != nil {
			return nil, err
		}
	}

	base := readBaseline(ctx, cp, opt.PIN)

	if opt.AdvertiseIP == "" {
		ip, err := AdvertiseIP(ctx, r)
		if err != nil {
			return nil, err
		}
		opt.AdvertiseIP = ip
	}

	if err := ensurePrivateEgressBlocked(ctx, r, opt.Network); err != nil {
		return nil, err
	}

	_, _ = r.Run(ctx, fmt.Sprintf("docker rm -f %s", sh(opt.Container)))
	if opt.MemberSecret != "" || opt.MemberID != "" {
		path := memberEnvFilePath(opt)
		if path == "" {
			return nil, fmt.Errorf("could not name an unpredictable env file for the box membership")
		}
		opt.envFilePath = path
		if err := r.PutMode(ctx, memberEnvFile(opt), path, 0o600); err != nil {
			return nil, fmt.Errorf("write member env file: %w", err)
		}
		defer func() { _, _ = r.Run(context.WithoutCancel(ctx), "rm "+sh(path)) }()
	}
	log.Infof("starting box container %s on %s", opt.Container, r.Target())
	if out, err := r.Run(ctx, dockerRun(opt)); err != nil {
		return nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}
	log.Infof("session %s box ready on %s (no host ports published)", opt.PIN, opt.AdvertiseIP)

	b := &Box{Target: r.Target(), Container: opt.Container, Image: opt.runImage(), JoinPIN: opt.PIN}
	if err := waitForEndpoints(ctx, r, opt, cp, base, b); err != nil {
		return b, fmt.Errorf("box booted but never became the session host: %w", err)
	}
	if err := checkVersion(ctx, r, opt); err != nil {
		return b, err
	}
	log.Infof("box live: git=%s control=%s", b.GitURL, b.ControlURL)
	return b, nil
}

func (o Options) validate() error {
	if o.BuildLocal {
		if len(o.Binary) == 0 {
			return fmt.Errorf("box: --build-local requires a binary (pass --binary <path>)")
		}
		return nil
	}
	switch o.PullPolicy {
	case PullAlways, PullMissing:
	default:
		return fmt.Errorf("box: unknown pull policy %q (want %s|%s)", o.PullPolicy, PullAlways, PullMissing)
	}
	if o.RequireVersionMatch && o.pullingLatestFallback() {
		return fmt.Errorf("box: --require-version-match cannot be satisfied — this CLI reports version %q, which has no published image, so it falls back to %s. "+
			"Use a released build, or pin the image with --image <ref>, or --build-local --binary <path>", o.CLIVersion, o.ImageRef)
	}
	return nil
}

func buildLocal(ctx context.Context, r Runner, opt Options) error {
	log.Infof("shipping build context to %s (%s)", r.Target(), humanBytes(len(opt.Binary)))
	if err := r.Put(ctx, opt.Binary, buildDir+"/club"); err != nil {
		return fmt.Errorf("ship binary: %w", err)
	}
	if err := r.Put(ctx, []byte(dockerfile), buildDir+"/Dockerfile"); err != nil {
		return fmt.Errorf("ship Dockerfile: %w", err)
	}
	log.Infof("building image %s on %s (first build pulls base + node, can take a minute)", opt.Image, r.Target())
	if out, err := r.Run(ctx, fmt.Sprintf("cd %s && docker build -f Dockerfile -t %s .", sh(buildDir), sh(opt.Image))); err != nil {
		return fmt.Errorf("docker build: %w\n%s", err, out)
	}
	return nil
}

func imagePresent(ctx context.Context, r Runner, ref string) bool {
	_, err := r.Run(ctx, fmt.Sprintf("docker image inspect %s", sh(ref)))
	return err == nil
}

func pullImage(ctx context.Context, r Runner, opt Options) error {
	ref := opt.ImageRef
	if opt.PullPolicy == PullMissing {
		if imagePresent(ctx, r, ref) {
			log.Infof("image %s already present on %s — skipping pull", ref, r.Target())
			return nil
		}
	}
	log.Infof("pulling image %s onto %s", ref, r.Target())
	out, err := r.Run(ctx, fmt.Sprintf("docker pull %s", sh(ref)))
	if err == nil {
		return nil
	}
	if imagePresent(ctx, r, ref) {
		log.Warnf("could not pull %s (%v) — booting the copy already on %s; if it is stale, rebuild it there with `make box-image`", ref, err, r.Target())
		return nil
	}
	return fmt.Errorf("docker pull %s: %w\n%s\n(hint: build the image on the box with `make box-image`; or `docker login` there for a private registry; or use --build-local --binary …)", ref, err, out)
}

func checkVersion(ctx context.Context, r Runner, opt Options) error {
	if opt.CLIVersion == "" {
		return nil
	}
	out, err := Exec(ctx, r, opt.Container, "--version")
	if err != nil {
		if opt.RequireVersionMatch {
			return fmt.Errorf("box: --require-version-match: could not read the container's `club --version` (image too old?): %w", err)
		}
		log.Warnf("could not read container club --version: %v", err)
		return nil
	}
	got := parseVersionOutput(out)
	if versionsMatch(got, opt.CLIVersion) {
		return nil
	}
	if opt.pullingLatestFallback() {
		log.Infof("box runs club %s (from %s); this CLI is an unreleased build (%s) — versions are not comparable", got, opt.ImageRef, opt.CLIVersion)
		return nil
	}
	msg := fmt.Sprintf("box: container runs club %s but this CLI is %s — rebuild/pull to match", got, opt.CLIVersion)
	if opt.RequireVersionMatch {
		return fmt.Errorf("%s (container %s is up on %s; `club box rm` to drop it)", msg, opt.Container, r.Target())
	}
	log.Warnf("%s", msg)
	return nil
}

func parseVersionOutput(out string) string {
	line := strings.TrimSpace(strings.Split(out, "\n")[0])
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return strings.TrimSpace(out)
	}
	return fields[len(fields)-1]
}

func versionsMatch(got, want string) bool {
	return normalizeVersion(got) == normalizeVersion(want)
}

func normalizeVersion(v string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-dirty")
}

func dockerRun(opt Options) string {
	var b strings.Builder
	b.WriteString("docker run -d")
	b.WriteString(" --init")
	fmt.Fprintf(&b, " --name %s", sh(opt.Container))
	fmt.Fprintf(&b, " --network %s", sh(opt.Network))
	for k, v := range opt.Labels {
		fmt.Fprintf(&b, " --label %s", sh(k+"="+v))
	}
	appendContainment(&b, opt)
	if opt.AdvertiseIP != "" {
		fmt.Fprintf(&b, " -e CLUB_ADVERTISE=%s", sh(opt.AdvertiseIP))
	}
	fmt.Fprintf(&b, " -e PORT=%d", devContainerPort)
	if opt.Telemetry != "" {
		fmt.Fprintf(&b, " -e %s=%s", telemetry.EnvMode, sh(opt.Telemetry))
	}
	if u := opt.containerControlURL(); u != "" {
		fmt.Fprintf(&b, " -e CLUB_CONTROL=%s", sh(u))
	}
	if opt.envFilePath != "" {
		fmt.Fprintf(&b, " --env-file %s", sh(opt.envFilePath))
	}
	b.WriteString(" " + sh(opt.runImage()))

	fmt.Fprintf(&b, " _host --pin %s", sh(opt.PIN))
	if opt.SeedURL != "" {
		fmt.Fprintf(&b, " --seed-url %s", sh(opt.SeedURL))
	}
	if opt.ServeOnly {
		b.WriteString(" --serve-only")
	}
	if opt.Dev != "" {
		fmt.Fprintf(&b, " --dev %s", sh(opt.Dev))
	}
	if opt.Install != "" {
		fmt.Fprintf(&b, " --install %s", sh(opt.Install))
	}
	if opt.Brief != "" {
		fmt.Fprintf(&b, " --brief %s", sh(opt.Brief))
	}
	if opt.Takeover {
		b.WriteString(" --takeover")
	}
	for _, a := range opt.ExtraArgs {
		b.WriteString(" " + sh(a))
	}
	return b.String()
}

func memberEnvFilePath(opt Options) string {
	if opt.MemberSecret == "" && opt.MemberID == "" {
		return ""
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return "/tmp/clubhouse-env-" + hex.EncodeToString(b[:]) + ".env"
}

func memberEnvFile(opt Options) []byte {
	var b strings.Builder
	if opt.MemberID != "" {
		fmt.Fprintf(&b, "CLUB_MEMBER_ID=%s\n", opt.MemberID)
	}
	if opt.MemberSecret != "" {
		fmt.Fprintf(&b, "CLUB_MEMBER_SECRET=%s\n", opt.MemberSecret)
	}
	return []byte(b.String())
}

func Exec(ctx context.Context, r Runner, container string, args ...string) (string, error) {
	return r.Run(ctx, execCmd(container, false, args))
}

func ExecAttached(ctx context.Context, r Runner, container string, in io.Reader, out, errW io.Writer, args ...string) error {
	return r.Stream(ctx, execCmd(container, false, args), in, out, errW)
}

func ExecDetached(ctx context.Context, r Runner, container string, args ...string) error {
	_, err := r.Run(ctx, execCmd(container, true, args))
	return err
}

func execCmd(container string, detach bool, args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = sh(a)
	}
	flag := ""
	if detach {
		flag = "-d "
	}
	return fmt.Sprintf("docker exec %s%s club %s", flag, sh(container), strings.Join(quoted, " "))
}

func Logs(ctx context.Context, r Runner, container string) (string, error) {
	return r.Run(ctx, fmt.Sprintf("docker logs %s", sh(container)))
}

func Remove(ctx context.Context, r Runner, container string) error {
	_, err := r.Run(ctx, fmt.Sprintf("docker rm -f %s", sh(container)))
	return err
}

func preflight(ctx context.Context, r Runner, buildLocal bool) error {
	if _, err := r.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		return fmt.Errorf("docker not usable on %s (is it installed and running?): %w", r.Target(), err)
	}
	arch, err := r.Run(ctx, "uname -m")
	if err != nil {
		return fmt.Errorf("probe arch on %s: %w", r.Target(), err)
	}
	a := strings.TrimSpace(arch)
	if buildLocal {
		if a != "x86_64" && a != "amd64" {
			return fmt.Errorf("box arch is %q; --build-local ships a linux/amd64 binary — use the pull path (default) on arm64, or cross-build an arm64 binary", a)
		}
		return nil
	}
	switch a {
	case "x86_64", "amd64", "aarch64", "arm64":
		return nil
	default:
		return fmt.Errorf("box arch is %q; the published box image is multi-arch linux/amd64+arm64 only — build your own image for this arch and pass --image <ref>", a)
	}
}

func ensureNetwork(ctx context.Context, r Runner, network string) error {
	switch network {
	case "", "bridge", "host", "none":
		return nil
	}
	inspect := fmt.Sprintf("docker network inspect %s", sh(network))
	if _, err := r.Run(ctx, inspect); err == nil {
		return nil
	}
	out, err := r.Run(ctx, fmt.Sprintf("docker network create %s", sh(network)))
	if err == nil {
		return nil
	}
	if _, inspectErr := r.Run(ctx, inspect); inspectErr == nil {
		return nil
	}
	return fmt.Errorf("create docker network %s on %s: %w\n%s", network, r.Target(), err, out)
}

type baseline struct {
	Generation int
	GitURL     string
}

func readBaseline(ctx context.Context, cp *controlplane.Client, pin string) baseline {
	sess, err := cp.Session(ctx, pin)
	if err != nil {
		return baseline{}
	}
	// raw endpoint ok: a baseline to compare later announcements against.
	return baseline{Generation: sess.Generation, GitURL: strings.TrimSpace(sess.Endpoints[controlplane.EndpointGit].URL)}
}

func waitForEndpoints(ctx context.Context, r Runner, opt Options, cp *controlplane.Client, base baseline, b *Box) error {
	deadline := time.Now().Add(opt.WaitTimeout)
	lastGen, lastGit := base.Generation, base.GitURL
	for {
		logsOut, _ := Logs(ctx, r, opt.Container)
		if strings.Contains(strings.ToLower(logsOut), "error:") {
			return fmt.Errorf("container reported an error:\n%s", logsOut)
		}
		if strings.Contains(logsOut, "joining as a client") {
			return fmt.Errorf("the container joined %s as a client instead of hosting it — "+
				"it never served canonical, so no dev server ran there. `box add` claims the host role with --takeover; "+
				"if you passed --no-cutover, that is expected.\ncontainer logs:\n%s", opt.PIN, logsOut)
		}
		sess, err := cp.Session(ctx, opt.PIN)
		if err == nil {
			lastGen = sess.Generation
			// raw endpoint ok: compared against the baseline above, and handed
			lastGit = strings.TrimSpace(sess.Endpoints[controlplane.EndpointGit].URL)
			if announcedByTheBox(opt, base, lastGen, lastGit) {
				b.GitURL = lastGit
				b.Generation = lastGen
				b.ControlURL = cp.Base
				b.Rendezvous = cp.Base
				b.RawJoinInfo = logsOut
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: session %s is at generation %d with git endpoint %q "+
				"(was generation %d / %q before the box booted) — the box never announced its own canonical.\ncontainer logs:\n%s",
				opt.WaitTimeout, opt.PIN, lastGen, orNone(lastGit), base.Generation, orNone(base.GitURL), logsOut)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func announcedByTheBox(opt Options, base baseline, gen int, git string) bool {
	if git == "" || git == base.GitURL || gen < base.Generation {
		return false
	}
	if opt.Takeover {
		return gen > base.Generation
	}
	return true
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func (o Options) controlClient() (*controlplane.Client, error) {
	if o.Control != nil {
		return o.Control, nil
	}
	if u := o.controlURL(); u != "" {
		return controlplane.NewClient(u), nil
	}
	return nil, fmt.Errorf("box: Control client or CLUB_CONTROL required")
}

func (o Options) controlURL() string {
	if u := strings.TrimSpace(o.ControlURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return strings.TrimRight(strings.TrimSpace(os.Getenv("CLUB_CONTROL")), "/")
}

func (o Options) containerControlURL() string {
	if u := strings.TrimSpace(o.ContainerControlURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return o.controlURL()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func sh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
