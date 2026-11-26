package console

// The Mail page and the Instance page's clocks (#2214), both CloudBurrow's
// own, in its Management tools rather than among Google's pages.
//
// Mail lists what the local mail sink (internal/mailsink) received: the
// emails of Cloud Monitoring's email channels, Model Monitoring's email
// alerts, and anything an application on the machine sent to its SMTP
// address. Nothing in it ever left the machine.
//
// Clocks are the clock:advance calls CloudBurrow adds to services whose
// runs are hours apart, which the UI parity rule wants reachable from the
// console: each moves its service's clock forward, never back.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/mailsink"
)

// MailSource is the local mail sink.
type MailSource interface {
	Addr() string
	Messages() []mailsink.Message
	Get(id string) (mailsink.Message, bool)
	Delete(id string) bool
}

// SetMail attaches the Mail page's sink.
func (s *Server) SetMail(m MailSource) { s.mail = m }

// Clock is one service clock the Instance page can advance.
type Clock struct {
	ID, Title, Help string
	// Advance moves the clock d forward and returns its new time.
	Advance func(d time.Duration) (time.Time, error)
}

// SetClocks attaches the Instance page's clocks.
func (s *Server) SetClocks(c []Clock) { s.clocks = c }

// mailSummary is a message as the list shows it.
type mailSummary struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	From    string    `json:"from"`
	To      []string  `json:"to"`
	Subject string    `json:"subject"`
	Source  string    `json:"source"`
	Size    int       `json:"size"`
}

func (s *Server) mailWired(w http.ResponseWriter) bool {
	if s.mail == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the local mail sink is not running on this instance"})
		return false
	}
	return true
}

// handleMail is GET /api/mail: the SMTP address and the messages, newest
// first.
func (s *Server) handleMail(w http.ResponseWriter, r *http.Request) {
	if !s.mailWired(w) {
		return
	}
	msgs := s.mail.Messages()
	out := make([]mailSummary, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, mailSummary{ID: m.ID, Time: m.Time, From: m.From, To: m.To, Subject: m.Subject, Source: m.Source, Size: m.Size})
	}
	writeJSON(w, http.StatusOK, map[string]any{"smtp": s.mail.Addr(), "messages": out, "limit": mailsink.MaxMessages})
}

// handleMailMessage is GET /api/mail/{id}: one message with its text and
// its raw source.
func (s *Server) handleMailMessage(w http.ResponseWriter, r *http.Request) {
	if !s.mailWired(w) {
		return
	}
	m, ok := s.mail.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such message: it was deleted, or pushed out by newer ones"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": m.ID, "time": m.Time, "from": m.From, "to": m.To, "subject": m.Subject,
		"source": m.Source, "size": m.Size, "text": m.Text, "envelope": m.Envelope, "raw": string(m.Raw)})
}

// handleMailDelete is DELETE /api/mail?id=, or ?all=true for every
// message.
func (s *Server) handleMailDelete(w http.ResponseWriter, r *http.Request) {
	if !s.mailWired(w) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" && r.URL.Query().Get("all") != "true" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name a message with ?id=, or delete every message with ?all=true"})
		return
	}
	if !s.mail.Delete(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such message"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleClocks is GET /api/clocks.
func (s *Server) handleClocks(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]string, 0, len(s.clocks))
	for _, c := range s.clocks {
		out = append(out, map[string]string{"id": c.ID, "title": c.Title, "help": c.Help})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clocks": out})
}

// handleClockAdvance is POST /api/clocks/{id} {"duration": "1h"}.
func (s *Server) handleClockAdvance(w http.ResponseWriter, r *http.Request) {
	var c *Clock
	for i := range s.clocks {
		if s.clocks[i].ID == r.PathValue("id") {
			c = &s.clocks[i]
		}
	}
	if c == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such clock on this instance"})
		return
	}
	var body struct {
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body is not JSON: " + err.Error()})
		return
	}
	d, err := ParseAdvance(body.Duration)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	now, err := c.Advance(d)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"now": now.UTC().Format(time.RFC3339), "advancedBy": d.String(),
		"ahead": time.Until(now).Round(time.Second).String()})
}

var advanceRE = regexp.MustCompile(`^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// MaxAdvance bounds one advance: a year.
const MaxAdvance = 366 * 24 * time.Hour

// ParseAdvance reads days, hours, minutes and seconds, such as 1h, 7d or
// 1d12h30m; the clock only moves forward, so it is at least a second.
func ParseAdvance(v string) (time.Duration, error) {
	v = strings.ReplaceAll(strings.TrimSpace(v), " ", "")
	m := advanceRE.FindStringSubmatch(v)
	if v == "" || m == nil {
		return 0, fmt.Errorf("%q is not a duration such as 1h, 7d or 1d12h30m", v)
	}
	var d time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > int64(MaxAdvance/unit) {
			return 0, fmt.Errorf("%q is more than a year", v)
		}
		d += time.Duration(n) * unit
	}
	if d < time.Second {
		return 0, fmt.Errorf("%q does not move the clock: give at least 1s", v)
	}
	if d > MaxAdvance {
		return 0, fmt.Errorf("%q is more than a year", v)
	}
	return d, nil
}
