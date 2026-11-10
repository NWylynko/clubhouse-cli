package console

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/devserver"
	"github.com/nwylynko/clubhouse-cli/logx"
)

type Feed struct {
	Control  *controlplane.Client
	PIN      string
	AwaitPIN <-chan string
	LogsURL  func(context.Context) (string, error)
	DevURL   func(context.Context) (string, error)
	Box      func(context.Context, controlplane.Session) (BoxMsg, error)
	Behind   func(context.Context) (int, error)
}

const Interval = 2 * time.Second

func Interactive(noConsole bool, out, in *os.File) bool {
	if noConsole {
		return false
	}
	return isCharDevice(out) && isCharDevice(in)
}

func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func Run(ctx context.Context, opt Options, feed Feed, work func(ctx context.Context, out io.Writer) error) error {
	m := New(opt)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))

	restore := divert(p)
	defer restore()

	go pump(ctx, p, feed)
	go forward(ctx, p, opt.Announce)
	failed := make(chan error, 1)
	if work != nil {
		go func() {
			if err := work(ctx, &sendWriter{send: p.Send, component: "club"}); err != nil {
				failed <- err
				p.Quit()
			}
		}()
	}

	_, err := p.Run()
	select {
	case werr := <-failed:
		return werr
	default:
	}
	return err
}

func forward(ctx context.Context, p *tea.Program, ch <-chan MemberMsg) {
	if ch == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			p.Send(msg)
		}
	}
}

type sendWriter struct {
	send      func(tea.Msg)
	component string
	buf       strings.Builder
}

func (w *sendWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	s := w.buf.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(s[:i]); line != "" {
			w.send(LogMsg{Component: w.component, Text: line})
		}
		s = s[i+1:]
	}
	w.buf.Reset()
	w.buf.WriteString(s)
	return len(b), nil
}

func divert(p *tea.Program) func() {
	return DivertTo(func(component, text string) {
		p.Send(LogMsg{Component: component, Text: text})
	})
}

func DivertTo(emit func(component, text string)) func() {
	prev := logx.SetOutput(&logSink{emit: emit})
	return func() { logx.SetOutput(prev) }
}

type logSink struct {
	emit func(component, text string)
	buf  strings.Builder
}

func (l *logSink) Write(b []byte) (int, error) {
	l.buf.Write(b)
	s := l.buf.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		l.line(s[:i])
		s = s[i+1:]
	}
	l.buf.Reset()
	l.buf.WriteString(s)
	return len(b), nil
}

func (l *logSink) line(text string) {
	fields := strings.Fields(text)
	if len(fields) < 4 {
		l.emit("club", text)
		return
	}
	_, rest, _ := strings.Cut(strings.TrimSpace(text), fields[2])
	l.emit(fields[2], strings.TrimSpace(rest))
}

func pump(ctx context.Context, p *tea.Program, feed Feed) {
	if feed.Control == nil {
		return
	}
	pin := feed.PIN
	if pin == "" {
		if feed.AwaitPIN == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case pin = <-feed.AwaitPIN:
			if pin == "" {
				return
			}
		}
	}
	var eventCursor int64
	tick := 0
	first := true
	t := time.NewTicker(Interval)
	defer t.Stop()

	updates := feed.Control.Updates(pin)

	if feed.LogsURL != nil {
		logs := devserver.Follow(ctx, devserver.FollowOptions{
			URL:   feed.LogsURL,
			Floor: controlplane.LogsFloor,
		}, func() { p.Send(LogsResetMsg{}) })
		defer logs.Close()
		go func() {
			for page := range logs.C {
				if page.Missed > 0 {
					logx.New("console").Warnf("dev-server log stream missed %d line(s)", page.Missed)
				}
				if len(page.Lines) > 0 {
					p.Send(LogsMsg(page.Lines))
				}
			}
		}()
	}
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		first = false
		tick++

		sess, err := feed.Control.Session(ctx, pin)
		p.Send(ErrorMsg{Source: "session", Err: err})
		wantPending := false
		if err == nil {
			p.Send(SessionMsg(sess))
			if tick == 1 {
				wantPending = true
			}
			if feed.Box != nil && tick%3 == 0 {
				if box, err := feed.Box(ctx, sess); err == nil {
					p.Send(box)
				}
			}
		}
		if feed.Behind != nil {
			n, err := feed.Behind(ctx)
			p.Send(ErrorMsg{Source: "behind", Err: err})
			if err == nil {
				p.Send(BehindMsg(n))
			}
		}
		if feed.Control.StreamLive(pin) {
			for drained := true; drained; {
				select {
				case u, ok := <-updates:
					if !ok {
						updates = nil
						drained = false
						continue
					}
					if len(u.Events) > 0 {
						if last := u.Events[len(u.Events)-1].Seq; last > eventCursor {
							eventCursor = last
						}
						p.Send(EventsMsg(u.Events))
						for _, e := range u.Events {
							switch e.Kind {
							case "member.knock", "member.declined", "member.joined", "member.left":
								wantPending = true
							}
						}
					}
				default:
					drained = false
				}
			}
		} else {
			wantPending = true
			if events, err := feed.Control.EventsSince(ctx, pin, eventCursor); err == nil && len(events) > 0 {
				eventCursor = events[len(events)-1].Seq
				p.Send(EventsMsg(events))
			}
		}
		if wantPending {
			if pending, perr := feed.Control.PendingMembers(ctx, pin); perr == nil {
				p.Send(PendingMsg(pending))
				p.Send(ErrorMsg{Source: "admission"})
			} else {
				p.Send(ErrorMsg{Source: "admission", Err: perr})
			}
		}
		if feed.DevURL != nil {
			if url, err := feed.DevURL(ctx); err == nil {
				p.Send(DevURLMsg(url))
			}
		}
		if feed.LogsURL != nil {
			_, err := feed.LogsURL(ctx)
			p.Send(ErrorMsg{Source: "logs", Err: err})
		}
	}
}
