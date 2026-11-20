package workflows

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultCallbackTimeout is events.await_callback's default timeout,
// 43,200 seconds (stdlib/events/await_callback).
const DefaultCallbackTimeout = 43200

// callbackSet is an execution's callback endpoints
// (events.create_callback_endpoint): requests received wait for an
// await_callback, and an await_callback waits for a request.
type callbackSet struct {
	mu     sync.Mutex
	byID   map[string]*callback
	base   string // the execution's resource name
	url    func(name string) string
	now    func() time.Time
	closed bool
}

type callback struct {
	id      string
	method  string
	queue   []map[string]any
	waiters int
	signal  chan struct{}
}

// CallbackInfo is a callbacks.list entry.
type CallbackInfo struct {
	Name              string   `json:"name"`
	Method            string   `json:"method"`
	AvailablePayloads []string `json:"availablePayloads,omitempty"`
	Waiters           int64    `json:"waiters,string"`
}

func (x *machine) createCallback(method any) (any, error) {
	m := "POST"
	if method != nil {
		s, err := str(method, "http_callback_method")
		if err != nil {
			return nil, err
		}
		m = strings.ToUpper(s)
	}
	switch m {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "PATCH":
	default:
		return nil, raiseErr("ValueError", "http_callback_method %q is not an HTTP method", m)
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	c := x.callbacks
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byID[id] = &callback{id: id, method: m, signal: make(chan struct{}, 1)}
	return map[string]any{"url": c.url(c.base + "/callbacks/" + id)}, nil
}

// deliver hands a callback request to its endpoint. ok is false for an
// unknown endpoint; methodOK false for the wrong method.
func (c *callbackSet) deliver(id, method string, req map[string]any) (ok, methodOK bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cb := c.byID[id]
	if cb == nil || c.closed {
		return false, false
	}
	if !strings.EqualFold(cb.method, method) {
		return true, false
	}
	cb.queue = append(cb.queue, map[string]any{"http_request": req, "received_time": c.now().UTC().Format(time.RFC3339Nano), "type": "HTTP"})
	select {
	case cb.signal <- struct{}{}:
	default:
	}
	return true, true
}

func (c *callbackSet) list() []CallbackInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CallbackInfo
	for _, cb := range c.byID {
		info := CallbackInfo{Name: c.base + "/callbacks/" + cb.id, Method: cb.method, Waiters: int64(cb.waiters)}
		for _, q := range cb.queue {
			if b, err := encodeJSON(q, false); err == nil {
				info.AvailablePayloads = append(info.AvailablePayloads, string(b))
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *callbackSet) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

// awaitCallback is events.await_callback: the first request the endpoint
// receives, or a TimeoutError.
func (x *machine) awaitCallback(args map[string]any) (any, error) {
	for k := range args {
		if k != "callback" && k != "timeout" {
			return nil, raiseErr("TypeError", "events.await_callback got an unexpected argument %q", k)
		}
	}
	cbv, ok := args["callback"].(map[string]any)
	if !ok {
		return nil, raiseErr("TypeError", "callback must be the map events.create_callback_endpoint returned")
	}
	u, _ := cbv["url"].(string)
	i := strings.LastIndex(u, "/callbacks/")
	if i < 0 {
		return nil, raiseErr("ValueError", "callback %q is not a callback endpoint of this execution", u)
	}
	id := u[i+len("/callbacks/"):]
	timeout := float64(DefaultCallbackTimeout)
	if t, ok := toFloat(args["timeout"]); ok {
		timeout = t
	}
	c := x.callbacks
	c.mu.Lock()
	cb := c.byID[id]
	if cb == nil {
		c.mu.Unlock()
		return nil, raiseErr("ValueError", "callback %q is not a callback endpoint of this execution", u)
	}
	cb.waiters++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		cb.waiters--
		c.mu.Unlock()
	}()
	deadline := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer deadline.Stop()
	for {
		c.mu.Lock()
		if len(cb.queue) > 0 {
			v := cb.queue[0]
			cb.queue = cb.queue[1:]
			c.mu.Unlock()
			return v, nil
		}
		c.mu.Unlock()
		select {
		case <-cb.signal:
		case <-deadline.C:
			return nil, raiseErr("TimeoutError", "timed out after %v seconds waiting for a callback", timeout)
		case <-x.ctx.Done():
			return nil, x.ctx.Err()
		}
	}
}
