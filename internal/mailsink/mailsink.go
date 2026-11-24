// Package mailsink is CloudBurrow's local mail sink (#2214): a minimal
// SMTP server on the loopback interface that keeps every message it is
// given, for the console's Mail page, and never relays one. The email
// notifications of Cloud Monitoring's email channels and of Agent
// Platform Model Monitoring are sent to it over SMTP, as Google sends
// them to an inbox; an application on the machine may send to it too.
//
// It speaks the subset of SMTP (RFC 5321) a client needs to hand over a
// message: EHLO or HELO, MAIL FROM, RCPT TO, DATA, RSET, NOOP, VRFY and
// QUIT. It offers no STARTTLS and no AUTH, accepts any sender and any
// recipient, and keeps at most MaxMessages messages, in memory, the oldest
// dropped first: like Cloud Monitoring's deliveries, they do not survive
// a restart.
package mailsink

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"time"
)

// Limits.
const (
	// MaxMessages bounds the messages kept.
	MaxMessages = 500
	// MaxMessageBytes bounds one message's DATA (the SIZE extension
	// advertises it).
	MaxMessageBytes = 10 << 20
	// MaxRecipients bounds one message's RCPT TO commands, RFC 5321's
	// minimum a server must accept.
	MaxRecipients = 100
	// idleTimeout closes a connection that sends nothing for this long.
	idleTimeout = 5 * time.Minute
)

// Message is one message the sink received.
type Message struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	From     string    `json:"from"`
	To       []string  `json:"to"`
	Subject  string    `json:"subject"`
	Text     string    `json:"text"`
	Size     int       `json:"size"`
	Envelope struct {
		From string   `json:"from"`
		To   []string `json:"to"`
	} `json:"envelope"`
	// Source is what sent it: the X-CloudBurrow-Source header of the
	// services' own messages ("monitoring", "model-monitoring"), or "smtp".
	Source string `json:"source"`
	Raw    []byte `json:"-"`
}

// Sink is the mail sink.
type Sink struct {
	mu    sync.Mutex
	msgs  []Message
	ln    net.Listener
	conns sync.WaitGroup
	host  string
}

// New returns a sink that is not yet listening.
func New() *Sink { return &Sink{host: "cloudburrow.localhost"} }

// Listen serves SMTP on addr, which must be a loopback address (port 0
// picks one).
func (s *Sink) Listen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("the mail sink listens on the loopback interface only, not %s", host)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	go s.serve(ln)
	return nil
}

// Addr is the SMTP address, host:port, or "" before Listen.
func (s *Sink) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close stops listening and waits for open connections to end.
func (s *Sink) Close() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return nil
	}
	err := ln.Close()
	s.conns.Wait()
	return err
}

func (s *Sink) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.conns.Add(1)
		go func() {
			defer s.conns.Done()
			s.session(c)
		}()
	}
}

// session is one SMTP connection.
func (s *Sink) session(c net.Conn) {
	defer c.Close()
	tp := textproto.NewConn(c)
	reply := func(code int, msg string) bool {
		_ = c.SetWriteDeadline(time.Now().Add(idleTimeout))
		return tp.PrintfLine("%d %s", code, msg) == nil
	}
	if !reply(220, s.host+" ESMTP CloudBurrow mail sink: messages are kept for the console, never relayed") {
		return
	}
	var from string
	var to []string
	inMail := false
	for {
		_ = c.SetReadDeadline(time.Now().Add(idleTimeout))
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			_ = c.SetWriteDeadline(time.Now().Add(idleTimeout))
			if tp.PrintfLine("250-%s", s.host) != nil || tp.PrintfLine("250-8BITMIME") != nil ||
				tp.PrintfLine("250-SIZE %d", MaxMessageBytes) != nil || tp.PrintfLine("250 SMTPUTF8") != nil {
				return
			}
			from, to, inMail = "", nil, false
		case "HELO":
			reply(250, s.host)
			from, to, inMail = "", nil, false
		case "MAIL":
			addr, ok := pathArg(arg, "FROM:")
			switch {
			case !ok:
				reply(501, "syntax: MAIL FROM:<address>")
			case inMail:
				reply(503, "nested MAIL command")
			default:
				from, to, inMail = addr, nil, true
				reply(250, "OK")
			}
		case "RCPT":
			addr, ok := pathArg(arg, "TO:")
			switch {
			case !inMail:
				reply(503, "need MAIL before RCPT")
			case !ok || addr == "":
				reply(501, "syntax: RCPT TO:<address>")
			case len(to) >= MaxRecipients:
				reply(452, "too many recipients")
			default:
				to = append(to, addr)
				reply(250, "OK")
			}
		case "DATA":
			if !inMail || len(to) == 0 {
				reply(503, "need MAIL and RCPT before DATA")
				continue
			}
			if !reply(354, "end data with <CR><LF>.<CR><LF>") {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(idleTimeout))
			raw, err := io.ReadAll(io.LimitReader(tp.DotReader(), MaxMessageBytes+1))
			if err != nil {
				return
			}
			if len(raw) > MaxMessageBytes {
				// The rest of DATA is read and dropped, so the session
				// stays in step.
				_, _ = io.Copy(io.Discard, tp.DotReader())
				reply(552, fmt.Sprintf("message exceeds %d bytes", MaxMessageBytes))
			} else {
				m := s.keep(from, to, raw)
				reply(250, "OK: queued as "+m.ID)
			}
			from, to, inMail = "", nil, false
		case "RSET":
			from, to, inMail = "", nil, false
			reply(250, "OK")
		case "NOOP":
			reply(250, "OK")
		case "VRFY":
			reply(252, "cannot verify the user, but will accept the message")
		case "QUIT":
			reply(221, "bye")
			return
		default:
			reply(502, "command not implemented")
		}
	}
}

// pathArg reads "FROM:<a@b>" (with optional parameters after it).
func pathArg(arg, prefix string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	if !strings.HasPrefix(rest, "<") {
		return "", false
	}
	end := strings.Index(rest, ">")
	if end < 0 {
		return "", false
	}
	return rest[1:end], true
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// keep parses and stores one message.
func (s *Sink) keep(from string, to []string, raw []byte) Message {
	m := Message{ID: newID(), Time: time.Now().UTC(), Size: len(raw), Raw: raw, Source: "smtp"}
	m.Envelope.From, m.Envelope.To = from, append([]string(nil), to...)
	m.From, m.To = from, append([]string(nil), to...)
	if msg, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		dec := new(mime.WordDecoder)
		if v := msg.Header.Get("Subject"); v != "" {
			if d, err := dec.DecodeHeader(v); err == nil {
				m.Subject = d
			} else {
				m.Subject = v
			}
		}
		if v := msg.Header.Get("From"); v != "" {
			m.From = v
		}
		if v := msg.Header.Get("To"); v != "" {
			if l, err := msg.Header.AddressList("To"); err == nil {
				m.To = nil
				for _, a := range l {
					m.To = append(m.To, a.Address)
				}
			} else {
				m.To = []string{v}
			}
		}
		if v := msg.Header.Get("X-CloudBurrow-Source"); v != "" {
			m.Source = v
		}
		m.Text = textOf(textproto.MIMEHeader(msg.Header), msg.Body)
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	if len(s.msgs) > MaxMessages {
		s.msgs = append([]Message(nil), s.msgs[len(s.msgs)-MaxMessages:]...)
	}
	s.mu.Unlock()
	return m
}

// textOf is a body's text: a text/plain part of a multipart message, or
// the body itself, its transfer encoding undone.
func textOf(h textproto.MIMEHeader, body io.Reader) string {
	ct, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		ct = "text/plain"
	}
	if strings.HasPrefix(ct, "multipart/") && params["boundary"] != "" {
		r := multipart.NewReader(body, params["boundary"])
		first := ""
		for {
			p, err := r.NextRawPart()
			if err != nil {
				return first
			}
			t := textOf(p.Header, p)
			if pct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type")); pct == "text/plain" || pct == "" {
				return t
			}
			if first == "" {
				first = t
			}
		}
	}
	var rd io.Reader = body
	switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
	case "quoted-printable":
		rd = quotedprintable.NewReader(body)
	case "base64":
		b, _ := io.ReadAll(body)
		if d, err := decodeBase64(b); err == nil {
			return string(d)
		}
		return string(b)
	}
	b, _ := io.ReadAll(io.LimitReader(rd, MaxMessageBytes))
	return string(b)
}

// Messages are the kept messages, newest first.
func (s *Sink) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, len(s.msgs))
	for i := len(s.msgs) - 1; i >= 0; i-- {
		out = append(out, s.msgs[i])
	}
	return out
}

// Get is one kept message.
func (s *Sink) Get(id string) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if m.ID == id {
			return m, true
		}
	}
	return Message{}, false
}

// Delete removes one message, or every message when id is "".
func (s *Sink) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		s.msgs = nil
		return true
	}
	for i, m := range s.msgs {
		if m.ID == id {
			s.msgs = append(s.msgs[:i:i], s.msgs[i+1:]...)
			return true
		}
	}
	return false
}

// ErrNotListening is Send's error before Listen.
var ErrNotListening = errors.New("the mail sink is not listening")

// Send hands a plain-text message to the sink over SMTP, as a mail
// client would: the notifications of Cloud Monitoring and Model
// Monitoring go this way. source is the message's X-CloudBurrow-Source.
func (s *Sink) Send(ctx context.Context, source, from string, to []string, subject, text string) error {
	addr := s.Addr()
	if addr == "" {
		return ErrNotListening
	}
	if len(to) == 0 {
		return errors.New("no recipient")
	}
	for _, a := range append([]string{from}, to...) {
		if strings.ContainsAny(a, "\r\n<>") {
			return fmt.Errorf("%q is not an email address", a)
		}
	}
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	fmt.Fprintf(w, "From: %s\r\n", from)
	fmt.Fprintf(w, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(w, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(w, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(w, "Message-ID: <%s@%s>\r\n", newID(), s.host)
	fmt.Fprintf(w, "X-CloudBurrow-Source: %s\r\n", strings.NewReplacer("\r", "", "\n", "").Replace(source))
	fmt.Fprintf(w, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(w)
	_, _ = qp.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	_ = qp.Close()
	_ = w.Flush()

	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	host, _, _ := net.SplitHostPort(addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, a := range to {
		if err := c.Rcpt(a); err != nil {
			return err
		}
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(b.Bytes()); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// decodeBase64 decodes a base64 body, ignoring its line breaks.
func decodeBase64(b []byte) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(b))
	return base64.StdEncoding.DecodeString(clean)
}
