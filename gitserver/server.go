package gitserver

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/clubhouse-cli/devserver"
	sbGit "github.com/nwylynko/clubhouse-cli/git"
	"github.com/nwylynko/clubhouse-cli/netbind"
	"github.com/nwylynko/clubhouse-cli/sessionnet"
)

type Server struct {
	Bare string

	Bind string

	Logs func() string

	LogsSelect func(stream, phase string) string

	LogsSince func(cursor int64, stream devserver.Stream, phase devserver.Phase) ([]devserver.Line, int64)

	LogsSubscribe func(cursor int64, stream devserver.Stream, phase devserver.Phase) *devserver.LogSubscriber

	Session *SessionNet

	mu            sync.Mutex
	ln            net.Listener
	sessionLn     net.Listener
	srv           *http.Server
	url           string
	sessionURL    string
	sessionDirect string
	name          string
	closing       chan struct{}
}

func (s *Server) closingCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing == nil {
		s.closing = make(chan struct{})
	}
	return s.closing
}

type SessionNet struct {
	Relay   string
	PIN     string
	Key     sessionnet.Key
	Context context.Context
	Direct  bool
	Ticket  func() (string, error)
}

func (s *Server) Start(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return s.url, nil
	}
	if name == "" {
		name = "canonical.git"
	}
	s.name = name
	abs, err := filepath.Abs(s.Bare)
	if err != nil {
		return "", err
	}
	s.Bare = abs

	mux := http.NewServeMux()
	prefix := "/" + name
	mux.HandleFunc(prefix+"/info/refs", s.infoRefs)
	mux.HandleFunc(prefix+"/git-upload-pack", s.service("upload-pack"))
	mux.HandleFunc(prefix+"/git-receive-pack", s.service("receive-pack"))
	mux.HandleFunc("/logs", s.logs)
	mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.ln = ln
	srv := &http.Server{Handler: mux}
	s.srv = srv
	port := ln.Addr().(*net.TCPAddr).Port
	s.url = fmt.Sprintf("http://%s/%s", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), name)
	go func() { _ = srv.Serve(ln) }()

	if s.Session != nil {
		if err := s.publishSessionLocked(); err != nil {
			_ = ln.Close()
			s.ln = nil
			return "", err
		}
	}
	return s.url, nil
}

func (s *Server) UnpublishSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionLn == nil {
		return
	}
	_ = s.sessionLn.Close()
	s.sessionLn = nil
	s.sessionURL = ""
	s.sessionDirect = ""
}

func (s *Server) PublishSession() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return nil
	}
	if s.sessionLn != nil {
		return nil
	}
	if s.srv == nil {
		return fmt.Errorf("gitserver: PublishSession before Start")
	}
	return s.publishSessionLocked()
}

func (s *Server) publishSessionLocked() error {
	ctx := s.Session.Context
	if ctx == nil {
		ctx = context.Background()
	}
	hc := sessionnet.HolderConfig{
		Relay: s.Session.Relay, PIN: s.Session.PIN,
		Service: "git", Key: s.Session.Key,
		Ticket: s.Session.Ticket,
	}
	if s.Session.Direct {
		dln, advHost, derr := netbind.ListenAdvertise(s.Bind, 0)
		if derr != nil {
			return fmt.Errorf("direct session listener: %w", derr)
		}
		hc.DirectListener = dln
		hc.DirectAdvertise = net.JoinHostPort(advHost, strconv.Itoa(dln.Addr().(*net.TCPAddr).Port))
	}
	sln, err := sessionnet.Serve(ctx, hc)
	if err != nil {
		return fmt.Errorf("publish canonical onto the session network: %w", err)
	}
	s.sessionLn = sln
	name := s.name
	if name == "" {
		name = "canonical.git"
	}
	s.sessionURL = sessionnet.FormatURL(s.Session.PIN, "git", name)
	s.sessionDirect = sessionnet.DirectAddr(sln)
	go func() { _ = s.srv.Serve(sln) }()
	return nil
}

func (s *Server) SessionURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionURL
}

func (s *Server) SessionDirect() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionDirect
}

func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	if svc != "git-upload-pack" && svc != "git-receive-pack" {
		http.Error(w, "unsupported service", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	pktWrite(&buf, "# service="+svc+"\n")
	buf.WriteString("0000")
	cmd := s.gitCmd(r.Context(), strings.TrimPrefix(svc, "git-"), "--stateless-rpc", "--advertise-refs", s.Bare)
	cmd.Stdout = &buf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		http.Error(w, errBuf.String(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

const gitFrameBuffer = 128 << 10

type writerOnly struct{ w io.Writer }

func (o writerOnly) Write(p []byte) (int, error) { return o.w.Write(p) }

func (s *Server) service(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := decodedRequestBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", "application/x-git-"+name+"-result")
		w.Header().Set("Cache-Control", "no-cache")
		cmd := s.gitCmd(r.Context(), name, "--stateless-rpc", s.Bare)
		cmd.Stdin = body
		out := bufio.NewWriterSize(writerOnly{w}, gitFrameBuffer)
		cmd.Stdout = out
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		defer out.Flush()
		if err := cmd.Run(); err != nil {
			_, _ = io.WriteString(os.Stderr, "gitserver "+name+": "+errBuf.String()+"\n")
		}
	}
}

func decodedRequestBody(r *http.Request) (io.ReadCloser, error) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		return r.Body, nil
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, fmt.Errorf("Content-Encoding: gzip but the body is not gzip: %w", err)
	}
	return zr, nil
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	stream, phase := r.URL.Query().Get("stream"), r.URL.Query().Get("phase")
	if raw := r.URL.Query().Get("since"); raw != "" && s.LogsSince != nil {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			http.Error(w, "since must be a cursor from a previous response", 400)
			return
		}
		st, ph := devserver.Stream(stream), devserver.Phase(phase)
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") && s.LogsSubscribe != nil {
			s.logsStream(w, r, cursor, st, ph)
			return
		}
		lines, next := s.LogsSince(cursor, st, ph)
		if lines == nil {
			lines = []devserver.Line{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(devserver.LogPage{Lines: lines, Cursor: next})
		return
	}
	if stream != "" || phase != "" {
		if s.LogsSelect == nil {
			return
		}
		_, _ = io.WriteString(w, s.LogsSelect(stream, phase))
		return
	}
	if s.Logs == nil {
		return
	}
	_, _ = io.WriteString(w, s.Logs())
}

func (s *Server) logsStream(w http.ResponseWriter, r *http.Request, cursor int64, stream devserver.Stream, phase devserver.Phase) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		lines, next := s.LogsSince(cursor, stream, phase)
		if lines == nil {
			lines = []devserver.Line{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(devserver.LogPage{Lines: lines, Cursor: next})
		return
	}
	sub := s.LogsSubscribe(cursor, stream, phase)
	if sub == nil {
		http.Error(w, "log stream unavailable", 503)
		return
	}
	defer sub.Close()
	closing := s.closingCh()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-closing:
			return
		case page, ok := <-sub.C:
			if !ok {
				return
			}
			writeLogSSE(w, page)
			flusher.Flush()
		case <-ticker.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeLogSSE(w http.ResponseWriter, page devserver.LogPage) {
	b, _ := json.Marshal(page)
	fmt.Fprintf(w, "event: page\ndata: %s\n\n", b)
}

func (s *Server) gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	bin, err := sbGit.Bin()
	if err != nil {
		cmd := exec.CommandContext(ctx, "false")
		return cmd
	}
	full := make([]string, 0, len(sbGit.HermeticConfig())*2+len(args))
	for _, kv := range sbGit.HermeticConfig() {
		full = append(full, "-c", kv)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Env = sbGit.Env()
	return cmd
}

func pktWrite(w io.Writer, s string) {
	fmt.Fprintf(w, "%04x%s", len(s)+4, s)
}

func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return nil
	}
	if s.closing != nil {
		close(s.closing)
		s.closing = nil
	}
	if s.sessionLn != nil {
		_ = s.sessionLn.Close()
		s.sessionLn = nil
	}
	err := s.srv.Shutdown(ctx)
	s.srv = nil
	s.ln = nil
	s.url = ""
	s.sessionURL = ""
	return err
}
