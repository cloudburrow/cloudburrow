//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestMailPageAndTheAgentPlatformClock (#2214): a message sent to the
// local mail sink's SMTP address, which the Mail page names, is listed on
// the page, and opening it shows its headers and text; Delete removes it.
// The Instance page's Clocks card, offered with or without the admin
// token, advances the Agent Platform clock and says where it now is; a
// duration it cannot read is refused on the page. Both pages pass the
// WCAG audit (names, contrast, keyboard, motion, reflow).
func TestMailPageAndTheAgentPlatformClock(t *testing.T) {
	code, body := consoleDo(t, http.MethodGet, "/api/mail", "")
	if code != http.StatusOK {
		t.Skipf("the console has no local mail sink (%d: %s)", code, body)
	}
	var mail struct{ SMTP string }
	if err := json.Unmarshal([]byte(body), &mail); err != nil || mail.SMTP == "" {
		t.Fatalf("GET /api/mail = %s", body)
	}
	if host, _, err := net.SplitHostPort(mail.SMTP); err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("the SMTP address %q is not on the loopback interface", mail.SMTP)
	}
	stamp := time.Now().UnixNano() % 1e6
	subject := fmt.Sprintf("Browser test %d", stamp)
	msg := "From: app@example.com\r\nTo: ops@example.com\r\nSubject: " + subject + "\r\n\r\nThe nightly export finished.\r\nRows: 42\r\n"
	if err := smtp.SendMail(mail.SMTP, nil, "app@example.com", []string{"ops@example.com"}, []byte(msg)); err != nil {
		t.Fatalf("send to the mail sink: %v", err)
	}

	p := open(t)
	p.navigate("/mail")
	listed := fmt.Sprintf(`[...document.querySelectorAll("#mail-list tbody tr")].some((r) => r.textContent.includes(%q))`, subject)
	p.waitFor(listed)
	var addr string
	p.eval(`document.querySelector("#mail-smtp-address").textContent`, &addr)
	if addr != mail.SMTP {
		t.Errorf("the page names the SMTP address %q, want %q", addr, mail.SMTP)
	}
	p.clickText("#mail-list tbody a", subject)
	p.waitFor(`(document.querySelector("#mail-message-text") || {}).textContent === "The nightly export finished.\nRows: 42\n"`)
	var headers string
	p.eval(`document.querySelector("#mail-message-headers").textContent`, &headers)
	if !strings.Contains(headers, "app@example.com") || !strings.Contains(headers, "ops@example.com") || !strings.Contains(headers, "smtp") {
		t.Errorf("the message's headers = %q", headers)
	}
	var id string
	p.eval(`location.hash.slice(1)`, &id)
	if id == "" {
		t.Fatal("opening the message left no #id in the address")
	}

	clocks := false
	if code, body := consoleDo(t, http.MethodGet, "/api/clocks", ""); code == http.StatusOK && strings.Contains(body, `"agent-platform"`) {
		clocks = true
	}
	pages := []a11yPage{
		{"mail", "/mail", listed, ""},
		{"mail message", "/mail#" + id, `document.querySelector("#mail-message-text") !== null`, ""},
	}
	if clocks {
		pages = append(pages, a11yPage{"instance clocks", "/instance#clocks", `document.querySelector("#instance-clock-agent-platform-advance") !== null`, ""})
	}
	auditScreens(t, pages)

	if clocks {
		p = open(t)
		p.navigate("/instance#clocks")
		p.waitFor(`document.querySelector("#instance-clock-agent-platform-advance") !== null`)
		// The token form's first read is refused with 401, on purpose.
		p.forgive()
		set := func(v string) {
			p.run(chromedp.SetValue(chromedp.CSS(`#instance-clock-agent-platform-duration`), v))
			p.run(chromedp.Click(chromedp.CSS(`#instance-clock-agent-platform-advance`)))
		}
		set("soon")
		p.waitFor(`!document.querySelector("#instance-clock-agent-platform-error").hidden`)
		// The refusal is a 400, on purpose; Chrome logs it as an error.
		p.forgive()
		var refusal string
		p.eval(`document.querySelector("#instance-clock-agent-platform-error").textContent`, &refusal)
		if !strings.Contains(refusal, "not a duration") {
			t.Errorf("a bad duration said %q", refusal)
		}
		set("2h")
		p.waitFor(`document.querySelector("#instance-clock-agent-platform-result").textContent.includes("ahead of this machine's")`)
		var result string
		p.eval(`document.querySelector("#instance-clock-agent-platform-result").textContent`, &result)
		if !strings.Contains(result, "Agent Platform clock is now") {
			t.Errorf("the clock's result = %q", result)
		}
	} else {
		t.Log("the local AI endpoint is not running here: the Agent Platform clock is not offered")
	}

	// Delete removes the message.
	p = open(t)
	p.navigate("/mail#" + id)
	p.waitFor(`document.querySelector("#mail-delete") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#mail-delete`)))
	p.waitFor(`document.querySelector("#mail-list, #mail-empty") !== null && !` + listed)
	if code, _ := consoleDo(t, http.MethodGet, "/api/mail/"+id, ""); code != http.StatusNotFound {
		t.Errorf("after Delete the message answers %d", code)
	}
}
