package canonical

import (
	"fmt"
	"strings"

	"github.com/nwylynko/clubhouse-cli/sessionnet"
)

const gitDetailMax = 160

type RemoteCanonicalError struct {
	PIN      string
	Endpoint string
	Err      error
}

func ExplainRemoteOpenFailure(pin, endpoint string, err error) error {
	if err == nil {
		return nil
	}
	return &RemoteCanonicalError{PIN: pin, Endpoint: endpoint, Err: err}
}

func (e *RemoteCanonicalError) Unwrap() error { return e.Err }

func (e *RemoteCanonicalError) Error() string {
	where := e.Endpoint
	if where == "" {
		where = "the address the control plane published for it"
	}
	return fmt.Sprintf("can't reach session %s's git at %s: %s", e.PIN, where, e.cause())
}

func (e *RemoteCanonicalError) cause() string {
	join := fmt.Sprintf("`club join %s`", e.PIN)
	monitor := fmt.Sprintf("`club monitor --pin %s`", e.PIN)
	onSessionNet := sessionnet.IsSessionURL(e.Endpoint)

	if why := forwarderReason(e.Endpoint); onSessionNet && why != "" {
		switch {
		case strings.Contains(why, "holder"):
			return "nobody in the session is serving git right now. Run " + monitor +
				" to see whether a member is taking it over — git moves to another machine on its own once one is there to take it."
		case strings.Contains(why, "ticket"):
			return "the session relay refused this machine (" + why + "). Rejoin the session with " + join + "."
		default:
			return "the session relay would not carry this connection (" + why + "). Run " + monitor + " to see what the session says."
		}
	}

	detail := gitFatalLine(e.Err)
	if nothingListening(detail) {
		if onSessionNet {
			return "this machine has no live connection to the session — nothing is listening on its local forwarder. " +
				"Is the join daemon running here? Start it with " + join + "."
		}
		return "nothing is listening at that address, so whoever published it has stopped serving git. Run " +
			monitor + " to see who holds git now."
	}
	return detail + ". Is the join daemon running here (" + join + "), and does " + monitor + " show somebody serving git?"
}

func forwarderReason(endpoint string) string {
	pin, service, _, err := sessionnet.ParseURL(endpoint)
	if err != nil {
		return ""
	}
	ferr := sessionnet.LastForwardError(pin, service)
	if ferr == nil {
		return ""
	}
	return strings.TrimPrefix(flatten(ferr.Error()), "sessionnet: ")
}

func gitFatalLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	pick := flatten(lines[0])
	for _, l := range lines[1:] {
		if t := flatten(l); strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "error:") {
			pick = t
			break
		}
	}
	if len(pick) > gitDetailMax {
		pick = pick[:gitDetailMax] + "…"
	}
	return strings.TrimSuffix(pick, ".")
}

func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

func nothingListening(detail string) bool {
	for _, phrase := range []string{
		"Could not connect to server", "Couldn't connect to server",
		"Failed to connect", "Connection refused", "connection refused",
	} {
		if strings.Contains(detail, phrase) {
			return true
		}
	}
	return false
}
