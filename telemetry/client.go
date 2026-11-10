package telemetry

import (
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/session"
)

const (
	eventClientLog     = "client.log"
	eventClientHarness = "client.harness"
)

var (
	memberMu     sync.Mutex
	memberCfg    MemberConfig
	memberEm     *Emitter
	memberUnhook func()

	memberLog       *session.ClientLog
	memberLogUnhook func()
)

func UseMember(cfg MemberConfig) {
	if memberIs(cfg) {
		return
	}
	em := ForMember(cfg)
	previous, noLocalLog := installMember(cfg, em)
	previous.Close()
	if noLocalLog != nil {
		logx.New("telemetry").Warnf("%s: no session log on this machine (%v) — `club report` will have nothing local to send", cfg.PIN, noLocalLog)
	}
}

func memberIs(cfg MemberConfig) bool {
	memberMu.Lock()
	defer memberMu.Unlock()
	return memberUnhook != nil && cfg == memberCfg
}

func installMember(cfg MemberConfig, em *Emitter) (previous *Emitter, noLocalLog error) {
	memberMu.Lock()
	defer memberMu.Unlock()
	if memberUnhook != nil && cfg == memberCfg {
		return em, nil
	}
	previous, memberEm = memberEm, em
	memberCfg = cfg
	if memberUnhook == nil {
		memberUnhook = logx.AddSink(logSink)
	}
	if memberLog == nil && cfg.PIN != "" {
		lf, err := session.OpenClientLog(cfg.PIN)
		if err != nil {
			return previous, err
		}
		memberLog = lf
		memberLogUnhook = logx.AddSink(func(ts time.Time, level, component, message string) {
			lf.Write(ts, level, component, message)
		})
	}
	return previous, nil
}

func StopMember() {
	memberMu.Lock()
	em := memberEm
	memberEm = nil
	memberCfg = MemberConfig{}
	if memberUnhook != nil {
		memberUnhook()
		memberUnhook = nil
	}
	if memberLogUnhook != nil {
		memberLogUnhook()
		memberLogUnhook = nil
	}
	lf := memberLog
	memberLog = nil
	memberMu.Unlock()
	_ = lf.Close()
	em.Close()
}

func member() (*Emitter, MemberConfig) {
	memberMu.Lock()
	defer memberMu.Unlock()
	return memberEm, memberCfg
}

func logSink(ts time.Time, level, component, message string) {
	em, cfg := member()
	if !em.Enabled() {
		return
	}
	body, truncated := CaptureString(message)
	em.Emit(eventClientLog, Event{
		TS: ts.UTC(), PIN: cfg.PIN, SessionUID: cfg.SessionUID, Member: cfg.MemberID,
		Level: level, Component: component,
		Body: body, Truncated: truncated,
	})
}

func EmitHarnessEvent(kind, tool, text, activity string) {
	em, cfg := member()
	if !em.Enabled() {
		return
	}
	body, truncated := CaptureString(strings.TrimSpace(text))
	em.Emit(eventClientHarness, Event{
		PIN: cfg.PIN, SessionUID: cfg.SessionUID, Member: cfg.MemberID,
		Component: "harness",
		Body:      body, Truncated: truncated,
		Data: map[string]any{"kind": kind, "tool": tool, "activity": activity},
	})
}
