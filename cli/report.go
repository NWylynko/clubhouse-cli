package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/nwylynko/clubhouse-cli/telemetry"
)

func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Upload everything about this session so somebody can look at why it broke",
		Args:  cobra.NoArgs,
		RunE:  runReport,
	}
	addPinFlag(cmd)
	cmd.Flags().Int("log-lines", 400, "how much of this machine's session log to include")
	return cmd
}

type reportSection struct {
	Name string
	Body string
	Err  string
}

func runReport(cmd *cobra.Command, _ []string) error {
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set CLUB_PIN")
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	client := controlClient(cmd)

	id, secret := session.ReadMembership(pin)
	if id == "" || secret == "" {
		return fmt.Errorf("this machine is not a member of %s, so it cannot report on it — run `club report` on a machine that joined the session", pin)
	}

	res, err := client.MemberSync(ctx, pin, id, controlplane.MemberSync{WantSnapshot: true})
	if err != nil {
		if errors.Is(err, controlplane.ErrNoSession) ||
			strings.Contains(err.Error(), "unknown or expired pin") ||
			strings.Contains(err.Error(), "unauthorized") {
			return fmt.Errorf("session %s is gone from the control plane, or this machine's membership of it is — "+
				"either way nothing can mint or verify a ticket for it any more, so there is nowhere to send a report. "+
				"Expired sessions take their members with them, so `club report` has to be run while the session is "+
				"still live (about 3 hours)", pin)
		}
		return fmt.Errorf("exchange this machine's membership for a telemetry ticket: %w", err)
	}
	ticket := res.RelayTickets[telemetry.TicketService]
	if ticket == "" {
		ticket = client.RelayTicket(pin, telemetry.TicketService)
	}
	if ticket == "" {
		return fmt.Errorf("the control plane minted no telemetry ticket for %s, so there is nothing to authenticate an upload with "+
			"(the deployment may be older than this binary)", pin)
	}

	sess := res.Session
	if sess == nil {
		s, err := client.Session(ctx, pin)
		if err != nil {
			return fmt.Errorf("read session %s: %w", pin, err)
		}
		sess = &s
	}
	ingest := strings.TrimSpace(sess.TelemetryURL)
	if ingest == "" {
		return fmt.Errorf("session %s advertises no telemetry ingest, so there is nowhere to send a report "+
			"(the control plane needs %s set)", pin, telemetry.AdvertiseEnv)
	}

	lines, _ := cmd.Flags().GetInt("log-lines")
	sections := gatherReport(ctx, client, pin, *sess, lines)

	reportID := newReportID()
	em := telemetry.New(telemetry.Config{
		URL: ingest, Bearer: ticket, Service: "client", Timeout: 30 * time.Second,
		Version: controlplane.ClientVersion,
	})
	for i, sec := range sections {
		body := sec.Body
		if sec.Err != "" {
			body = "could not gather this source: " + sec.Err
		}
		captured, truncated := telemetry.CaptureString(body)
		em.Emit("client.report", telemetry.Event{
			PIN: pin, SessionUID: sess.UID, Member: id, TraceID: reportID,
			Component: sec.Name, Body: captured, Truncated: truncated,
			Data: map[string]any{
				"section": sec.Name, "part": i + 1, "parts": len(sections),
				"gathered": sec.Err == "",
			},
		})
	}
	em.Close()

	fmt.Fprintf(out, "reported %s as %s (%d sections)\n", pin, reportID, len(sections))
	fmt.Fprintf(out, "quote that id — everything about this session is under it:\n")
	fmt.Fprintf(out, "  SELECT * FROM envelopes WHERE trace_id = '%s' ORDER BY id;\n", reportID)
	return nil
}

func newReportID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "rp_unknown"
	}
	return "rp_" + hex.EncodeToString(b[:])
}

func gatherReport(ctx context.Context, client *controlplane.Client,
	pin string, sess controlplane.Session, logLines int) []reportSection {
	var out []reportSection
	add := func(name, body string, err error) {
		s := reportSection{Name: name, Body: body}
		if err != nil {
			s.Err = err.Error()
		}
		out = append(out, s)
	}

	add("session", asJSON(sess), nil)

	events, err := client.EventsSince(ctx, pin, 0)
	add("events", asJSON(events), err)

	logsURL, lerr := client.EndpointURL(ctx, pin, controlplane.EndpointLogs)
	if lerr != nil {
		add("dev-logs", "", lerr)
	} else {
		body, err := httpGetText(ctx, logsURL)
		add("dev-logs", body, err)
	}

	add("git", gitTopology(pin), nil)

	if sess.Box != nil && strings.TrimSpace(sess.Box.Target) != "" {
		body, err := boxLogs(sess.Box.Target, pin)
		add("box", body, err)
	} else {
		add("box", "", fmt.Errorf("no BYO box target on this session — a managed box's container log lives on the control plane's docker host"))
	}

	tail, err := session.ReadClientLogTail(pin, logLines*200)
	add("client-log", tail, err)
	return out
}

func gitTopology(pin string) string {
	p := session.ForPin(pin)
	var b strings.Builder
	for _, repo := range []struct{ name, dir string }{
		{"canonical", p.Canonical}, {"mirror", p.Mirror}, {"work", p.Work},
	} {
		out, err := git.Output(context.Background(), repo.dir, "branch", "-vv", "--all")
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "--- %s (%s)\n%s\n", repo.name, repo.dir, out)
	}
	if b.Len() == 0 {
		return "(no local git repository for this session on this machine)"
	}
	return b.String()
}

func asJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("(could not encode: %v)", err)
	}
	return string(b)
}

func httpGetText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return string(b), err
}

func boxLogs(target, pin string) (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(bin, "box", "logs", target, "--pin", pin).CombinedOutput()
	return string(out), err
}
