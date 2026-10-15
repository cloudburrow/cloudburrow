package storage

// Object change notification channels (#1281), to
// docs.cloud.google.com/storage/docs/object-change-notification:
// objects.watchAll registers a web hook channel on a bucket, the server POSTs
// it a sync message, then one notification per object change, and
// channels.stop ends it.
//
// Google removed objects.watchAll from the discovery document (revision
// 20260821, the one pinned here, has channels.stop alone), so it is routed by
// its documented path, POST b/{bucket}/o/watch, ahead of the discovery
// methods, and is not in methodStatus.
//
// Every message carries X-Goog-Channel-ID, X-Goog-Channel-Token (when the
// channel has one), X-Goog-Channel-Expiration (when it has one),
// X-Goog-Resource-ID, X-Goog-Resource-URI, X-Goog-Resource-State and
// X-Goog-Message-Number. The sync message is number 1, has state sync and no
// body; a change has state exists (a new live version, or a metadata change)
// or not_exists (the live version deleted or archived, not replaced), and the
// object resource as its body.
//
// Channels and their pending messages are kept in the server's store, like
// notificationConfigs, written in the mutation's own transaction: delivery is
// at least once and in order per channel, retried with backoff up to
// maxChannelAttempts. A channel past its expiration is removed, and its
// pending messages with it. Google requires an https address; CloudBurrow
// accepts http as well, since a local receiver rarely has a certificate.
//
// Messages are POSTed only to local addresses (LocalChannelClient):
// CloudBurrow never calls outside. In the cluster the server POSTs nothing
// (Options.RelayChannels, #1290): the CLI takes the messages from
// ChannelOutboxPath through the tunnel and POSTs them on the host.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	channelPrefix      = "channel/"
	channelOutPrefix   = "chanout/"
	maxChannelAttempts = 8
	// ChannelsPath lists a bucket's active channels (?bucket=), a CloudBurrow
	// extension for the console: Cloud Storage has no channels.list.
	ChannelsPath = "/_cloudburrow/channels"
)

var watchAllRE = pathRE("b/{bucket}/o/watch")

// channelRecord is one registered channel.
type channelRecord struct {
	ID         string            `json:"id"`
	ResourceID string            `json:"resourceId"`
	Bucket     string            `json:"bucket"`
	Address    string            `json:"address"`
	Token      string            `json:"token,omitempty"`
	Expiration int64             `json:"expiration,omitempty"` // Unix ms; 0 is none
	Params     map[string]string `json:"params,omitempty"`
	Prefix     string            `json:"prefix,omitempty"`
	Next       int64             `json:"next"` // the next message number
	Created    time.Time         `json:"created"`
}

func (c channelRecord) expired(now time.Time) bool {
	return c.Expiration != 0 && now.UnixMilli() >= c.Expiration
}

func (c channelRecord) resourceURI() string {
	return "https://storage.googleapis.com" + jsonPrefix + "b/" + escape(c.Bucket) + "/o"
}

// channelMessage is one message bound for one channel.
type channelMessage struct {
	Channel  string        `json:"channel"` // the channel's key
	Number   int64         `json:"number"`
	State    string        `json:"state"` // sync, exists or not_exists
	Object   *objectRecord `json:"object,omitempty"`
	Attempts int           `json:"attempts,omitempty"`
	// Due is when a relayed message that failed may be offered again.
	Due time.Time `json:"due,omitzero"`
}

func channelKey(bucket, resourceID string) string {
	return channelPrefix + bucket + "/" + resourceID
}

func channelsOf(tx Tx, bucket string) []channelRecord {
	var out []channelRecord
	for _, k := range tx.List(channelPrefix + bucket + "/") {
		raw, _ := tx.Get(k)
		var c channelRecord
		if json.Unmarshal(raw, &c) == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// queueChannelMessage numbers and writes a message for c, in tx.
func queueChannelMessage(tx Tx, c *channelRecord, state string, o *objectRecord, now time.Time) error {
	c.Next++
	m := channelMessage{Channel: channelKey(c.Bucket, c.ResourceID), Number: c.Next, State: state, Object: o}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tx.Put(channelOutPrefix+strings.TrimPrefix(outboxKey(now), outboxPrefix), raw)
	craw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tx.Put(channelKey(c.Bucket, c.ResourceID), craw)
	return nil
}

// emitChannels queues ev for every live channel on its bucket. A version
// another replaced is not a not_exists: the object still exists, and the
// replacing version's exists follows.
func emitChannels(tx Tx, ev objectEvent) error {
	state := "exists"
	switch ev.Type {
	case EventDelete, EventArchive:
		if ev.OverwrittenBy != 0 {
			return nil
		}
		state = "not_exists"
	}
	for _, c := range channelsOf(tx, ev.Object.Bucket) {
		if c.expired(ev.Time) {
			removeChannel(tx, c)
			continue
		}
		if c.Prefix != "" && !strings.HasPrefix(ev.Object.Name, c.Prefix) {
			continue
		}
		o := ev.Object
		if err := queueChannelMessage(tx, &c, state, &o, ev.Time); err != nil {
			return err
		}
	}
	return nil
}

// removeChannel deletes c and every message still pending for it.
func removeChannel(tx Tx, c channelRecord) {
	key := channelKey(c.Bucket, c.ResourceID)
	tx.Delete(key)
	for _, k := range tx.List(channelOutPrefix) {
		raw, _ := tx.Get(k)
		var m channelMessage
		if json.Unmarshal(raw, &m) == nil && m.Channel == key {
			tx.Delete(k)
		}
	}
}

// deleteChannels removes a deleted bucket's channels.
func deleteChannels(tx Tx, bucket string) {
	for _, c := range channelsOf(tx, bucket) {
		removeChannel(tx, c)
	}
}

func (s *Server) channelJSON(c channelRecord) map[string]any {
	out := map[string]any{"kind": "api#channel", "id": c.ID, "resourceId": c.ResourceID,
		"resourceUri": c.resourceURI(), "address": c.Address, "type": "web_hook"}
	if c.Token != "" {
		out["token"] = c.Token
	}
	if c.Expiration != 0 {
		out["expiration"] = strconv.FormatInt(c.Expiration, 10)
	}
	if len(c.Params) > 0 {
		out["params"] = c.Params
	}
	return out
}

// unsupportedWatchParams are objects.watchAll's listing parameters that
// would narrow what is watched and are not built. Paging and projection
// change nothing about notifications and are accepted.
var unsupportedWatchParams = []string{"delimiter", "startOffset", "endOffset", "includeTrailingDelimiter", "matchGlob"}

func (s *Server) objectsWatchAll(w http.ResponseWriter, r *http.Request) {
	bucket := pathVar(r, jsonPrefix, 1)
	q := r.URL.Query()
	for _, p := range unsupportedWatchParams {
		if q.Get(p) != "" {
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented",
				"storage.objects.watchAll with %s is not implemented: a channel watches every object, or those under prefix", p))
			return
		}
	}
	if q.Get("versions") == "true" {
		writeError(w, errorf(http.StatusNotImplemented, "notImplemented",
			"storage.objects.watchAll with versions=true is not implemented: a channel watches live versions"))
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	str := func(k string) string { v, _ := body[k].(string); return v }
	c := channelRecord{ID: str("id"), Bucket: bucket, Address: str("address"), Token: str("token"),
		Prefix: q.Get("prefix"), Created: s.now()}
	switch {
	case c.ID == "":
		writeError(w, required("resource.id"))
		return
	case c.Address == "":
		writeError(w, required("resource.address"))
		return
	}
	if t := strings.ToLower(str("type")); t != "web_hook" && t != "webhook" {
		writeError(w, badRequest("Invalid value for resource.type: %q. Only web_hook channels are supported.", str("type")))
		return
	}
	if u, perr := url.Parse(c.Address); perr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		writeError(w, badRequest("Invalid value for resource.address: %q. It must be an https:// (or, locally, http://) URL.", c.Address))
		return
	}
	if v, ok := body["expiration"]; ok && v != nil {
		ms, perr := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		if perr != nil {
			writeError(w, badRequest("Invalid value for resource.expiration: %v", v))
			return
		}
		if ms != 0 && ms <= s.now().UnixMilli() {
			writeError(w, badRequest("Invalid value for resource.expiration: %d is in the past.", ms))
			return
		}
		c.Expiration = ms
	}
	if p, ok := body["params"].(map[string]any); ok {
		c.Params = map[string]string{}
		for k, v := range p {
			c.Params[k] = fmt.Sprint(v)
		}
	}
	rid := make([]byte, 16)
	_, _ = rand.Read(rid)
	c.ResourceID = hex.EncodeToString(rid)
	err = s.meta.Update(func(tx Tx) error {
		if _, ok, gerr := s.getBucket(tx, bucket); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		now := s.now()
		for _, o := range channelsOf(tx, bucket) {
			if o.expired(now) {
				removeChannel(tx, o)
			} else if o.ID == c.ID {
				return badRequest("Channel id %s is not unique on bucket %s.", c.ID, bucket)
			}
		}
		return queueChannelMessage(tx, &c, "sync", nil, now)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	s.wakeChannels()
	writeResponse(w, r, http.StatusOK, s.channelJSON(c))
}

func (s *Server) channelsStop(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	id, _ := body["id"].(string)
	rid, _ := body["resourceId"].(string)
	if id == "" {
		writeError(w, required("resource.id"))
		return
	}
	if rid == "" {
		writeError(w, required("resource.resourceId"))
		return
	}
	err = s.meta.Update(func(tx Tx) error {
		for _, k := range tx.List(channelPrefix) {
			if !strings.HasSuffix(k, "/"+rid) {
				continue
			}
			raw, _ := tx.Get(k)
			var c channelRecord
			if json.Unmarshal(raw, &c) == nil && c.ID == id && c.ResourceID == rid {
				removeChannel(tx, c)
				return nil
			}
		}
		return notFound("Channel %s with resource id %s not found.", id, rid)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveChannels is GET ChannelsPath?bucket=: the bucket's live channels, as
// watchAll returned them, with the next message number. It is not a Cloud
// Storage method; the console reads it.
func (s *Server) serveChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errorf(http.StatusMethodNotAllowed, "methodNotAllowed", "GET only"))
		return
	}
	bucket := r.URL.Query().Get("bucket")
	var cs []channelRecord
	_ = s.meta.View(func(tx Tx) error { cs = channelsOf(tx, bucket); return nil })
	items := []any{}
	now := s.now()
	for _, c := range cs {
		if c.expired(now) {
			continue
		}
		j := s.channelJSON(c)
		j["messagesQueued"] = c.Next
		j["created"] = c.Created.UTC().Format(time.RFC3339)
		items = append(items, j)
	}
	writeResponse(w, r, http.StatusOK, map[string]any{"items": items})
}

// channelDispatch is the channel deliverer's state.
type channelDispatch struct {
	once      sync.Once
	wake      chan struct{}
	client    *http.Client
	mu        sync.Mutex
	delivered int
	dropped   int
}

func (s *Server) channelState() *channelDispatch {
	s.chans.once.Do(func() {
		s.chans.wake = make(chan struct{}, 1)
		s.chans.client = LocalChannelClient()
	})
	return &s.chans
}

func (s *Server) wakeChannels() {
	select {
	case s.channelState().wake <- struct{}{}:
	default:
	}
}

// ChannelStats reports how many channel messages were delivered and how many
// were given up on.
func (s *Server) ChannelStats() (delivered, dropped int) {
	d := s.channelState()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.delivered, d.dropped
}

// DispatchChannels tries every pending channel message once, in order; a
// channel whose message fails gets no later one this round, so each channel
// sees its messages in number order. It returns how many remain.
func (s *Server) DispatchChannels(ctx context.Context) int {
	d := s.channelState()
	var keys []string
	_ = s.meta.View(func(tx Tx) error { keys = tx.List(channelOutPrefix); return nil })
	blocked := map[string]bool{}
	remaining := 0
	for _, k := range keys {
		var m channelMessage
		var c channelRecord
		live := false
		_ = s.meta.View(func(tx Tx) error {
			raw, ok := tx.Get(k)
			if !ok || json.Unmarshal(raw, &m) != nil {
				return nil
			}
			craw, ok := tx.Get(m.Channel)
			live = ok && json.Unmarshal(craw, &c) == nil
			return nil
		})
		if m.Channel == "" {
			continue
		}
		if !live || c.expired(s.now()) {
			_ = s.meta.Update(func(tx Tx) error {
				if live {
					removeChannel(tx, c)
				}
				tx.Delete(k)
				return nil
			})
			continue
		}
		if blocked[m.Channel] {
			remaining++
			continue
		}
		err := s.deliver(ctx, d.client, c, m)
		_ = s.meta.Update(func(tx Tx) error {
			if err == nil {
				tx.Delete(k)
				return nil
			}
			m.Attempts++
			if m.Attempts >= maxChannelAttempts {
				tx.Delete(k)
				return nil
			}
			raw, _ := json.Marshal(m)
			tx.Put(k, raw)
			return nil
		})
		d.mu.Lock()
		switch {
		case err == nil:
			d.delivered++
		case m.Attempts >= maxChannelAttempts:
			d.dropped++
		default:
			remaining++
			blocked[m.Channel] = true
		}
		d.mu.Unlock()
	}
	return remaining
}

// ChannelDelivery is one channel message as it is POSTed: the channel's
// address, the X-Goog-* headers and the body (the object resource, or none
// for the sync message). Key names it at ChannelOutboxPath.
type ChannelDelivery struct {
	Key     string      `json:"key"`
	Address string      `json:"address"`
	Header  http.Header `json:"header"`
	Body    []byte      `json:"body,omitempty"`
}

// delivery builds m's POST to c.
func (s *Server) delivery(key string, c channelRecord, m channelMessage) ChannelDelivery {
	var body []byte
	if m.Object != nil {
		body, _ = json.Marshal(s.objectJSONWith(nil, *m.Object, bucketRecord{}))
	}
	h := http.Header{}
	h.Set("X-Goog-Channel-ID", c.ID)
	if c.Token != "" {
		h.Set("X-Goog-Channel-Token", c.Token)
	}
	if c.Expiration != 0 {
		h.Set("X-Goog-Channel-Expiration", time.UnixMilli(c.Expiration).UTC().Format(http.TimeFormat))
	}
	h.Set("X-Goog-Resource-ID", c.ResourceID)
	h.Set("X-Goog-Resource-URI", c.resourceURI())
	h.Set("X-Goog-Resource-State", m.State)
	h.Set("X-Goog-Message-Number", strconv.FormatInt(m.Number, 10))
	if body != nil {
		h.Set("Content-Type", "application/json; charset=UTF-8")
	}
	return ChannelDelivery{Key: key, Address: c.Address, Header: h, Body: body}
}

// deliver POSTs one message.
func (s *Server) deliver(ctx context.Context, client *http.Client, c channelRecord, m channelMessage) error {
	return PostChannelDelivery(ctx, client, s.delivery("", c, m))
}

// PostChannelDelivery POSTs d with client. Google's documented success
// codes for a notification receiver are 102, 200, 201, 202 and 204; any
// other answer is a failed delivery.
func PostChannelDelivery(ctx context.Context, client *http.Client, d ChannelDelivery) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Address, bytes.NewReader(d.Body))
	if err != nil {
		return err
	}
	for k, v := range d.Header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case 102, 200, 201, 202, 204:
		return nil
	}
	return fmt.Errorf("%s answered %d", d.Address, resp.StatusCode)
}

// LocalChannelClient is the HTTP client channel messages are POSTed with
// (#1290). CloudBurrow never calls outside the developer's machine, so it
// connects only to a loopback, private (RFC 1918, RFC 4193) or link-local
// address, checked on the address it dials, after name resolution, so a
// name that resolves elsewhere is refused too. It takes no proxy from the
// environment, follows no redirect, and gives up after 10 seconds.
func LocalChannelClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !LocalChannelAddress(ip) {
			return fmt.Errorf("%s is not a local address: CloudBurrow delivers channel messages only to loopback, private and link-local addresses", host)
		}
		return nil
	}}
	return &http.Client{Timeout: 10 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 5 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// LocalChannelAddress reports whether a channel message may be sent to ip.
func LocalChannelAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// ChannelOutboxPath is where a relay takes channel messages (#1290), when
// the server runs with Options.RelayChannels:
//
//   - GET ?wait=N (seconds, at most 25) answers {"deliveries": [...]}, the
//     next due message of every channel that has one, waiting up to N
//     seconds for one;
//   - POST {"key": k, "error": e} reports the delivery of k: an empty error
//     is a success and removes the message; any other is a failed attempt,
//     retried with backoff up to maxChannelAttempts.
//
// A channel's next message is offered only once the one before it is
// delivered or given up on, so each channel sees its messages in order, as
// when the server POSTs them itself.
const ChannelOutboxPath = "/_cloudburrow/channels/outbox"

// channelRetry is how long a relayed message waits after its nth failure.
func channelRetry(attempts int) time.Duration {
	d := time.Second << min(attempts, 5)
	return min(d, 30*time.Second)
}

// channelOutbox returns the next due message of every channel, removing
// expired channels' and orphaned messages on the way.
func (s *Server) channelOutbox() []ChannelDelivery {
	var keys []string
	_ = s.meta.View(func(tx Tx) error { keys = tx.List(channelOutPrefix); return nil })
	seen := map[string]bool{}
	out := []ChannelDelivery{}
	now := s.now()
	for _, k := range keys {
		var m channelMessage
		var c channelRecord
		live := false
		_ = s.meta.View(func(tx Tx) error {
			raw, ok := tx.Get(k)
			if !ok || json.Unmarshal(raw, &m) != nil {
				return nil
			}
			craw, ok := tx.Get(m.Channel)
			live = ok && json.Unmarshal(craw, &c) == nil
			return nil
		})
		if m.Channel == "" {
			continue
		}
		if !live || c.expired(now) {
			_ = s.meta.Update(func(tx Tx) error {
				if live {
					removeChannel(tx, c)
				}
				tx.Delete(k)
				return nil
			})
			continue
		}
		if seen[m.Channel] {
			continue
		}
		seen[m.Channel] = true
		if m.Due.After(now) {
			continue
		}
		out = append(out, s.delivery(k, c, m))
	}
	return out
}

// channelReported applies a relay's report of the delivery of key.
func (s *Server) channelReported(key, failure string) {
	d := s.channelState()
	outcome := 0 // 1 delivered, -1 dropped
	_ = s.meta.Update(func(tx Tx) error {
		raw, ok := tx.Get(key)
		var m channelMessage
		if !ok || json.Unmarshal(raw, &m) != nil {
			return nil
		}
		if failure == "" {
			tx.Delete(key)
			outcome = 1
			return nil
		}
		m.Attempts++
		if m.Attempts >= maxChannelAttempts {
			tx.Delete(key)
			outcome = -1
			return nil
		}
		m.Due = s.now().Add(channelRetry(m.Attempts))
		raw, _ = json.Marshal(m)
		tx.Put(key, raw)
		return nil
	})
	d.mu.Lock()
	switch outcome {
	case 1:
		d.delivered++
	case -1:
		d.dropped++
	}
	d.mu.Unlock()
}

func (s *Server) serveChannelOutbox(w http.ResponseWriter, r *http.Request) {
	if !s.relayChannels {
		writeError(w, errorf(http.StatusConflict, "conflict", "this server POSTs channel messages itself; it relays them only when started to"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		deadline := time.Now().Add(time.Duration(min(max(wait, 0), 25)) * time.Second)
		d := s.channelState()
		for {
			out := s.channelOutbox()
			if len(out) > 0 || !time.Now().Before(deadline) {
				writeResponse(w, r, http.StatusOK, map[string]any{"deliveries": out})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-d.wake:
			case <-time.After(time.Second):
			}
		}
	case http.MethodPost:
		var rep struct {
			Key   string `json:"key"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&rep); err != nil || !strings.HasPrefix(rep.Key, channelOutPrefix) {
			writeError(w, badRequest("want {\"key\": a delivery's key, \"error\": \"\" or why it failed}"))
			return
		}
		s.channelReported(rep.Key, rep.Error)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, errorf(http.StatusMethodNotAllowed, "methodNotAllowed", "GET or POST only"))
	}
}

// runChannels delivers when woken, and with backoff while messages are left
// over from a failed delivery.
func (s *Server) runChannels(ctx context.Context) {
	d := s.channelState()
	backoff := time.Second
	for {
		left := s.DispatchChannels(ctx)
		wait := time.Minute
		if left > 0 {
			wait = backoff
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		} else {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-time.After(wait):
		}
	}
}
