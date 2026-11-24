package nbruntime

// A Colab runtime's kernel (#2198): one ipykernel Python 3 kernel on the
// runtime's Jupyter Server, started through the server's REST API
// (POST /api/kernels) and driven through its channels WebSocket
// (/api/kernels/ID/channels), with Jupyter's messaging protocol 5.3
// (https://jupyter-client.readthedocs.io/en/stable/messaging.html), as a
// Colab notebook's frontend drives its runtime's kernel. A cell is an
// execute_request on the shell channel; its outputs are the iopub
// messages whose parent is that request (stream, display_data,
// update_display_data, execute_result, error and clear_output), folded
// into nbformat 4 output objects; it ends when the execute_reply has come
// and the kernel's status is idle again. Interrupt execution is the
// server's POST /api/kernels/ID/interrupt.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// maxText bounds one output's text, as the console shows it.
const maxText = 1 << 20

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

type kernelMsg struct {
	Header       map[string]any  `json:"header"`
	ParentHeader map[string]any  `json:"parent_header"`
	Metadata     map[string]any  `json:"metadata"`
	Content      json.RawMessage `json:"content"`
	Buffers      []any           `json:"buffers"`
	Channel      string          `json:"channel"`
}

func (k kernelMsg) msgType() string { s, _ := k.Header["msg_type"].(string); return s }
func (k kernelMsg) parent() string  { s, _ := k.ParentHeader["msg_id"].(string); return s }

// kernelConn is a runtime's kernel and its channels.
type kernelConn struct {
	addr, token, id, session string
	ws                       *websocket.Conn
	msgs                     chan kernelMsg
	closeOnce                sync.Once
	python                   string
	notes                    []string
}

func (k *kernelConn) close() {
	k.closeOnce.Do(func() { _ = k.ws.Close() })
}

func kernelRequest(ctx context.Context, method, addr, token, path string, body any, v any) error {
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// startKernel starts the runtime's Python 3 kernel and opens its channels.
func startKernel(ctx context.Context, addr, token string) (*kernelConn, error) {
	var kernel struct {
		ID string `json:"id"`
	}
	if err := kernelRequest(ctx, http.MethodPost, addr, token, "/api/kernels", map[string]string{"name": "python3"}, &kernel); err != nil {
		return nil, fmt.Errorf("start the runtime's kernel: %w", err)
	}
	k := &kernelConn{addr: addr, token: token, id: kernel.ID, session: randomHex(16), msgs: make(chan kernelMsg, 256)}
	cfg, err := websocket.NewConfig("ws://"+addr+"/api/kernels/"+kernel.ID+"/channels?session_id="+k.session, "http://"+addr)
	if err != nil {
		return nil, err
	}
	cfg.Header = http.Header{"Authorization": {"token " + token}}
	var ws *websocket.Conn
	for deadline := time.Now().Add(time.Minute); ; {
		ws, err = cfg.DialContext(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("open the kernel's channels: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	ws.MaxPayloadBytes = 64 << 20
	k.ws = ws
	go func() {
		defer close(k.msgs)
		for {
			var m kernelMsg
			if err := websocket.JSON.Receive(ws, &m); err != nil {
				return
			}
			k.msgs <- m
		}
	}()
	// kernel_info: the kernel is ready, and its Python's version.
	id, err := k.send("shell", "kernel_info_request", map[string]any{})
	if err != nil {
		k.close()
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		m, err := k.next(wait)
		if err != nil {
			k.close()
			return nil, fmt.Errorf("the runtime's kernel did not answer: %w", err)
		}
		if m.parent() == id && m.msgType() == "kernel_info_reply" {
			var info struct {
				LanguageInfo struct {
					Version string `json:"version"`
				} `json:"language_info"`
			}
			_ = json.Unmarshal(m.Content, &info)
			k.python = info.LanguageInfo.Version
			break
		}
	}
	// The setup's notes (cloudburrow_colab's), silently.
	res, err := k.execute(wait, "import cloudburrow_colab as _cb, json as _j; print(_j.dumps(_cb.notes)); del _cb, _j", true)
	if err == nil {
		for _, o := range res.Outputs {
			var s struct {
				Name string `json:"name"`
				Text string `json:"text"`
			}
			if json.Unmarshal(o, &s) == nil && s.Name == "stdout" {
				_ = json.Unmarshal([]byte(strings.TrimSpace(s.Text)), &k.notes)
			}
		}
	}
	return k, nil
}

func (k *kernelConn) send(channel, msgType string, content any) (string, error) {
	id := randomHex(16)
	m := map[string]any{
		"header": map[string]any{"msg_id": id, "username": "colab", "session": k.session, "msg_type": msgType,
			"version": "5.3", "date": time.Now().UTC().Format(time.RFC3339Nano)},
		"parent_header": map[string]any{}, "metadata": map[string]any{}, "content": content, "buffers": []any{}, "channel": channel,
	}
	if err := websocket.JSON.Send(k.ws, m); err != nil {
		return "", errors.New("the runtime's kernel channels closed: " + err.Error())
	}
	return id, nil
}

var (
	errKernelGone = errors.New("the runtime's kernel channels closed")
	errKernelDied = errors.New("the runtime's kernel died while the cell ran")
)

func (k *kernelConn) next(ctx context.Context) (kernelMsg, error) {
	select {
	case m, ok := <-k.msgs:
		if !ok {
			return kernelMsg{}, errKernelGone
		}
		return m, nil
	case <-ctx.Done():
		return kernelMsg{}, ctx.Err()
	}
}

func (k *kernelConn) interrupt(ctx context.Context) error {
	return kernelRequest(ctx, http.MethodPost, k.addr, k.token, "/api/kernels/"+k.id+"/interrupt", nil, nil)
}

func clip(s string) string {
	if len(s) <= maxText {
		return s
	}
	return s[:maxText] + "\n… (output truncated)"
}

// mime keeps a display's text, image and JSON representations.
func mime(data map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range data {
		if strings.HasPrefix(k, "text/") || strings.HasPrefix(k, "image/") || k == "application/json" {
			if s, ok := v.(string); ok {
				v = clip(s)
			}
			out[k] = v
		}
	}
	return out
}

// outputs folds a cell's iopub messages into nbformat outputs.
type outputs struct {
	list         []map[string]any
	clearPending bool
	displays     map[string]int
}

func (o *outputs) add(out map[string]any, displayID string) {
	if o.clearPending {
		o.list, o.displays, o.clearPending = nil, nil, false
	}
	if displayID != "" {
		if o.displays == nil {
			o.displays = map[string]int{}
		}
		o.displays[displayID] = len(o.list)
	}
	o.list = append(o.list, out)
}

func (o *outputs) take(m kernelMsg) {
	var c struct {
		Name           string         `json:"name"`
		Text           string         `json:"text"`
		Data           map[string]any `json:"data"`
		Metadata       map[string]any `json:"metadata"`
		Transient      map[string]any `json:"transient"`
		ExecutionCount int            `json:"execution_count"`
		Ename          string         `json:"ename"`
		Evalue         string         `json:"evalue"`
		Traceback      []string       `json:"traceback"`
		Wait           bool           `json:"wait"`
	}
	_ = json.Unmarshal(m.Content, &c)
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	displayID, _ := c.Transient["display_id"].(string)
	switch m.msgType() {
	case "stream":
		if o.clearPending {
			o.list, o.displays, o.clearPending = nil, nil, false
		}
		if n := len(o.list); n > 0 && o.list[n-1]["output_type"] == "stream" && o.list[n-1]["name"] == c.Name {
			o.list[n-1]["text"] = clip(o.list[n-1]["text"].(string) + c.Text)
			return
		}
		o.add(map[string]any{"output_type": "stream", "name": c.Name, "text": clip(c.Text)}, "")
	case "display_data":
		o.add(map[string]any{"output_type": "display_data", "data": mime(c.Data), "metadata": c.Metadata}, displayID)
	case "update_display_data":
		if i, ok := o.displays[displayID]; ok && i < len(o.list) {
			o.list[i]["data"], o.list[i]["metadata"] = mime(c.Data), c.Metadata
		}
	case "execute_result":
		o.add(map[string]any{"output_type": "execute_result", "execution_count": c.ExecutionCount, "data": mime(c.Data), "metadata": c.Metadata}, displayID)
	case "error":
		tb := make([]string, len(c.Traceback))
		for i, l := range c.Traceback {
			tb[i] = ansi.ReplaceAllString(clip(l), "")
		}
		o.add(map[string]any{"output_type": "error", "ename": c.Ename, "evalue": clip(c.Evalue), "traceback": tb}, "")
	case "clear_output":
		if c.Wait {
			o.clearPending = true
		} else {
			o.list, o.displays = nil, nil
		}
	}
}

// execute runs one cell and returns its outputs. A context that ends
// first returns its error; the caller interrupts the kernel.
func (k *kernelConn) execute(ctx context.Context, code string, silent bool) (Result, error) {
	id, err := k.send("shell", "execute_request", map[string]any{"code": code, "silent": false, "store_history": !silent,
		"user_expressions": map[string]any{}, "allow_stdin": false, "stop_on_error": true})
	if err != nil {
		return Result{}, err
	}
	var outs outputs
	var res Result
	replied, idle := false, false
	for !replied || !idle {
		m, err := k.next(ctx)
		if err != nil {
			return Result{}, err
		}
		if m.msgType() == "status" {
			var c struct {
				State string `json:"execution_state"`
			}
			_ = json.Unmarshal(m.Content, &c)
			if c.State == "restarting" || c.State == "dead" {
				return Result{}, errKernelDied
			}
		}
		if m.parent() != id {
			continue
		}
		switch t := m.msgType(); {
		case t == "execute_reply":
			var c struct {
				Status         string `json:"status"`
				ExecutionCount int    `json:"execution_count"`
			}
			_ = json.Unmarshal(m.Content, &c)
			res.Status, res.ExecutionCount = c.Status, c.ExecutionCount
			if res.Status != "ok" {
				res.Status = "error"
			}
			replied = true
		case t == "status":
			var c struct {
				State string `json:"execution_state"`
			}
			_ = json.Unmarshal(m.Content, &c)
			idle = c.State == "idle"
		default:
			outs.take(m)
		}
	}
	res.Outputs = make([]json.RawMessage, 0, len(outs.list))
	for _, o := range outs.list {
		b, _ := json.Marshal(o)
		res.Outputs = append(res.Outputs, b)
	}
	return res, nil
}
