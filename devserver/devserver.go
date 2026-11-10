package devserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
	StreamClub   Stream = "club"
)

type Phase string

const (
	PhaseInstall Phase = "install"
	PhaseDev     Phase = "dev"
)

type tag struct {
	stream Stream
	phase  Phase
}

type record struct {
	tag  tag
	text string
	open bool
}

const logSubscriberQueue = 8

var TestSubscribeQueueSize int

type LogBuffer struct {
	mu   sync.Mutex
	recs []record
	open map[tag]int
	base int64
	subs []*logSubscriber
}

type logSubscriber struct {
	ch     chan LogPage
	cursor int64
	stream Stream
	phase  Phase
	missed int64
}

type LogSubscriber struct {
	C     <-chan LogPage
	close func()
}

func (s *LogSubscriber) Close() {
	if s != nil && s.close != nil {
		s.close()
	}
}

func (l *LogBuffer) Write(p []byte) (int, error) {
	l.write(tag{StreamClub, PhaseDev}, p)
	return len(p), nil
}

func (l *LogBuffer) Writer(stream Stream, phase Phase) io.Writer {
	return &tagWriter{buf: l, tag: tag{stream, phase}}
}

func (l *LogBuffer) WriteLine(stream Stream, phase Phase, text string) {
	l.write(tag{stream, phase}, []byte(strings.TrimSuffix(text, "\n")+"\n"))
}

type tagWriter struct {
	buf *LogBuffer
	tag tag
}

func (w *tagWriter) Write(p []byte) (int, error) {
	w.buf.write(w.tag, p)
	return len(p), nil
}

func (l *LogBuffer) write(t tag, p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer l.notifySubs()
	if l.open == nil {
		l.open = map[tag]int{}
	}
	s := string(p)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			if s != "" {
				l.appendOpen(t, s)
			}
			return
		}
		l.appendOpen(t, s[:i])
		if idx, ok := l.open[t]; ok {
			l.recs[idx].open = false
			delete(l.open, t)
		}
		s = s[i+1:]
	}
}

func (l *LogBuffer) appendOpen(t tag, s string) {
	if idx, ok := l.open[t]; ok {
		l.recs[idx].text += s
		return
	}
	l.recs = append(l.recs, record{tag: t, text: s, open: true})
	l.open[t] = len(l.recs) - 1
}

func (l *LogBuffer) render(match func(tag) bool) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, r := range l.recs {
		if match != nil && !match(r.tag) {
			continue
		}
		b.WriteString(r.text)
		if !r.open {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (l *LogBuffer) String() string { return l.render(nil) }

func (l *LogBuffer) Select(stream Stream, phase Phase) string {
	return l.render(func(t tag) bool {
		return (stream == "" || t.stream == stream) && (phase == "" || t.phase == phase)
	})
}

func (l *LogBuffer) Watchable() string { return l.Select(StreamStderr, PhaseDev) }

type Line struct {
	Seq    int64  `json:"seq"`
	Stream Stream `json:"stream,omitempty"`
	Phase  Phase  `json:"phase,omitempty"`
	Text   string `json:"text"`
}

type LogPage struct {
	Lines  []Line `json:"lines"`
	Cursor int64  `json:"cursor"`
	Missed int64  `json:"missed,omitempty"`
}

func (l *LogBuffer) Since(cursor int64, stream Stream, phase Phase) ([]Line, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sinceLocked(cursor, stream, phase)
}

func (l *LogBuffer) sinceLocked(cursor int64, stream Stream, phase Phase) ([]Line, int64) {
	next := cursor
	var out []Line
	for i, r := range l.recs {
		seq := l.base + int64(i) + 1
		if seq <= cursor {
			continue
		}
		if (stream == "" || r.tag.stream == stream) && (phase == "" || r.tag.phase == phase) {
			out = append(out, Line{Seq: seq, Stream: r.tag.stream, Phase: r.tag.phase, Text: r.text})
		}
		if !r.open {
			next = seq
		}
	}
	return out, next
}

func (l *LogBuffer) Subscribe(cursor int64, stream Stream, phase Phase) *LogSubscriber {
	q := logSubscriberQueue
	if TestSubscribeQueueSize > 0 {
		q = TestSubscribeQueueSize
	}
	ch := make(chan LogPage, q)
	sub := &logSubscriber{ch: ch, cursor: cursor, stream: stream, phase: phase}
	l.mu.Lock()
	lines, next := l.sinceLocked(cursor, stream, phase)
	l.subs = append(l.subs, sub)
	l.mu.Unlock()
	if sub.deliver(LogPage{Lines: lines, Cursor: next}) {
		sub.cursor = next
	}
	return &LogSubscriber{C: ch, close: func() { l.unsubscribe(sub) }}
}

func (l *LogBuffer) unsubscribe(sub *logSubscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, s := range l.subs {
		if s == sub {
			l.subs = append(l.subs[:i], l.subs[i+1:]...)
			lines, next := l.sinceLocked(sub.cursor, sub.stream, sub.phase)
			if len(lines) > 0 {
				page := LogPage{Lines: lines, Cursor: next}
				if sub.missed > 0 {
					page.Missed = sub.missed
					sub.missed = 0
				}
				sub.ch <- page
			} else {
				sub.flushPending()
			}
			close(sub.ch)
			return
		}
	}
}

func (sub *logSubscriber) deliver(page LogPage) bool {
	if sub.missed > 0 {
		page.Missed = sub.missed
		sub.missed = 0
	}
	if len(page.Lines) == 0 && page.Missed == 0 {
		return true
	}
	select {
	case sub.ch <- page:
		return true
	default:
		sub.missed += int64(len(page.Lines))
		if page.Missed > 0 {
			sub.missed += page.Missed
		}
		sub.flushMissed()
		return false
	}
}

func (sub *logSubscriber) flushMissed() {
	if sub.missed == 0 {
		return
	}
	select {
	case sub.ch <- LogPage{Missed: sub.missed}:
		sub.missed = 0
	default:
	}
}

func (sub *logSubscriber) flushPending() {
	if sub.missed > 0 {
		sub.ch <- LogPage{Missed: sub.missed}
		sub.missed = 0
	}
}

func (l *LogBuffer) notifySubs() {
	for _, sub := range l.subs {
		sub.flushMissed()
		lines, next := l.sinceLocked(sub.cursor, sub.stream, sub.phase)
		if len(lines) == 0 {
			continue
		}
		if sub.deliver(LogPage{Lines: lines, Cursor: next}) {
			sub.cursor = next
		}
	}
}

func (l *LogBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.base += int64(len(l.recs))
	l.recs = nil
	l.open = nil
	l.notifySubs()
}

func (l *LogBuffer) Tail(n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(l.String(), "\n"), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			keep = append([]string{lines[i]}, keep...)
		}
	}
	return strings.Join(keep, "\n")
}

type Supervisor struct {
	WorkDir string
	Command []string
	Logs    *LogBuffer

	mu        sync.Mutex
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	running   bool
	stopping  bool
	exitErr   error
	startedAt time.Time
	exitedAt  time.Time
	done      chan struct{}

	shortExits int
	gaveUp     bool
}

func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Command) == 0 {
		return nil
	}
	if s.Logs == nil {
		s.Logs = &LogBuffer{}
	}
	cctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	cmd := exec.CommandContext(cctx, s.Command[0], s.Command[1:]...)
	cmd.Dir = s.WorkDir
	port, _ := ResolveLocalPort()
	cmd.Env = WithPortEnv(os.Environ(), port)
	cmd.Stdout = s.Logs.Writer(StreamStdout, PhaseDev)
	cmd.Stderr = s.Logs.Writer(StreamStderr, PhaseDev)
	s.startedAt = time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		s.cancel = nil
		s.exitErr = err
		s.exitedAt = time.Now()
		s.done = closedChan()
		return err
	}
	s.cmd = cmd
	s.running = true
	s.stopping = false
	s.exitErr = nil
	s.exitedAt = time.Time{}
	done := make(chan struct{})
	s.done = done
	go s.reap(cmd, done)
	return nil
}

func (s *Supervisor) reap(cmd *exec.Cmd, done chan struct{}) {
	err := cmd.Wait()
	s.mu.Lock()
	s.running = false
	s.exitedAt = time.Now()
	if !s.stopping {
		s.exitErr = err
	}
	if s.cmd == cmd {
		s.cmd = nil
	}
	s.mu.Unlock()
	close(done)
}

func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Supervisor) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

func (s *Supervisor) RanFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startedAt.IsZero() {
		return 0
	}
	if s.exitedAt.IsZero() {
		return time.Since(s.startedAt)
	}
	return s.exitedAt.Sub(s.startedAt)
}

func (s *Supervisor) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

func (s *Supervisor) Stop() error {
	s.mu.Lock()
	s.stopping = true
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (s *Supervisor) Reload(ctx context.Context) error {
	_ = s.Stop()
	return s.Start(ctx)
}

const (
	maxShortExits = 3
	shortRun      = 5 * time.Second
)

var ErrGaveUp = errors.New("dev server exited immediately too many times in a row")

func (s *Supervisor) NeedsRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Command) > 0 && !s.startedAt.IsZero() && !s.running && !s.stopping && !s.gaveUp
}

func (s *Supervisor) Restart(ctx context.Context) error {
	s.mu.Lock()
	if s.gaveUp {
		s.mu.Unlock()
		return ErrGaveUp
	}
	if s.startedAt.IsZero() || s.exitedAt.IsZero() {
		s.mu.Unlock()
		return nil
	}
	if s.exitedAt.Sub(s.startedAt) < shortRun {
		s.shortExits++
	} else {
		s.shortExits = 0
	}
	if s.shortExits > maxShortExits {
		s.gaveUp = true
		s.mu.Unlock()
		return ErrGaveUp
	}
	s.mu.Unlock()
	return s.Start(ctx)
}

func (s *Supervisor) ShortExits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shortExits
}

func (s *Supervisor) ClearGiveUp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shortExits, s.gaveUp = 0, false
}

var lockfiles = []string{
	"pnpm-lock.yaml",
	"package-lock.json",
	"yarn.lock",
	"bun.lockb",
	"Cargo.lock",
	"go.sum",
}

func Install(ctx context.Context, workDir string, cmd []string, out io.Writer) error {
	if len(cmd) == 0 {
		cmd = DetectInstall(workDir)
	}
	if len(cmd) == 0 {
		return nil
	}
	if out == nil {
		out = io.Discard
	}
	var buf bytes.Buffer
	w := io.MultiWriter(out, &buf)
	if err := Run(ctx, workDir, cmd, w); err != nil {
		return fmt.Errorf("install %v: %w\n%s", cmd, err, buf.String())
	}
	return nil
}

func PostMergeInstall(ctx context.Context, workDir, oldHEAD, newHEAD string, installCmd []string, gitRun func(ctx context.Context, dir string, args ...string) (string, error), out io.Writer) error {
	changed := false
	for _, lf := range lockfiles {
		out, err := gitRun(ctx, workDir, "diff", "--name-only", oldHEAD, newHEAD, "--", lf)
		if err == nil && strings.TrimSpace(out) != "" {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	return Install(ctx, workDir, installCmd, out)
}

func DetectInstall(workDir string) []string {
	switch {
	case exists(workDir, "pnpm-lock.yaml"):
		return []string{"pnpm", "install"}
	case exists(workDir, "yarn.lock"):
		return []string{"yarn", "install"}
	case exists(workDir, "package-lock.json"), exists(workDir, "package.json"):
		return []string{"npm", "install"}
	case exists(workDir, "go.mod"):
		return []string{"go", "mod", "download"}
	case exists(workDir, "Cargo.lock"):
		return []string{"cargo", "fetch"}
	default:
		return nil
	}
}

func DetectDev(workDir string) []string {
	if hasNodeScript(workDir, "dev") {
		switch {
		case exists(workDir, "pnpm-lock.yaml"):
			return []string{"pnpm", "run", "dev"}
		case exists(workDir, "yarn.lock"):
			return []string{"yarn", "run", "dev"}
		default:
			return []string{"npm", "run", "dev"}
		}
	}
	if exists(workDir, "go.mod") && hasGoMainPackage(workDir) {
		return []string{"go", "run", "."}
	}
	return nil
}

func hasNodeScript(workDir, script string) bool {
	data, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts[script]) != ""
}

func hasGoMainPackage(workDir string) bool {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(workDir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "package main" {
				return true
			}
		}
	}
	return false
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func Run(ctx context.Context, workDir string, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("run: no command")
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Dir = workDir
	c.Stdout = out
	c.Stderr = out
	c.WaitDelay = killGrace
	return c.Run()
}

const killGrace = 5 * time.Second
