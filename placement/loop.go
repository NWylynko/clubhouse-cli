package placement

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/logx"
)

var log = logx.New("placement")

type Loop struct {
	Control  *controlplane.Client
	PIN      string
	MemberID string
	Name     string
	Machine  string

	TTLSeconds int

	Start func(ctx context.Context, service string) error
	Stop  func(ctx context.Context, service string) error

	Serves func(service string) bool

	OnChange func(service, detail string)

	Outbox *controlplane.Outbox

	mu       sync.Mutex
	held     map[string]bool
	unplaced map[string]string
	failed   map[string]startFailure
}

type startFailure struct {
	fingerprint string
	reason      string
}

func (l *Loop) HoldsService(service string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held[service]
}

func (l *Loop) Unplaced() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.unplaced))
	for k, v := range l.unplaced {
		out[k] = v
	}
	return out
}

func (l *Loop) Tick(ctx context.Context) error {
	if l.Control == nil || l.PIN == "" || l.MemberID == "" {
		return nil
	}
	sess, err := l.Control.Session(ctx, l.PIN)
	if err != nil {
		return err
	}
	now := time.Now()
	var firstErr error
	for _, service := range controlplane.Services {
		if err := l.reconcile(ctx, sess, service, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Loop) reconcile(ctx context.Context, sess controlplane.Session, service string, now time.Time) error {
	if l.HoldsService(service) {
		return l.renew(ctx, service)
	}
	if l.Serves != nil && !l.Serves(service) {
		l.noteUnplaced(sess, service, now)
		return nil
	}
	mine := Holds(sess, service, l.MemberID, now)
	if !mine && !ShouldClaim(sess, service, l.MemberID, now) {
		l.noteUnplaced(sess, service, now)
		return nil
	}
	fp := StateFingerprint(sess)
	if reason, stuck := l.stillFailing(service, fp); stuck {
		log.Debugf("%s: not retrying here, nothing changed since %q", service, reason)
		if mine {
			l.reportStartFailure(ctx, service, reason)
		}
		return nil
	}
	if mine {
		return l.take(ctx, service, fp, "resuming a lease this machine still holds")
	}
	return l.take(ctx, service, fp, l.claimReason(sess, service, now))
}

func (l *Loop) stillFailing(service, fingerprint string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.failed[service]
	return f.reason, ok && f.fingerprint == fingerprint
}

func (l *Loop) noteStartFailure(service, fingerprint, reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed == nil {
		l.failed = map[string]startFailure{}
	}
	was := l.failed[service]
	l.failed[service] = startFailure{fingerprint: fingerprint, reason: reason}
	return was.reason != reason
}

func (l *Loop) forgetStartFailure(service string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failed, service)
}

func (l *Loop) reportStartFailure(ctx context.Context, service, reason string) {
	if err := l.Control.ReportStartFailure(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		StartFailReason: reason,
	}); err != nil {
		log.Debugf("reporting the %s start failure: %v", service, err)
	}
}

func oneLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	head := flattenLine(lines[0])
	why := ""
	for _, l := range lines[1:] {
		if t := flattenLine(l); strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "error:") {
			why = t
			break
		}
	}
	if why == "" {
		return clampLine(head, controlplane.StartFailReasonMax)
	}
	why = clampLine(why, controlplane.StartFailReasonMax)
	room := controlplane.StartFailReasonMax - len(why) - len(" — ")
	if room <= 0 {
		return why
	}
	return clampLine(head, room) + " — " + why
}

func flattenLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clampLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}

func (l *Loop) claimReason(sess controlplane.Session, service string, now time.Time) string {
	if prev, ok := sess.Leases[service]; ok && prev.Owner != "" && !prev.Live(now) {
		owner := prev.OwnerName
		if owner == "" {
			owner = prev.Owner
		}
		return "lease expired — " + owner + " went away"
	}
	return "unheld"
}

func (l *Loop) take(ctx context.Context, service, fingerprint, why string) error {
	if _, err := l.Control.ClaimLease(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		TTLSeconds: l.ttl(),
	}); err != nil {
		if errors.Is(err, controlplane.ErrLeaseHeld) {
			log.Debugf("%s: %v", service, err)
			return nil
		}
		return err
	}
	if l.Start != nil {
		if err := l.Start(ctx, service); err != nil {
			reason := oneLine(err)
			if l.noteStartFailure(service, fingerprint, reason) {
				log.Warnf("could not start %s here (%s) — handing the lease back so another member can", service, reason)
				l.changed(service, "cannot start here — "+reason)
			}
			l.reportStartFailure(ctx, service, reason)
			return err
		}
	}
	l.forgetStartFailure(service)
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]bool{}
	}
	l.held[service] = true
	delete(l.unplaced, service)
	l.mu.Unlock()
	log.Infof("took %s (%s)", service, why)
	l.changed(service, "now served here — "+why)
	return nil
}

func (l *Loop) renew(ctx context.Context, service string) error {
	if l.Outbox != nil {
		l.Outbox.Hold(service)
		return nil
	}
	_, err := l.Control.RenewLease(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		TTLSeconds: l.ttl(),
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, controlplane.ErrLeaseHeld):
		log.Infof("lost %s — %v; standing down", service, err)
		l.standDown(ctx, service)
		l.changed(service, "moved to another member")
		return nil
	default:
		return err
	}
}

func (l *Loop) ApplyLost(ctx context.Context, lost []string) {
	for _, service := range lost {
		if !l.HoldsService(service) {
			continue
		}
		log.Infof("lost %s — member sync; standing down", service)
		l.standDown(ctx, service)
		l.changed(service, "moved to another member")
	}
}

func (l *Loop) standDown(ctx context.Context, service string) {
	if l.Stop != nil {
		if err := l.Stop(ctx, service); err != nil {
			log.Warnf("stopping %s: %v", service, err)
		}
	}
	l.mu.Lock()
	delete(l.held, service)
	l.mu.Unlock()
}

func (l *Loop) noteUnplaced(sess controlplane.Session, service string, now time.Time) {
	if lease, ok := sess.Leases[service]; ok && lease.Live(now) {
		l.mu.Lock()
		delete(l.unplaced, service)
		l.mu.Unlock()
		return
	}
	r := Rank(sess, service)
	if _, ok := r.Best(); ok {
		return
	}
	l.mu.Lock()
	if l.unplaced == nil {
		l.unplaced = map[string]string{}
	}
	was := l.unplaced[service]
	l.unplaced[service] = r.Reason
	l.mu.Unlock()
	if was != r.Reason {
		log.Infof("%s unplaced: %s", service, r.Reason)
		l.changed(service, "unplaced — "+r.Reason)
	}
}

func (l *Loop) ReleaseAll(ctx context.Context) {
	l.mu.Lock()
	services := make([]string, 0, len(l.held))
	for s, ok := range l.held {
		if ok {
			services = append(services, s)
		}
	}
	l.mu.Unlock()
	for _, s := range services {
		if err := l.Control.ReleaseLease(ctx, l.PIN, s); err != nil {
			log.Debugf("release %s: %v", s, err)
		}
		l.mu.Lock()
		delete(l.held, s)
		l.mu.Unlock()
	}
}

func (l *Loop) changed(service, detail string) {
	if l.OnChange != nil {
		l.OnChange(service, detail)
	}
}

func (l *Loop) ttl() int {
	if l.TTLSeconds > 0 {
		return l.TTLSeconds
	}
	return controlplane.DefaultLeaseTTL
}

func (l *Loop) Adopt(ctx context.Context, service string) error {
	if l.Control == nil || l.MemberID == "" {
		return nil
	}
	if _, err := l.Control.ClaimLease(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		TTLSeconds: l.ttl(),
	}); err != nil {
		return err
	}
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]bool{}
	}
	l.held[service] = true
	l.mu.Unlock()
	return nil
}
