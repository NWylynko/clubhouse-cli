package boxexec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"

	"github.com/nwylynko/clubhouse-cli/logx"
	"github.com/nwylynko/clubhouse-cli/sessionnet"
)

const Service = "exec"

type HolderOptions struct {
	Relay      string
	PIN        string
	Key        sessionnet.Key
	WorkDir    string
	OnShutdown func()
	Ticket     func() (string, error)
}

type Holder struct {
	ln     net.Listener
	cancel context.CancelFunc
	once   sync.Once
	opt    HolderOptions
}

func StartHolder(ctx context.Context, opt HolderOptions) (*Holder, error) {
	if opt.Relay == "" {
		return nil, errors.New("boxexec: no relay address")
	}
	if opt.WorkDir == "" {
		return nil, errors.New("boxexec: no work directory")
	}
	if opt.Key.Zero() {
		return nil, fmt.Errorf("boxexec: serving %s needs the session key", Service)
	}
	ctx, cancel := context.WithCancel(ctx)
	ln, err := sessionnet.Serve(ctx, sessionnet.HolderConfig{
		Relay: opt.Relay, PIN: opt.PIN, Service: Service, Key: opt.Key,
		Ticket: opt.Ticket,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("publish box exec onto the session network: %w", err)
	}
	h := &Holder{ln: ln, cancel: cancel, opt: opt}
	go h.accept(ctx)
	logx.New("boxexec").Infof("box exec published on the session network for %s", opt.PIN)
	return h, nil
}

func (h *Holder) Close() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		h.cancel()
		_ = h.ln.Close()
	})
	return nil
}

func (h *Holder) accept(ctx context.Context) {
	log := logx.New("boxexec")
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			if err := h.serveConn(ctx, conn); err != nil && ctx.Err() == nil {
				log.Debugf("exec connection: %v", err)
			}
		}(c)
	}
}

func (h *Holder) serveConn(ctx context.Context, conn net.Conn) error {
	typ, body, err := readMsg(conn)
	if err != nil {
		return err
	}
	switch typ {
	case msgShutdown:
		if h.opt.OnShutdown != nil {
			h.opt.OnShutdown()
		}
		return nil
	case msgRun:
		argv, err := readArgv(body)
		if err != nil {
			_ = writeError(conn, err)
			return err
		}
		return h.run(ctx, conn, argv)
	default:
		return fmt.Errorf("boxexec: unexpected first message %d", typ)
	}
}

func (h *Holder) run(ctx context.Context, conn net.Conn, argv []string) error {
	if len(argv) == 0 {
		return writeError(conn, errors.New("empty argv"))
	}
	src := ""
	if addr := conn.RemoteAddr(); addr != nil {
		src = addr.String()
	}
	logx.New("boxexec").Infof("exec argv=%q source=%s", argv, src)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wmu sync.Mutex
	write := func(typ byte, body []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeMsg(conn, typ, body)
	}

	stdinReader, stdinWriter := io.Pipe()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = h.opt.WorkDir
	cmd.Stdin = stdinReader
	cmd.Stdout = &msgWriter{write: write, typ: msgStdout}
	cmd.Stderr = &msgWriter{write: write, typ: msgStderr}
	if err := cmd.Start(); err != nil {
		_ = write(msgError, appendString(nil, err.Error()))
		return err
	}

	go h.pumpStdin(conn, stdinWriter, cancel, func() {})

	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			_ = write(msgError, appendString(nil, err.Error()))
			code = 1
		}
	}
	_ = stdinWriter.Close()
	_ = stdinReader.Close()
	if code < 0 {
		code = 1
	}
	var body [4]byte
	binary.BigEndian.PutUint32(body[:], uint32(code))
	return write(msgExit, body[:])
}

type msgWriter struct {
	write func(byte, []byte) error
	typ   byte
}

func (m *msgWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := m.write(m.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (h *Holder) pumpStdin(conn net.Conn, stdin *io.PipeWriter, cancel context.CancelFunc, _ func()) {
	defer stdin.Close()
	for {
		typ, body, err := readMsg(conn)
		if err != nil {
			cancel()
			return
		}
		switch typ {
		case msgStdin:
			if _, err := stdin.Write(body); err != nil {
				cancel()
				return
			}
		case msgStdinEOF:
			return
		default:
			cancel()
			return
		}
	}
}

func writeError(conn net.Conn, err error) error {
	if err == nil {
		return nil
	}
	return writeMsg(conn, msgError, appendString(nil, err.Error()))
}
