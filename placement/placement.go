package placement

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
)

type Result struct {
	Service  string
	Eligible []controlplane.Member
	Reason   string
}

func (r Result) Best() (controlplane.Member, bool) {
	if len(r.Eligible) == 0 {
		return controlplane.Member{}, false
	}
	return r.Eligible[0], true
}

func Rank(sess controlplane.Session, service string) Result {
	res := Result{Service: service}
	var skipped []string
	for _, m := range sess.Members {
		if m.State != "" && m.State != controlplane.MemberAdmitted {
			continue
		}
		if !m.Online {
			continue
		}
		if ok, why := eligible(sess, m, service); ok {
			res.Eligible = append(res.Eligible, m)
		} else if why != "" {
			skipped = append(skipped, m.Name+" ("+why+")")
		}
	}
	sort.SliceStable(res.Eligible, func(i, j int) bool {
		return less(res.Eligible[i], res.Eligible[j], service)
	})
	if len(res.Eligible) == 0 {
		res.Reason = unplacedReason(sess, service, skipped)
	}
	return res
}

func eligible(sess controlplane.Session, m controlplane.Member, service string) (bool, string) {
	switch service {
	case controlplane.ServiceGit:
		return true, ""
	case controlplane.ServiceDev:
		want := stackRuntime(sess)
		if want == "" {
			return true, ""
		}
		if m.Capability == nil || !strings.EqualFold(m.Capability.Runtime, want) {
			return false, "no " + want
		}
		return true, ""
	case controlplane.ServiceConductor:
		if m.Role == controlplane.RoleBox {
			return false, "box (harness login never lands on a box)"
		}
		if m.Harness == "" {
			return false, "no harness CLI"
		}
		return true, ""
	default:
		return false, "unknown service"
	}
}

func less(a, b controlplane.Member, service string) bool {
	if service != controlplane.ServiceConductor {
		if ab, bb := a.Role == controlplane.RoleBox, b.Role == controlplane.RoleBox; ab != bb {
			return ab
		}
	}
	if service == controlplane.ServiceGit {
		if a.MainMirrorHeight != b.MainMirrorHeight {
			return a.MainMirrorHeight > b.MainMirrorHeight
		}
	}
	if as, bs := score(a), score(b); as != bs {
		return as > bs
	}
	return a.ID < b.ID
}

func score(m controlplane.Member) int {
	if m.Capability == nil {
		return 0
	}
	return m.Capability.Score
}

func stackRuntime(sess controlplane.Session) string {
	if sess.Stack == nil {
		return ""
	}
	return sess.Stack.Runtime
}

func unplacedReason(sess controlplane.Session, service string, skipped []string) string {
	switch service {
	case controlplane.ServiceDev:
		want := stackRuntime(sess)
		if want == "" {
			want = "a runtime"
		}
		return fmt.Sprintf("no online member has %s%s", want, detail(skipped))
	case controlplane.ServiceConductor:
		return "no online member has a harness CLI logged in" + detail(skipped)
	default:
		return "no online members" + detail(skipped)
	}
}

func detail(skipped []string) string {
	if len(skipped) == 0 {
		return ""
	}
	return " — " + strings.Join(skipped, ", ")
}

func Holds(sess controlplane.Session, service, memberID string, now time.Time) bool {
	l, ok := sess.Leases[service]
	return ok && l.Owner == memberID && l.Live(now)
}

func ShouldClaim(sess controlplane.Session, service, memberID string, now time.Time) bool {
	if l, ok := sess.Leases[service]; ok && l.Live(now) {
		return false
	}
	best, ok := Rank(sess, service).Best()
	return ok && best.ID == memberID
}

func StateFingerprint(sess controlplane.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gen=%d;", sess.Generation)
	kinds := make([]string, 0, len(sess.Endpoints))
	for k := range sess.Endpoints {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(&b, "ep:%s=%s;", k, sess.Endpoints[k].URL)
	}
	for _, svc := range controlplane.Services {
		fmt.Fprintf(&b, "lease:%s=%s;", svc, sess.Leases[svc].Owner)
	}
	if sess.Convergence != nil {
		fmt.Fprintf(&b, "main=%s;", sess.Convergence.MainSHA)
	}
	roster := make([]string, 0, len(sess.Members))
	for _, m := range sess.Members {
		roster = append(roster, m.ID+":"+m.State)
	}
	sort.Strings(roster)
	b.WriteString("members=" + strings.Join(roster, ","))
	return b.String()
}

func Describe(sess controlplane.Session, service string, now time.Time) string {
	l, ok := sess.Leases[service]
	if ok && l.Live(now) {
		owner := l.OwnerName
		if owner == "" {
			owner = l.Owner
		}
		where := ""
		if l.Machine != "" {
			where = " (" + l.Machine + ")"
		}
		return fmt.Sprintf("%s%s  %ds left", owner, where, int(time.Until(l.ExpiresAt).Seconds()))
	}
	if ok && l.StartFailure != nil {
		return l.StartFailure.Line(service)
	}
	r := Rank(sess, service)
	if best, ok := r.Best(); ok {
		return fmt.Sprintf("unheld — %s next", best.Name)
	}
	return "unplaced — " + r.Reason
}
