package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nwylynko/clubhouse-cli/box"
	"github.com/nwylynko/clubhouse-cli/canonical"
	"github.com/nwylynko/clubhouse-cli/conductor"
	"github.com/nwylynko/clubhouse-cli/console"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/firstrun"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/harness"
	"github.com/nwylynko/clubhouse-cli/hoststart"
	"github.com/nwylynko/clubhouse-cli/joindaemon"
	"github.com/nwylynko/clubhouse-cli/runfile"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/nwylynko/clubhouse-cli/trust"
	"github.com/spf13/cobra"
)

func interactive(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup("interactive")
	if f == nil {
		return false
	}
	if f.Changed {
		on, _ := cmd.Flags().GetBool("interactive")
		return on
	}
	if once, _ := cmd.Flags().GetBool("once"); once {
		return false
	}
	if serveOnly, _ := cmd.Flags().GetBool("serve-only"); serveOnly {
		return false
	}
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

func isCharDevice(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func resolveFirstRun(cmd *cobra.Command, seedDir string) (firstrun.Plan, bool, error) {
	plan := firstrun.Detect(seedDir)
	asked := map[string]bool{}

	conductorFlag, _ := cmd.Flags().GetString("conductor")
	modelFlag, _ := cmd.Flags().GetString("model")
	if conductorFlag != "" {
		plan.SetAllAgents(firstrun.Agent{Harness: conductorFlag, Model: modelFlag})
	}
	perRole := false
	for flag, set := range map[string]*firstrun.Agent{
		"agent-merger": &plan.Merger, "agent-watcher": &plan.Watcher, "agent-setup": &plan.Setup,
	} {
		v, _ := cmd.Flags().GetString(flag)
		if v == "" {
			continue
		}
		perRole = true
		*set = parseAgent(v)
	}
	if conductorFlag != "" || perRole {
		asked[firstrun.QAgents] = true
	}
	if brief, _ := cmd.Flags().GetString("brief"); brief != "" {
		plan.Brief, asked[firstrun.QBrief] = brief, true
	}
	if cmd.Flags().Changed("box") {
		plan.Box, _ = cmd.Flags().GetBool("box")
		asked[firstrun.QBox] = true
	}
	if target, _ := cmd.Flags().GetString("box-ssh"); target != "" {
		plan.BoxTarget, asked[firstrun.QBox] = target, true
	}
	if v, _ := cmd.Flags().GetString("install"); v != "" {
		plan.Install, asked[firstrun.QInstall] = strings.Fields(v), true
	}
	if v, _ := cmd.Flags().GetString("dev"); v != "" {
		plan.Dev, asked[firstrun.QDev] = strings.Fields(v), true
	}
	if v, _ := cmd.Flags().GetString("mirror"); v != "" {
		plan.MirrorURL, asked[firstrun.QMirror] = v, true
	}

	if !interactive(cmd) {
		return plan, false, nil
	}
	plan, err := firstrun.Ask(cmd.InOrStdin(), cmd.OutOrStdout(), plan, asked, firstrun.Recall())
	if err != nil {
		return plan, true, err
	}
	_ = firstrun.SaveDefaults(plan)
	return plan, true, nil
}

func parseAgent(v string) firstrun.Agent {
	name, model, _ := strings.Cut(v, ":")
	return firstrun.Agent{Harness: strings.TrimSpace(name), Model: strings.TrimSpace(model)}
}

func agentsOption(p firstrun.Plan) map[string]controlplane.RoleAgent {
	out := map[string]controlplane.RoleAgent{}
	for role, a := range p.Agents() {
		if a.Harness != "" {
			out[role] = controlplane.RoleAgent{Harness: a.Harness, Model: a.Model}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func preflightBox(ctx context.Context, cmd *cobra.Command, target string, out io.Writer) error {
	r, err := boxRunnerForTarget(cmd, target)
	if err != nil {
		return fmt.Errorf("box %s: %w", target, err)
	}
	ver, err := r.Run(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return fmt.Errorf("box %s is not usable: %w\n"+
			"       check the ssh target and that docker is installed and running there", target, err)
	}
	fmt.Fprintf(out, "    ✓ ssh ok — docker %s on %s\n", strings.TrimSpace(ver), r.Target())
	return nil
}

func scaffoldNow(ctx context.Context, cmd *cobra.Command, out io.Writer, host *canonical.Host, pin string, plan firstrun.Plan) {
	if plan.Brief == "" {
		return
	}
	agent := testHooks().SetupAgent
	if agent == nil {
		brain := harness.Lookup(plan.Setup.Harness, plan.Setup.Model)
		if brain == nil {
			fmt.Fprintf(out, "  setup: no agent CLI available — the brief is on main and every joined agent's\n"+
				"         contract carries it, so the first teammate with a harness can build it.\n")
			return
		}
		agent = conductor.HarnessScaffolder(brain)
	}
	fmt.Fprintf(out, "\n  setup (%s): working from %q…\n", plan.Setup.Harness, plan.Brief)
	client := controlClient(cmd)
	board := &conductor.StateBoard{}
	role := &conductor.Setup{
		Host: host, ID: sbGit.SessionIdentity("conductor", pin),
		Agent: agent, Harness: plan.Setup.Harness, Control: client, PIN: pin,
		States: board,
	}
	pubCtx, stopPublisher := context.WithCancel(ctx)
	publisher := &conductor.StatePublisher{
		Control: client, PIN: pin, Board: board,
		Record: conductor.SessionRecord(ctx, client, pin, agentsOption(plan)),
	}
	go publisher.Run(pubCtx)
	err := role.Tick(ctx)
	stopPublisher()
	if err != nil {
		fmt.Fprintf(out, "  setup failed: %v — the session is still up; ask an agent to build it\n", err)
		return
	}
	fmt.Fprintln(out, "  setup: committed to main")
}

func recordRunCommands(ctx context.Context, cmd *cobra.Command, out io.Writer, host *canonical.Host, pin string, plan firstrun.Plan) firstrun.Plan {
	if err := host.Refresh(ctx); err != nil {
		fmt.Fprintf(out, "warning: could not refresh canonical: %v\n", err)
	}
	_ = host.SyncWorkToMain(ctx)
	plan = firstrun.ResolveCommands(plan, host.Work)
	if len(plan.Install) == 0 && len(plan.Dev) == 0 {
		return plan
	}
	fmt.Fprintf(out, "  detected: install %q  dev %q\n",
		strings.Join(plan.Install, " "), strings.Join(plan.Dev, " "))
	if err := runfile.Commit(ctx, host, sbGit.SessionIdentity("host", pin),
		plan.Install, plan.Dev); err != nil {
		fmt.Fprintf(out, "warning: could not record %s (a future host will re-detect): %v\n", runfile.Path, err)
	}
	return plan
}

func standUpOnBox(ctx context.Context, cmd *cobra.Command, plan firstrun.Plan, once bool) error {
	out := cmd.OutOrStdout()
	var pin string
	opt, err := BoxAddOptions(cmd, "")
	if err != nil {
		return err
	}
	opt.ServeOnly, opt.Takeover = true, true
	opt.Dev, opt.Install = "", ""
	opt.Brief = plan.Brief

	client := controlClient(cmd)
	ann, lv := newAnnouncer(), &leaver{}
	defer func() { _ = lv.Leave(context.Background()) }()

	work := func(ctx context.Context, out io.Writer) error {
		hostname, _ := os.Hostname()
		claim, err := client.Claim(ctx, controlplane.ClaimRequest{
			HostMachine: hostname,
			MemberName:  clientName(), MemberRole: controlplane.RoleClient,
		})
		if err != nil {
			return fmt.Errorf("claim the session before asking for a box: %w", err)
		}
		pin = claim.Session.PIN
		opt.PIN = pin
		ann.Announce(console.MemberMsg{PIN: pin})
		b, err := provisionSessionBox(ctx, cmd, client, pin, plan, opt)
		if err != nil {
			_ = client.EndSession(context.Background(), pin)
			return err
		}
		fmt.Fprintf(out, "box provisioned on %s (container %s)\n\n", b.Target, b.Container)
		sess, err := client.Session(ctx, pin)
		if err != nil {
			return fmt.Errorf("read back the session the box just claimed: %w", err)
		}
		gitURL, err := client.Dialable(ctx, sess, b.GitURL)
		if err != nil {
			return fmt.Errorf("reach the box's canonical at %s: %w", b.GitURL, err)
		}
		if seedDir, _ := cmd.Flags().GetString("seed"); seedDir != "" {
			if err := canonical.SeedRemote(ctx, gitURL, seedDir, sbGit.SessionIdentity("host", pin)); err != nil {
				return fmt.Errorf("seed the box's canonical from %s: %w", seedDir, err)
			}
			fmt.Fprintf(out, "seeded canonical from %s\n", seedDir)
		}

		fmt.Fprintf(out, "session live. share with your team:\n\n    club join %s\n\n", pin)
		fmt.Fprintf(out, "git:     %s\ncontrol: %s\n\n", b.GitURL, client.Base)

		j, err := joinSelf(ctx, cmd, pin, out)
		if err != nil {
			return fmt.Errorf("join the session this laptop just created: %w", err)
		}
		lv.holds(j.Close)
		ann.Announce(console.MemberMsg{
			PIN: pin, MemberID: j.MemberID, Branch: j.Session.Branch, WorkPath: j.Paths.Work,
		})
		if err := electSelf(cmd, pin, plan); err != nil {
			return fmt.Errorf("elect this machine as the harness runner: %w", err)
		}

		remote, err := canonical.OpenRemote(ctx, filepath.Join(session.ForPin(pin).Root, "replica"), pin, gitURL)
		if err != nil {
			return fmt.Errorf("open the box's canonical: %w", err)
		}
		defer remote.Close(context.Background())
		scaffoldNow(ctx, cmd, out, remote, pin, plan)
		recordRunCommands(ctx, cmd, out, remote, pin, plan)
		if once {
			return nil
		}
		return conductorLoop(ctx, cmd, pin, b.GitURL)
	}
	if once {
		return work(ctx, out)
	}

	return runConsole(ctx, cmd, consoleSession{
		PIN: pin, Me: clientName(),
		Elector:  true,
		Leave:    lv.Leave,
		Work:     work,
		Announce: ann,
	})
}

func joinSelf(ctx context.Context, cmd *cobra.Command, pin string, out io.Writer) (*joindaemon.Joined, error) {
	j, err := joindaemon.Join(ctx, joindaemon.Options{
		PIN: pin, Name: clientName(), Control: controlClient(cmd),
		ConductsHere: true,
	})
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "you are %s — edit here: %s\n\n", j.Session.Branch, j.Paths.Work)
	return j, nil
}

func clientName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "agent"
}

func electSelf(cmd *cobra.Command, pin string, plan firstrun.Plan) error {
	p := session.ForPin(pin)
	name := os.Getenv("USER")
	if name == "" {
		name = "host"
	}
	primary := plan.Merger
	if primary.Harness == "" {
		primary = firstrun.Agent{Harness: "claude"}
	}
	b := trust.NewBroker(trust.RemoteBox)
	lease, err := b.Elect(trust.ElectRequest{Elector: name, Harness: primary.Harness, Model: primary.Model})
	if err != nil {
		_ = b.Close()
		return err
	}
	defer b.Close()
	_ = trust.SavePersist(p.Root, trust.PersistState{
		PIN: pin, Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID,
	})
	roles := map[string]controlplane.RoleAgent{}
	for role, a := range plan.Agents() {
		if a.Harness != "" {
			roles[role] = controlplane.RoleAgent{Harness: a.Harness, Model: a.Model}
		}
	}
	_ = controlClient(cmd).PutConductor(context.Background(), pin, controlplane.ConductorRecord{
		Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID, Roles: roles, Active: true,
	})
	fmt.Fprintf(cmd.OutOrStdout(), "elected %s — merger=%s  error-watcher=%s  setup=%s\n",
		lease.Elector, plan.Merger, plan.Watcher, plan.Setup)
	return nil
}

func startLocalDev(ctx context.Context, cmd *cobra.Command, running *hoststart.Running, plan firstrun.Plan) {
	if len(plan.Install) == 0 && len(plan.Dev) == 0 {
		return
	}
	if err := running.StartDev(ctx, plan.Install, plan.Dev); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
		return
	}
	if len(plan.Dev) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "dev: %q supervised here\n", strings.Join(plan.Dev, " "))
	}
}

func warnMirrorCredential(cmd *cobra.Command, plan firstrun.Plan) {
	if plan.MirrorURL == "" {
		return
	}
	if name, _ := firstrun.MirrorCredential(); name == "" {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: mirroring paused: no credential here (set GITHUB_TOKEN or GH_TOKEN and restart)\n")
	}
}

func seedOrURL(seedDir, seedURL string) string {
	if seedDir != "" {
		return seedDir
	}
	if seedURL != "" {
		return seedURL
	}
	return ""
}

func testBrains() map[string]*harness.Client {
	agent := testHooks().SetupAgent
	if agent == nil {
		return nil
	}
	return map[string]*harness.Client{
		"setup": {Name: "fake", Bin: "fake", AgentFn: func(ctx context.Context, dir, prompt string, _ io.Writer) error {
			return agent(ctx, dir, prompt, nil)
		}},
	}
}

func provisionSessionBox(ctx context.Context, cmd *cobra.Command, client *controlplane.Client,
	pin string, plan firstrun.Plan, opt box.Options) (*box.Box, error) {
	if plan.BoxTarget != "" {
		inv, err := client.Invite(ctx, pin, controlplane.MemberJoinRequest{
			Name: "box", Role: controlplane.RoleBox,
		})
		if err != nil {
			return nil, fmt.Errorf("invite the box: %w", err)
		}
		opt.MemberID, opt.MemberSecret = inv.MemberID, inv.Secret
		r, err := boxRunnerForTarget(cmd, plan.BoxTarget)
		if err != nil {
			return nil, err
		}
		b, err := box.Provision(ctx, r, opt)
		if err != nil {
			return nil, err
		}
		_ = client.PutBox(ctx, pin, controlplane.BoxFacts{
			Target: b.Target, Container: b.Container, Image: b.Image, Version: Version,
		})
		return b, nil
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "asking %s for a box…\n", client.Base)
	if _, err := client.RequestBox(ctx, pin, controlplane.BoxRequest{
		Brief: plan.Brief, SeedURL: opt.SeedURL,
	}); err != nil {
		return nil, fmt.Errorf("ask the control plane for a box: %w", err)
	}
	rec, err := client.WaitForBox(ctx, pin, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	return &box.Box{
		Target: rec.Target, Container: rec.Container, Image: rec.Image,
		JoinPIN: pin, GitURL: rec.GitURL, ControlURL: client.Base,
	}, nil
}
