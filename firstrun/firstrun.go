package firstrun

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nwylynko/clubhouse-cli/devserver"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/session"
)

type Agent struct {
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
}

func (a Agent) String() string {
	if a.Harness == "" {
		return "none"
	}
	if a.Model == "" {
		return a.Harness
	}
	return a.Harness + "/" + a.Model
}

type Plan struct {
	Merger, Watcher, Setup Agent
	Brief                  string
	Box                    bool
	BoxTarget              string
	Install                []string
	Dev                    []string
	MirrorURL              string
	Seeded                 bool
}

func (p Plan) Agents() map[string]Agent {
	return map[string]Agent{
		"merger":        p.Merger,
		"error-watcher": p.Watcher,
		"setup":         p.Setup,
	}
}

func (p *Plan) SetAllAgents(a Agent) { p.Merger, p.Watcher, p.Setup = a, a, a }

const (
	QAgents  = "agents"
	QBrief   = "brief"
	QBox     = "box"
	QInstall = "install"
	QDev     = "dev"
	QMirror  = "mirror"
)

func Detect(seedDir string) Plan {
	var p Plan
	for _, a := range harness.Available() {
		if a.Present {
			p.SetAllAgents(Agent{Harness: string(a.Name)})
			break
		}
	}
	p.Seeded = seedDir != ""
	if seedDir != "" {
		p.MirrorURL = originOf(seedDir)
	}
	return p
}

func originOf(dir string) string {
	out, err := sbGit.Output(context.Background(), dir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func Ask(in io.Reader, out io.Writer, base Plan, asked map[string]bool, remembered Remembered) (Plan, error) {
	p := base
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 0, 4096), 1<<20)
	q := &prompter{in: s, out: out}

	if remembered.Exists {
		p = remembered.apply(p)
		fmt.Fprint(out, remembered.render())
		if q.yesNo("use these?", true) {
			asked = withAll(asked, QAgents, QBox, QMirror)
		} else {
			p.Box = remembered.UsedBox
			p.BoxTarget = remembered.Plan.BoxTarget
		}
	}

	avail := harness.Available()
	installed := make([]harness.Name, 0, len(avail))
	var names []string
	for _, a := range avail {
		mark := "✗"
		if a.Present {
			mark = "✓"
			installed = append(installed, a.Name)
		}
		names = append(names, fmt.Sprintf("%s %s", a.Name, mark))
	}

	if !asked[QAgents] {
		fmt.Fprintf(out, "\n  agents on PATH: %s\n\n", strings.Join(names, "  "))
		if len(installed) == 0 {
			fmt.Fprintf(out, "  no agent CLI installed — the fleet will merge mechanically and cannot scaffold.\n"+
				"  install Claude Code / Codex / Cursor and restart to get the full fleet.\n\n")
		} else if q.yesNo("use one agent for the whole fleet?", true) {
			a := q.agent("agent", installed, p.Merger)
			p.SetAllAgents(a)
		} else {
			p.Merger = q.agent("merger", installed, p.Merger)
			p.Watcher = q.agent("error-watcher", installed, p.Watcher)
			p.Setup = q.agent("setup", installed, p.Setup)
		}
	}

	if !asked[QBrief] {
		if p.Seeded {
			fmt.Fprintf(out, "\n  what should we turn this repo into?  (blank = leave it as-is)\n"+
				"  a non-blank answer lets the setup agent make ONE commit against the repo you seeded.\n")
		} else {
			fmt.Fprintf(out, "\n  what are we building?  (blank to skip)\n")
		}
		p.Brief = q.line(fmt.Sprintf("  >%s ", defaultHint(p.Brief)), p.Brief)
	}

	if !asked[QBox] {
		fmt.Fprintf(out, "\n  run this session on a box?  (we host it; your laptop can close)\n"+
			"  we are not your backup — the box is for liveness. `--mirror` snapshots to GitHub.\n")
		p.Box = q.yesNo("  box?", p.Box)
	}

	if !asked[QMirror] {
		fmt.Fprintf(out, "\n  blank is safe: `club export <path>` copies your workspace out whenever you like,\n"+
			"  as a plain git repo with no account involved.\n")
		p.MirrorURL = q.line(fmt.Sprintf("  github snapshot   (blank = off)%s > ", defaultHint(p.MirrorURL)), p.MirrorURL)
		if p.MirrorURL != "" {
			if name, tok := MirrorCredential(); tok != "" {
				fmt.Fprintf(out, "    ✓ %s found — mirroring from this machine (the token never leaves it)\n", name)
			} else {
				fmt.Fprintf(out, "    ! no GitHub token in this environment (GITHUB_TOKEN / GH_TOKEN) —\n"+
					"      the URL is recorded, but mirroring stays paused until one is set here\n")
			}
		}
	}

	if err := s.Err(); err != nil {
		return p, err
	}
	return p, nil
}

func MirrorCredential() (string, string) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(name); v != "" {
			return name, v
		}
	}
	return "", ""
}

type prompter struct {
	in  *bufio.Scanner
	out io.Writer
}

func (q *prompter) line(prompt, def string) string {
	fmt.Fprint(q.out, prompt)
	if !q.in.Scan() {
		fmt.Fprintln(q.out)
		return def
	}
	txt := strings.TrimSpace(q.in.Text())
	if txt == "" {
		return def
	}
	return txt
}

func (q *prompter) yesNo(question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		switch strings.ToLower(q.line(fmt.Sprintf("  %s %s > ", question, hint), "")) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(q.out, "  please answer y or n")
	}
}

func (q *prompter) agent(role string, installed []harness.Name, def Agent) Agent {
	if len(installed) == 0 {
		return Agent{}
	}
	var menu []string
	for i, n := range installed {
		menu = append(menu, fmt.Sprintf("[%d] %s", i+1, n))
	}
	pick := def
	for {
		ans := q.line(fmt.Sprintf("    %-14s %s%s > ", role, strings.Join(menu, " "), defaultHint(def.Harness)), def.Harness)
		chosen := ""
		for i, n := range installed {
			if ans == fmt.Sprint(i+1) || strings.EqualFold(ans, string(n)) {
				chosen = string(n)
				break
			}
		}
		if chosen != "" {
			pick.Harness = chosen
			break
		}
		fmt.Fprintf(q.out, "    %q is not installed on this machine — pick one of: %s\n",
			ans, strings.Join(menu, " "))
	}
	hints := harness.SuggestedModels(harness.Name(pick.Harness))
	label := fmt.Sprintf("[%s default]", pick.Harness)
	if len(hints) > 0 {
		label = fmt.Sprintf("[%s default; e.g. %s]", pick.Harness, strings.Join(hints, ", "))
	}
	model := def.Model
	if def.Harness != pick.Harness {
		model = ""
	}
	pick.Model = q.line(fmt.Sprintf("      model     %s%s > ", label, defaultHint(model)), model)
	return pick
}

func defaultHint(def string) string {
	if def == "" {
		return ""
	}
	return "  [" + def + "]"
}

func splitCmd(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

func defaultsFile() string { return filepath.Join(session.Home(), "defaults.json") }

type machineDefaults struct {
	Merger    Agent  `json:"merger,omitempty"`
	Watcher   Agent  `json:"watcher,omitempty"`
	Setup     Agent  `json:"setup,omitempty"`
	Brief     string `json:"brief,omitempty"`
	BoxTarget string `json:"boxTarget,omitempty"`
	MirrorURL string `json:"mirrorUrl,omitempty"`
	Telemetry string `json:"telemetry,omitempty"`
	UsedBox   bool   `json:"usedBox,omitempty"`
}

type Remembered struct {
	Plan    Plan
	UsedBox bool
	Exists  bool
}

func Recall() Remembered {
	d, err := loadDefaults()
	if err != nil {
		return Remembered{}
	}
	return Remembered{
		Exists:  true,
		UsedBox: d.UsedBox,
		Plan: Plan{
			Merger: d.Merger, Watcher: d.Watcher, Setup: d.Setup,
			Brief: d.Brief, BoxTarget: d.BoxTarget, MirrorURL: d.MirrorURL,
		},
	}
}

func (r Remembered) apply(p Plan) Plan {
	p = ApplyDefaults(p, r.Plan)
	p.Box = r.UsedBox
	if !r.UsedBox {
		p.BoxTarget = ""
	}
	return p
}

func (r Remembered) render() string {
	var b strings.Builder
	b.WriteString("\n  last time on this machine:\n")
	agents := r.Plan.Merger.String()
	if r.Plan.Watcher != r.Plan.Merger || r.Plan.Setup != r.Plan.Merger {
		agents = fmt.Sprintf("merger %s  error-watcher %s  setup %s",
			r.Plan.Merger, r.Plan.Watcher, r.Plan.Setup)
	}
	fmt.Fprintf(&b, "    agents  %s\n", agents)
	switch {
	case r.UsedBox && r.Plan.BoxTarget != "":
		fmt.Fprintf(&b, "    box     %s (your machine)\n", r.Plan.BoxTarget)
	case r.UsedBox:
		b.WriteString("    box     yes (we provision it)\n")
	default:
		b.WriteString("    box     none\n")
	}
	if r.Plan.MirrorURL != "" {
		fmt.Fprintf(&b, "    mirror  %s\n", r.Plan.MirrorURL)
	}
	b.WriteString("\n")
	return b.String()
}

func withAll(asked map[string]bool, keys ...string) map[string]bool {
	out := map[string]bool{}
	for k, v := range asked {
		out[k] = v
	}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func loadDefaults() (machineDefaults, error) {
	var d machineDefaults
	data, err := os.ReadFile(defaultsFile())
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, err
	}
	return d, nil
}

func Defaults() Plan { return Recall().Plan }

func SaveDefaults(p Plan) error {
	if err := os.MkdirAll(session.Home(), 0o755); err != nil {
		return err
	}
	d := machineDefaults{
		Merger: p.Merger, Watcher: p.Watcher, Setup: p.Setup,
		Brief: p.Brief, BoxTarget: p.BoxTarget, MirrorURL: p.MirrorURL,
		UsedBox: p.Box || p.BoxTarget != "",
	}
	if prev, err := loadDefaults(); err == nil {
		d.Telemetry = prev.Telemetry
	}
	if !d.UsedBox {
		if prev, err := loadDefaults(); err == nil {
			d.BoxTarget = prev.BoxTarget
		}
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(defaultsFile(), append(data, '\n'), 0o644)
}

func ApplyDefaults(base, d Plan) Plan {
	if d.Merger.Harness != "" && harness.IsAvailable(d.Merger.Harness) {
		base.Merger = d.Merger
	}
	if d.Watcher.Harness != "" && harness.IsAvailable(d.Watcher.Harness) {
		base.Watcher = d.Watcher
	}
	if d.Setup.Harness != "" && harness.IsAvailable(d.Setup.Harness) {
		base.Setup = d.Setup
	}
	if d.MirrorURL != "" && base.MirrorURL == "" {
		base.MirrorURL = d.MirrorURL
	}
	if d.Brief != "" && base.Brief == "" && !base.Seeded {
		base.Brief = d.Brief
	}
	return base
}

func ResolveCommands(p Plan, workDir string) Plan {
	if len(p.Install) == 0 {
		p.Install = devserver.DetectInstall(workDir)
	}
	if len(p.Dev) == 0 {
		p.Dev = devserver.DetectDev(workDir)
	}
	return p
}
