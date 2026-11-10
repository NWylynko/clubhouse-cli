package telemetry

import (
	"strings"
	"sync"

	"github.com/nwylynko/clubhouse-cli/logx"
)

const AdvertiseEnv = "CLUB_TELEMETRY_ADVERTISE"

var (
	ingestWarnMu   sync.Mutex
	ingestWarnedAt map[string]bool
)

func SessionIngest(pin, advertised string) string {
	url := strings.TrimSpace(advertised)
	if url != "" {
		return url
	}
	ingestWarnMu.Lock()
	if ingestWarnedAt == nil {
		ingestWarnedAt = map[string]bool{}
	}
	first := !ingestWarnedAt[pin]
	ingestWarnedAt[pin] = true
	ingestWarnMu.Unlock()
	if first {
		logx.New("telemetry").Warnf("%s: telemetry is on for this machine but the session advertises no ingest, so nothing is being recorded — the control plane needs %s set (see .env.example)",
			pin, AdvertiseEnv)
	}
	return ""
}

func ForgetIngestWarnings() {
	ingestWarnMu.Lock()
	ingestWarnedAt = nil
	ingestWarnMu.Unlock()
}
