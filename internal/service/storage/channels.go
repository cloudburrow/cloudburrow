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

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
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
		s.chans.client = &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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

// deliver POSTs one message. Google's documented success codes for a
// notification receiver are 102, 200, 201, 202 and 204.
func (s *Server) deliver(ctx context.Context, client *http.Client, c channelRecord, m channelMessage) error {
	var body []byte
	if m.Object != nil {
		body, _ = json.Marshal(s.objectJSONWith(nil, *m.Object, bucketRecord{}))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Address, bytes.NewReader(body))
	if err != nil {
		return err
	}
	h := req.Header
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
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case 102, 200, 201, 202, 204:
		return nil
	}
	return fmt.Errorf("%s answered %d", c.Address, resp.StatusCode)
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
