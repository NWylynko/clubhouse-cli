package admission

import (
	"context"
	"fmt"
	"strings"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/logx"
)

func LogEvents(events []controlplane.Event) {
	if len(events) == 0 {
		return
	}
	log := logx.New("admission")
	for _, e := range events {
		switch e.Kind {
		case "member.knock":
			who := payload(e, "name")
			if m := payload(e, "machine"); m != "" {
				who += "@" + m
			}
			log.Infof("join request from %s — `club members accept %s`", who, payload(e, "id"))
		case "member.declined":
			log.Infof("join request from %s declined", payload(e, "name"))
		case "member.joined":
			if payload(e, "acceptedBy") == "" {
				continue
			}
			log.Infof("accepted %s into the session", payload(e, "name"))
		}
	}
}

func payload(e controlplane.Event, key string) string {
	s, _ := e.Payload[key].(string)
	return strings.TrimSpace(s)
}

func WaitLine(pin string, acceptors []string) string {
	who := strings.Join(acceptors, ", ")
	if who == "" {
		who = "nobody human is in the session yet"
	}
	return fmt.Sprintf("waiting to be let into %s — anyone already in the session can accept (%s)", pin, who)
}

type Log struct {
	Control *controlplane.Client
	PIN     string
	cursor  int64
}

func (l *Log) Stream(events []controlplane.Event) {
	l.advance(events)
	LogEvents(events)
}

func (l *Log) Poll(ctx context.Context) {
	if l == nil || l.Control == nil || l.PIN == "" {
		return
	}
	events, err := l.Control.EventsSince(ctx, l.PIN, l.cursor)
	if err != nil || len(events) == 0 {
		return
	}
	l.advance(events)
	LogEvents(events)
}

func (l *Log) advance(events []controlplane.Event) {
	for _, e := range events {
		if e.Seq > l.cursor {
			l.cursor = e.Seq
		}
	}
}
