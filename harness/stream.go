package harness

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/nwylynko/clubhouse-cli/telemetry"
)

type EventKind string

const (
	EventThinking EventKind = "thinking"
	EventText     EventKind = "text"
	EventTool     EventKind = "tool"
)

type Event struct {
	Kind EventKind
	Tool string
	Text string
}

func (e Event) Line() string {
	switch e.Kind {
	case EventThinking:
		return "· " + e.Text
	case EventTool:
		return "▸ " + pad(e.Tool, 6) + " " + e.Text
	default:
		return e.Text
	}
}

func (e Event) Activity() string {
	if e.Kind != EventTool {
		return ""
	}
	return e.Tool + " " + e.Text
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

const maxDetail = 100

type streamDecoder struct {
	name Name
	emit func(Event)

	buf      string
	cwd      string
	thinking []string
	seenCall map[string]bool
	lastTool string
}

func newStreamDecoder(name Name, emit func(Event)) *streamDecoder {
	return &streamDecoder{name: name, emit: emit}
}

func (d *streamDecoder) Write(p []byte) (int, error) {
	d.buf += string(p)
	for {
		nl := strings.IndexByte(d.buf, '\n')
		if nl < 0 {
			break
		}
		d.line(d.buf[:nl])
		d.buf = d.buf[nl+1:]
	}
	return len(p), nil
}

func (d *streamDecoder) Close() {
	if rest := strings.TrimSpace(d.buf); rest != "" {
		d.line(rest)
	}
	d.buf = ""
	d.flushThinking()
}

func (d *streamDecoder) line(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	if d.name == Codex || !strings.HasPrefix(raw, "{") {
		d.flushThinking()
		d.send(Event{Kind: EventText, Text: raw})
		return
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		d.flushThinking()
		d.send(Event{Kind: EventText, Text: raw})
		return
	}
	switch d.name {
	case Cursor:
		d.cursorFrame(frame)
	default:
		d.claudeFrame(frame)
	}
}

func (d *streamDecoder) send(e Event) {
	if strings.TrimSpace(e.Text) == "" {
		return
	}
	e.Text = truncate(strings.TrimSpace(e.Text), maxDetail)
	telemetry.EmitHarnessEvent(string(e.Kind), e.Tool, e.Text, e.Activity())
	d.emit(e)
}

func (d *streamDecoder) think(text string) { d.thinking = append(d.thinking, text) }

func (d *streamDecoder) flushThinking() {
	if len(d.thinking) == 0 {
		return
	}
	whole := strings.Join(d.thinking, "")
	d.thinking = nil
	for _, part := range strings.Split(whole, "\n") {
		d.send(Event{Kind: EventThinking, Text: part})
	}
}

func (d *streamDecoder) tool(id, verb, detail string) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return
	}
	if id != "" {
		if d.seenCall[id] {
			return
		}
		if d.seenCall == nil {
			d.seenCall = map[string]bool{}
		}
		d.seenCall[id] = true
	} else {
		key := verb + "\x00" + detail
		if key == d.lastTool {
			return
		}
		d.lastTool = key
	}
	d.flushThinking()
	d.send(Event{Kind: EventTool, Tool: verb, Text: detail})
}

func (d *streamDecoder) rel(path string) string {
	if path == "" {
		return ""
	}
	if d.cwd != "" {
		if r := strings.TrimPrefix(path, strings.TrimSuffix(d.cwd, "/")+"/"); r != path {
			return r
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Base(path)
	}
	return path
}

func (d *streamDecoder) shorten(cmd string) string {
	if d.cwd == "" {
		return cmd
	}
	root := strings.TrimSuffix(d.cwd, "/")
	cmd = strings.ReplaceAll(cmd, root+"/", "")
	return strings.ReplaceAll(cmd, root, ".")
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func (d *streamDecoder) cursorFrame(frame map[string]json.RawMessage) {
	typ, sub := str(frame["type"]), str(frame["subtype"])
	switch typ {
	case "system":
		if cwd := str(frame["cwd"]); cwd != "" {
			d.cwd = cwd
		}
	case "thinking":
		if sub == "delta" {
			d.think(str(frame["text"]))
			return
		}
		d.flushThinking()
	case "assistant":
		d.flushThinking()
		for _, t := range messageText(frame["message"]) {
			d.send(Event{Kind: EventText, Text: t})
		}
	case "tool_call":
		var call map[string]json.RawMessage
		if err := json.Unmarshal(frame["tool_call"], &call); err != nil {
			return
		}
		for key, body := range call {
			verb := strings.TrimSuffix(key, "ToolCall")
			if verb == key {
				continue
			}
			var wrap struct {
				Args map[string]json.RawMessage `json:"args"`
			}
			if err := json.Unmarshal(body, &wrap); err != nil {
				continue
			}
			d.tool(str(frame["call_id"]), cursorVerb(verb), d.detail(wrap.Args))
			return
		}
	}
}

func cursorVerb(v string) string {
	switch v {
	case "shell":
		return "shell"
	case "edit", "write":
		return "edit"
	case "read":
		return "read"
	case "ls", "glob":
		return "list"
	case "grep", "search", "semSearch":
		return "search"
	case "delete", "rm":
		return "delete"
	default:
		return strings.ToLower(v)
	}
}

func (d *streamDecoder) claudeFrame(frame map[string]json.RawMessage) {
	switch str(frame["type"]) {
	case "system":
		if cwd := str(frame["cwd"]); cwd != "" {
			d.cwd = cwd
		}
	case "assistant":
		var msg struct {
			Content []struct {
				Type     string                     `json:"type"`
				Text     string                     `json:"text"`
				Thinking string                     `json:"thinking"`
				ID       string                     `json:"id"`
				Name     string                     `json:"name"`
				Input    map[string]json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal(frame["message"], &msg); err != nil {
			return
		}
		for _, block := range msg.Content {
			switch block.Type {
			case "thinking":
				d.think(block.Thinking)
			case "text":
				d.flushThinking()
				d.send(Event{Kind: EventText, Text: block.Text})
			case "tool_use":
				d.tool(block.ID, strings.ToLower(block.Name), d.detail(block.Input))
			}
		}
	}
}

func (d *streamDecoder) detail(args map[string]json.RawMessage) string {
	for _, key := range []string{"command", "cmd", "file_path", "path", "filePath", "notebook_path"} {
		if v := str(args[key]); v != "" {
			if key == "command" || key == "cmd" {
				return d.shorten(v)
			}
			return d.rel(v)
		}
	}
	for _, key := range []string{"pattern", "query", "url", "prompt", "description"} {
		if v := str(args[key]); v != "" {
			return v
		}
	}
	return ""
}

func messageText(raw json.RawMessage) []string {
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var out []string
	for _, b := range msg.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

func str(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

var _ io.Writer = (*streamDecoder)(nil)
