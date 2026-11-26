package mailsink

import (
	"context"
	"net/smtp"
	"strings"
	"testing"
)

func TestSinkKeepsWhatSMTPClientsSend(t *testing.T) {
	s := New()
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Listen("0.0.0.0:0"); err == nil {
		t.Fatal("the sink listened on every interface")
	}
	// A plain net/smtp client.
	msg := "From: app@example.com\r\nTo: Ops <ops@example.com>\r\nSubject: =?utf-8?q?caf=C3=A9?=\r\n\r\nline one\r\n.leading dot\r\n"
	if err := smtp.SendMail(s.Addr(), nil, "app@example.com", []string{"ops@example.com", "b@example.com"}, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	// Send, the services' path.
	if err := s.Send(context.Background(), "monitoring", "alerts-noreply@cloudburrow.localhost", []string{"x@example.com"}, "Incident ✓",
		strings.Repeat("a long line ", 20)+"\nsecond line"); err != nil {
		t.Fatal(err)
	}
	got := s.Messages()
	if len(got) != 2 {
		t.Fatalf("kept %d messages", len(got))
	}
	m := got[1]
	if m.Subject != "café" || m.Envelope.From != "app@example.com" || len(m.Envelope.To) != 2 || m.To[0] != "ops@example.com" ||
		!strings.Contains(m.Text, "\n.leading dot") || m.Source != "smtp" {
		t.Fatalf("message = %+v", m)
	}
	n := got[0]
	if n.Subject != "Incident ✓" || n.Source != "monitoring" || !strings.Contains(n.Text, strings.Repeat("a long line ", 20)) || !strings.Contains(n.Text, "second line") {
		t.Fatalf("sent message = %+v %q", n, n.Text)
	}
	if _, ok := s.Get(n.ID); !ok || !s.Delete(n.ID) || len(s.Messages()) != 1 {
		t.Fatal("get or delete failed")
	}
	s.Delete("")
	if len(s.Messages()) != 0 {
		t.Fatal("delete all left messages")
	}
	if err := s.Send(context.Background(), "x", "a@b.c", []string{"bad\r\nRCPT"}, "s", "t"); err == nil {
		t.Fatal("an address with a line break was sent")
	}
}

func TestSinkRefusesOutOfOrderCommands(t *testing.T) {
	s := New()
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := smtp.Dial(s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Rcpt("a@example.com"); err == nil {
		t.Fatal("RCPT before MAIL was accepted")
	}
	if _, err := c.Data(); err == nil {
		t.Fatal("DATA before MAIL was accepted")
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS is offered")
	}
}
