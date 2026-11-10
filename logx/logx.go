package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) tag() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	default:
		return "ERROR"
	}
}

var (
	mu    sync.Mutex
	out   io.Writer = os.Stderr
	level           = LevelInfo
	clock           = time.Now

	sinks      map[int]Sink
	nextSinkID int
)

func init() {
	switch strings.ToLower(os.Getenv("CLUB_LOG")) {
	case "debug", "trace":
		level = LevelDebug
	case "info":
		level = LevelInfo
	case "warn":
		level = LevelWarn
	case "error":
		level = LevelError
	}
}

func SetOutput(w io.Writer) io.Writer {
	mu.Lock()
	defer mu.Unlock()
	prev := out
	out = w
	return prev
}

type Sink func(ts time.Time, level, component, message string)

func AddSink(s Sink) (remove func()) {
	if s == nil {
		return func() {}
	}
	mu.Lock()
	id := nextSinkID
	nextSinkID++
	if sinks == nil {
		sinks = map[int]Sink{}
	}
	sinks[id] = s
	mu.Unlock()
	return func() {
		mu.Lock()
		delete(sinks, id)
		mu.Unlock()
	}
}

func SetLevel(l Level) Level {
	mu.Lock()
	defer mu.Unlock()
	prev := level
	level = l
	return prev
}

func DebugEnabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return level <= LevelDebug
}

type Logger struct{ component string }

func New(component string) *Logger { return &Logger{component: component} }

func (l *Logger) emit(lv Level, format string, args ...any) {
	mu.Lock()
	if lv < level {
		mu.Unlock()
		return
	}
	now := clock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(out, "%s %-5s %-6s %s\n", now.Format("15:04:05.000"), lv.tag(), l.component, msg)
	var observers []Sink
	for _, s := range sinks {
		observers = append(observers, s)
	}
	mu.Unlock()

	for _, s := range observers {
		s(now, strings.ToLower(lv.tag()), l.component, msg)
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.emit(LevelDebug, format, args...) }

func (l *Logger) Infof(format string, args ...any) { l.emit(LevelInfo, format, args...) }

func (l *Logger) Warnf(format string, args ...any) { l.emit(LevelWarn, format, args...) }

func (l *Logger) Errorf(format string, args ...any) { l.emit(LevelError, format, args...) }
