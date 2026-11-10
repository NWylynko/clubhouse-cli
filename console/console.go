package console

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/devserver"
	"github.com/nwylynko/clubhouse-cli/reach"
)

type Options struct {
	PIN      string
	Me       string
	MemberID string
	Branch   string
	WorkPath string

	Elector bool

	Roles []string

	Quit func(context.Context) error

	Decide func(ctx context.Context, memberID, decision string) error

	SetAccess func(ctx context.Context, access string) error

	Announce <-chan MemberMsg
}

type (
	SessionMsg controlplane.Session
	EventsMsg  []controlplane.Event
	LogsMsg    []devserver.Line

	LogsResetMsg   struct{}
	AgentOutputMsg struct {
		Role string
		Text string
	}
	LogMsg struct {
		Component string
		Text      string
	}
	DevURLMsg string
	ErrorMsg  struct {
		Source string
		Err    error
	}
	BoxMsg struct {
		Git reach.Result
		Dev reach.Result
	}
	BehindMsg int
	MemberMsg struct {
		PIN      string
		MemberID string
		Branch   string
		WorkPath string
	}
	PendingMsg []controlplane.Member
)

type feedItem struct {
	when time.Time
	text string
}

const (
	tabDashboard = "dashboard"
	tabDev       = "dev"
	tabFeed      = "feed"
	tabUsers     = "users"
)

type rosterRow struct {
	ID, Name, Machine, State, Role string
}

var defaultRoles = []string{"merger", "error-watcher", "setup"}

type Model struct {
	opt   Options
	tabs  []string
	tab   int
	w, h  int
	ready bool

	sess     controlplane.Session
	events   []feedItem
	lastSeq  int64
	logs     []devserver.Line
	agent    map[string][]string
	errs     map[string]error
	errOrder []string
	confirm  bool
	quitting bool

	behind   int
	boxReach BoxMsg

	seen bool

	roster     []rosterRow
	selectedID string

	devURL string
}

func New(opt Options) *Model {
	roles := opt.Roles
	if len(roles) == 0 {
		roles = defaultRoles
	}
	tabs := append([]string{tabDashboard, tabDev, tabFeed, tabUsers}, roles...)
	return &Model{opt: opt, tabs: tabs, agent: map[string][]string{},
		errs: map[string]error{}, w: 100, h: 30}
}

func (m *Model) Quitting() bool { return m.quitting }

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h, m.ready = msg.Width, msg.Height, true
	case SessionMsg:
		m.applySession(controlplane.Session(msg))
	case EventsMsg:
		m.applyEvents(msg)
	case DevURLMsg:
		m.devURL = string(msg)
	case LogsMsg:
		m.applyLogLines(msg)
	case LogsResetMsg:
		m.logs = nil
	case AgentOutputMsg:
		m.agent[msg.Role] = appendCapped(m.agent[msg.Role],
			strings.Split(strings.TrimSuffix(msg.Text, "\n"), "\n"), 2000)
	case LogMsg:
		m.applyLog(msg)
	case BoxMsg:
		m.boxReach = msg
	case BehindMsg:
		m.behind = int(msg)
	case MemberMsg:
		m.applyMember(msg)
	case PendingMsg:
		m.applyPending(msg)
	case ErrorMsg:
		m.applyError(msg)
	case leaveFailedMsg:
		m.applyError(ErrorMsg{Source: "leave", Err: msg.err})
		return m, tea.Quit
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	s := msg.String()
	if m.confirm {
		switch s {
		case "y", "Y", "enter":
			m.confirm, m.quitting = false, true
			return m.leave()
		default:
			m.confirm = false
		}
		return nil
	}
	switch s {
	case "q", "ctrl+c", "esc":
		m.confirm = true
	case "tab", "right", "l":
		m.tab = (m.tab + 1) % len(m.tabs)
	case "shift+tab", "left", "h":
		m.tab = (m.tab - 1 + len(m.tabs)) % len(m.tabs)
	default:
		if m.onUsers() {
			if cmd := m.usersKey(s); cmd != nil {
				return cmd
			}
		}
		if n := int(s[0]) - '1'; len(s) == 1 && n >= 0 && n < len(m.tabs) {
			m.tab = n
		}
	}
	return nil
}

func (m *Model) onUsers() bool {
	return m.tab >= 0 && m.tab < len(m.tabs) && m.tabs[m.tab] == tabUsers
}

func (m *Model) usersKey(s string) tea.Cmd {
	switch s {
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "a":
		return m.decide(controlplane.DecisionAccept)
	case "d":
		return m.decide(controlplane.DecisionDecline)
	case "c":
		return m.flipAccess()
	}
	return nil
}

func (m *Model) decide(decision string) tea.Cmd {
	id := m.selectedID
	if id == "" || m.opt.Decide == nil {
		return nil
	}
	fn := m.opt.Decide
	return func() tea.Msg {
		err := fn(context.Background(), id, decision)
		return ErrorMsg{Source: "admission", Err: err}
	}
}

func (m *Model) flipAccess() tea.Cmd {
	if m.opt.SetAccess == nil {
		return nil
	}
	cur := m.sess.Access
	if cur == "" {
		cur = controlplane.AccessOpen
	}
	next := controlplane.AccessClosed
	if cur == controlplane.AccessClosed {
		next = controlplane.AccessOpen
	}
	fn := m.opt.SetAccess
	return func() tea.Msg {
		err := fn(context.Background(), next)
		return ErrorMsg{Source: "admission", Err: err}
	}
}

func (m *Model) leave() tea.Cmd {
	quit := m.opt.Quit
	if quit == nil {
		return tea.Quit
	}
	return func() tea.Msg {
		if err := quit(context.Background()); err != nil {
			return leaveFailedMsg{err: err}
		}
		return tea.QuitMsg{}
	}
}

type leaveFailedMsg struct{ err error }

func (m *Model) applyError(msg ErrorMsg) {
	if !m.seen && errors.Is(msg.Err, controlplane.ErrNoSession) {
		msg.Err = nil
	}
	if _, known := m.errs[msg.Source]; !known {
		m.errOrder = append(m.errOrder, msg.Source)
	}
	m.errs[msg.Source] = msg.Err
}

func (m *Model) applySession(s controlplane.Session) {
	m.sess = s
	m.seen = true
	m.mergeAdmitted(s.Members)
}

func (m *Model) applyPending(list []controlplane.Member) {
	m.mergePending(list)
}

func (m *Model) applyMember(msg MemberMsg) {
	if msg.PIN != "" {
		m.opt.PIN = msg.PIN
	}
	if msg.MemberID != "" {
		m.opt.MemberID = msg.MemberID
	}
	if msg.Branch != "" {
		m.opt.Branch = msg.Branch
	}
	if msg.WorkPath != "" {
		m.opt.WorkPath = msg.WorkPath
	}
}

func (m *Model) applyEvents(events []controlplane.Event) {
	for _, e := range events {
		if e.Seq <= m.lastSeq {
			continue
		}
		m.lastSeq = e.Seq
		m.events = appendCapped(m.events, []feedItem{{when: e.CreatedAt.Local(), text: describe(e)}}, 500)
	}
}

func (m *Model) applyLog(msg LogMsg) {
	role := msg.Component
	if role == "watcher" {
		role = "error-watcher"
	}
	for _, tab := range m.roleTabs() {
		if tab == role {
			m.agent[role] = appendCapped(m.agent[role], []string{msg.Text}, 2000)
			return
		}
	}
	m.events = appendCapped(m.events, []feedItem{{when: time.Now(), text: msg.Component + ": " + msg.Text}}, 500)
}

func (m *Model) myMember() (controlplane.Member, bool) {
	for _, mem := range m.sess.Members {
		if m.opt.MemberID != "" && mem.ID == m.opt.MemberID {
			return mem, true
		}
	}
	for _, mem := range m.sess.Members {
		if mem.Name == m.opt.Me {
			return mem, true
		}
	}
	return controlplane.Member{Name: m.opt.Me}, false
}

func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(m.tabBar() + "\n")
	switch tab := m.tabs[m.tab]; tab {
	case tabDashboard:
		b.WriteString(m.dashboard())
	case tabDev:
		b.WriteString(m.logView())
	case tabFeed:
		b.WriteString(m.feed())
	case tabUsers:
		b.WriteString(m.users())
	default:
		b.WriteString(m.agentView(tab))
	}
	for _, source := range m.errOrder {
		if err := m.errs[source]; err != nil {
			b.WriteString("\n" + styleWarn.Render("! "+err.Error()))
		}
	}
	if m.confirm {
		b.WriteString("\n\n" + m.confirmBanner())
	} else {
		b.WriteString("\n\n" + styleDim.Render("tab/1-9 switch · q leave the session"))
		if m.onUsers() {
			b.WriteString("\n" + styleDim.Render("users: ↑↓ select · a accept · d decline · c access"))
		}
	}
	return b.String()
}

func (m *Model) tabBar() string {
	var parts []string
	pending := m.pendingCount()
	for i, t := range m.tabs {
		label := t
		if t == tabUsers && pending > 0 {
			label = fmt.Sprintf("users (%d)", pending)
		}
		text := fmt.Sprintf(" %d %s ", i+1, label)
		if i == m.tab {
			parts = append(parts, styleTabOn.Render(text))
			continue
		}
		parts = append(parts, styleTabOff.Render(text))
	}
	return strings.Join(parts, "")
}

func (m *Model) dashboard() string {
	var b strings.Builder
	join := "minting session…"
	if m.opt.PIN != "" {
		join = "club join " + m.opt.PIN
	}
	fmt.Fprintf(&b, "\n  %s\n\n", stylePIN.Render(join))

	if !m.seen {
		fmt.Fprintf(&b, "  %s\n", styleDim.Render("session   standing up…"))
	} else {
		online, names := m.onlineMembers()
		line := fmt.Sprintf("  members   %d online   %s", len(online), strings.Join(names, ", "))
		if n := m.pendingCount(); n > 0 {
			line += fmt.Sprintf(" · %d join request(s)", n)
		}
		fmt.Fprintf(&b, "%s\n", line)

		if line := m.boxLine(); line != "" {
			b.WriteString(line + "\n")
		}

		for _, line := range m.startFailureLines() {
			b.WriteString("  " + styleWarn.Render("! "+line) + "\n")
		}

		if m.behind > 0 {
			fmt.Fprintf(&b, "  main      %d commit(s) behind main — run sync to take them\n", m.behind)
		} else {
			fmt.Fprintf(&b, "  main      level with main\n")
		}
	}
	if m.opt.Branch != "" {
		fmt.Fprintf(&b, "  branch    %s\n", m.opt.Branch)
	}
	if m.opt.WorkPath != "" {
		fmt.Fprintf(&b, "  workspace %s\n", m.opt.WorkPath)
		fmt.Fprintf(&b, "            club open — new terminal\n")
	}
	if url := m.devURL; url != "" {
		fmt.Fprintf(&b, "  site      %s\n", url)
	} else {
		fmt.Fprintf(&b, "  site      no dev server up yet\n")
	}

	b.WriteString("\n")
	for _, role := range m.roleNames() {
		b.WriteString("  " + m.roleLine(role) + "\n")
	}
	return b.String()
}

func (m *Model) startFailureLines() []string {
	var out []string
	for _, svc := range controlplane.Services {
		if f := m.sess.Leases[svc].StartFailure; f != nil {
			out = append(out, f.Line(svc))
		}
	}
	return out
}

func (m *Model) roleLine(role string) string {
	r, ok := m.roles()[role]
	label := fmt.Sprintf("%-14s", role)
	harness := roleHarnessLabel(r, ok)
	switch {
	case !ok && m.fleetComingUp():
		return styleDim.Render(label + harnessLabelPad(harness) + "○ starting")
	case !ok:
		return styleDim.Render(label + harnessLabelPad(harness) + "○ not running")
	case r.UpdatedAt.IsZero():
		return styleDim.Render(label + harnessLabelPad(harness) + "○ starting")
	case r.Stale(time.Now()):
		return styleDim.Render(fmt.Sprintf("%s%s○ stale — nothing published for %s", label, harnessLabelPad(harness), since(r.UpdatedAt)))
	case r.Working(time.Now()):
		return fmt.Sprintf("%s%s%s %s %s", label, harnessLabelPad(harness), styleWorking.Render("●"), r.Activity, styleDim.Render("("+since(r.Since)+")"))
	default:
		return fmt.Sprintf("%s%s%s idle", label, harnessLabelPad(harness), styleIdle.Render("○"))
	}
}

func (m *Model) fleetComingUp() bool {
	if !m.seen {
		return true
	}
	if _, joined := m.myMember(); !joined {
		return true
	}
	return m.sess.Leases[controlplane.ServiceConductor].Live(time.Now())
}

func roleHarnessLabel(r controlplane.RoleAgent, ok bool) string {
	if !ok {
		return ""
	}
	var s string
	if r.Harness != "" {
		s = r.Harness
		if r.Model != "" {
			s += "/" + r.Model
		}
	}
	if r.Mechanical {
		if s != "" {
			s += " (mechanical here)"
		} else {
			s = "(mechanical here)"
		}
	}
	return s
}

func harnessLabelPad(harness string) string {
	if harness == "" {
		return ""
	}
	if len(harness) >= 18 {
		return harness + " "
	}
	return fmt.Sprintf("%-18s", harness)
}

func (m *Model) boxLine() string {
	if m.sess.Box == nil {
		return ""
	}
	box := *m.sess.Box
	if box.Pending() {
		return "  box       provisioning…"
	}
	if box.State == controlplane.BoxFailed {
		err := strings.TrimSpace(box.Error)
		if i := strings.Index(err, "\n"); i >= 0 {
			err = err[:i]
		}
		if err == "" {
			err = "provisioning failed"
		}
		return "  box       failed — " + err
	}
	prov := box.Provider
	if prov == "" {
		prov = "box"
	}
	parts := []string{"ready", prov}
	for _, svc := range []struct {
		name string
		r    reach.Result
	}{{"git", m.boxReach.Git}, {"dev", m.boxReach.Dev}} {
		switch {
		case !svc.r.Published:
			continue
		case svc.r.Reachable:
			parts = append(parts, svc.name+" ok")
		default:
			parts = append(parts, svc.name+" unreachable from here")
		}
	}
	if len(parts) == 2 {
		parts = append(parts, "services coming up…")
	}
	if via := pathKind(m.boxReach.Git.Via); via != "" {
		parts = append(parts, "via "+via)
	}
	return "  box       " + strings.Join(parts, " · ")
}

func pathKind(path string) string {
	kind, _, _ := strings.Cut(path, " ")
	return kind
}

func (m *Model) feed() string {
	if len(m.events) == 0 {
		return "\n  nothing has happened yet.\n"
	}
	var b strings.Builder
	b.WriteString("\n")
	start := 0
	if n := m.h - 8; n > 0 && len(m.events) > n {
		start = len(m.events) - n
	}
	for _, it := range m.events[start:] {
		fmt.Fprintf(&b, "  %s  %s\n", styleDim.Render(it.when.Format("15:04:05")), it.text)
	}
	return b.String()
}

func describe(e controlplane.Event) string {
	switch e.Kind {
	case "member.joined":
		return memberLabel(e) + " joined"
	case "member.left":
		return memberLabel(e) + " left"
	case "member.knock":
		who := payloadString(e, "name")
		if mach := payloadString(e, "machine"); mach != "" {
			who += "@" + mach
		}
		return who + " — join request"
	case "member.declined":
		return payloadString(e, "name") + " — join request declined"
	case controlplane.EventSyncPushed:
		return fmt.Sprintf("%s synced %s — %s",
			payloadString(e, "member"), payloadString(e, "branch"), payloadString(e, "intent"))
	case controlplane.EventMergeApplied:
		line := fmt.Sprintf("merge landed from %s — %s",
			payloadString(e, "branch"), payloadString(e, "intent"))
		if n := payloadInt(e, "conflicts"); n > 0 {
			line += fmt.Sprintf(" (%d conflict(s) resolved)", n)
		}
		return line
	case "error.detected":
		return styleWarn.Render("dev server error detected")
	case "error.resolved":
		return "dev server error resolved"
	case "main.advanced":
		return "main advanced"
	case "endpoint.changed":
		return fmt.Sprintf("%s endpoint moved to %s", payloadString(e, "kind"), payloadString(e, "url"))
	case "conductor.elected":
		line := payloadString(e, "elector") + " is conducting"
		if h := payloadString(e, "harness"); h != "" {
			line += " with " + h
		}
		return line
	case "role.working":
		line := payloadString(e, "role") + " working"
		if a := payloadString(e, "activity"); a != "" {
			line += " — " + a
		}
		return line
	case "role.idle":
		return payloadString(e, "role") + " idle"
	case "session.access":
		return "the session is " + payloadString(e, "access")
	case "host.cutover":
		line := "hosting moved"
		if from := payloadString(e, "from"); from != "" {
			line += " from " + from
		}
		if gen := payloadInt(e, "generation"); gen > 0 {
			line += fmt.Sprintf(" (generation %d)", gen)
		}
		return line
	case "endpoint.cleared":
		return payloadString(e, "kind") + " endpoint cleared"
	case "lease.claimed":
		return leaseOwnerLabel(e) + " took " + payloadString(e, "service")
	case "lease.released":
		if who := payloadString(e, "owner"); who != "" {
			return who + " released " + payloadString(e, "service")
		}
		return payloadString(e, "service") + " released"
	case "lease.handover":
		return payloadString(e, "service") + " handed to " + payloadString(e, "to")
	case controlplane.EventBoxRequested:
		line := "provisioning the session's box"
		if b, _ := e.Payload["restart"].(bool); b {
			line = "starting the session's box again"
		}
		if prov := payloadString(e, "provider"); prov != "" {
			line += " (" + prov + ")"
		}
		return line
	case controlplane.EventBoxFailed:
		return styleWarn.Render(withReason("the session's box could not start", e))
	case controlplane.EventBoxBootFailed:
		return styleWarn.Render(withReason("the session's box could not boot", e))
	case controlplane.EventPlacementFailed:
		who := payloadString(e, "member")
		if machine := payloadString(e, "machine"); machine != "" {
			who += "@" + machine
		}
		return styleWarn.Render(fmt.Sprintf("%s: %s can't start it — %s",
			payloadString(e, "service"), who, payloadString(e, "reason")))
	default:
		return e.Kind
	}
}

func memberLabel(e controlplane.Event) string {
	if name := payloadString(e, "name"); name != "" {
		return name
	}
	if id := payloadString(e, "id"); id != "" {
		return id
	}
	return "a member"
}

func leaseOwnerLabel(e controlplane.Event) string {
	if name := payloadString(e, "owner"); name != "" {
		return name
	}
	if id := payloadString(e, "memberId"); id != "" {
		return id
	}
	return "somebody"
}

func withReason(line string, e controlplane.Event) string {
	if reason := payloadString(e, "reason"); reason != "" {
		return line + " — " + reason
	}
	return line
}

func (m *Model) logView() string {
	if len(m.logs) == 0 {
		return "\n  no dev-server output yet.\n"
	}
	var b strings.Builder
	b.WriteString("\n")
	start := 0
	if n := m.h - 8; n > 0 && len(m.logs) > n {
		start = len(m.logs) - n
	}
	for _, l := range m.logs[start:] {
		if l.Stream == devserver.StreamStderr {
			fmt.Fprintf(&b, "  %s\n", styleWarn.Render(l.Text))
			continue
		}
		fmt.Fprintf(&b, "  %s\n", l.Text)
	}
	return b.String()
}

func (m *Model) agentView(role string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %s\n\n", m.roleLine(role))
	if m.opt.Elector {
		lines := m.agent[role]
		if len(lines) == 0 {
			b.WriteString("  waiting for " + role + " to do something.\n")
			return b.String()
		}
		start := 0
		if n := m.h - 10; n > 0 && len(lines) > n {
			start = len(lines) - n
		}
		for _, l := range lines[start:] {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		return b.String()
	}
	elector := "the elector"
	if m.sess.Conductor != nil && m.sess.Conductor.Elector != "" {
		elector = m.sess.Conductor.Elector
	}
	fmt.Fprintf(&b, "  the full %s stream is on %s's machine — it runs under that\n"+
		"  harness login and stays there. What travels is the state above.\n", role, elector)
	return b.String()
}

func (m *Model) confirmBanner() string {
	online, _ := m.onlineMembers()
	what := fmt.Sprintf("leave session %s", m.opt.PIN)
	if m.opt.Elector {
		what = fmt.Sprintf("leave session %s and hand off what this machine is running", m.opt.PIN)
	}
	rest := ""
	if others := len(online) - 1; others > 0 {
		rest = fmt.Sprintf(" %d other member(s) stay in the session.", others)
	}
	return styleWarn.Render(fmt.Sprintf("  %s?%s  [y/N]", what, rest))
}

func (m *Model) onlineMembers() (out []controlplane.Member, names []string) {
	for _, mem := range m.sess.Members {
		if mem.Role == controlplane.RoleBox {
			continue
		}
		if mem.State == controlplane.MemberLeft || mem.State == controlplane.MemberPending {
			continue
		}
		if mem.Online {
			out = append(out, mem)
			names = append(names, mem.Name)
		}
	}
	sort.Strings(names)
	return out, names
}

func (m *Model) roleTabs() []string {
	for i, t := range m.tabs {
		if t == tabUsers {
			if i+1 < len(m.tabs) {
				return m.tabs[i+1:]
			}
			return nil
		}
	}
	return nil
}

func (m *Model) roles() map[string]controlplane.RoleAgent {
	if m.sess.Conductor == nil {
		return nil
	}
	return m.sess.Conductor.Roles
}

func (m *Model) roleNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range m.roleTabs() {
		seen[t] = true
		out = append(out, t)
	}
	var extra []string
	for role := range m.roles() {
		if !seen[role] {
			extra = append(extra, role)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// raw endpoint ok: rendered on the dashboard for a human to read and open.
func (m *Model) endpoint(kind string) string {
	if ep, ok := m.sess.Endpoints[kind]; ok {
		return ep.URL
	}
	return ""
}

func payloadString(e controlplane.Event, key string) string {
	s, _ := e.Payload[key].(string)
	return s
}

func payloadInt(e controlplane.Event, key string) int {
	switch v := e.Payload[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func since(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

func appendCapped[T any](dst []T, add []T, max int) []T {
	dst = append(dst, add...)
	if len(dst) > max {
		dst = dst[len(dst)-max:]
	}
	return dst
}

var (
	stylePIN     = lipgloss.NewStyle().Bold(true)
	styleTabOn   = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleTabOff  = lipgloss.NewStyle().Faint(true)
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleIdle    = lipgloss.NewStyle().Faint(true)
)

func (m *Model) applyLogLines(lines []devserver.Line) {
	for _, l := range lines {
		replaced := false
		for i := len(m.logs) - 1; i >= 0 && i >= len(m.logs)-64; i-- {
			if m.logs[i].Seq == l.Seq && l.Seq != 0 {
				m.logs[i] = l
				replaced = true
				break
			}
		}
		if !replaced {
			m.logs = appendCapped(m.logs, []devserver.Line{l}, 2000)
		}
	}
}
