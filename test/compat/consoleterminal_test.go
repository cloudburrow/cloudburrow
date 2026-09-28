//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// terminalPrepare bounds the first connection, which waits for the
// terminal pod: its first start pulls the Cloud SDK image (about 1 GB).
const terminalPrepare = 15 * time.Minute

// consoleTerminal is one shell in the console's terminal, driven through
// the same WebSocket the drawer uses.
type consoleTerminal struct {
	t   *testing.T
	ws  *websocket.Conn
	out strings.Builder
}

func openConsoleTerminal(t *testing.T, addr, project string) *consoleTerminal {
	t.Helper()
	cfg, err := websocket.NewConfig("ws://"+addr+"/api/terminal/socket?cols=200&rows=50&project="+project, "http://"+addr)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("open the console terminal: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	c := &consoleTerminal{t: t, ws: ws}
	// Until the session message, every control frame is progress or the
	// reason there is no shell.
	_ = ws.SetReadDeadline(time.Now().Add(terminalPrepare))
	for {
		m, text, ok := c.frame()
		if !ok {
			continue
		}
		switch m.Type {
		case "status":
			t.Logf("terminal: %s", m.Message)
		case "session":
			if m.Project != project {
				t.Fatalf("the session is in project %q, want %q", m.Project, project)
			}
			return c
		default:
			t.Fatalf("the terminal did not open: %s", text)
		}
	}
}

type termMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Project string `json:"project"`
}

// frame reads one frame: output is appended to c.out, and a control frame
// is returned.
func (c *consoleTerminal) frame() (termMessage, string, bool) {
	c.t.Helper()
	var f termFrame
	if err := termCodec.Receive(c.ws, &f); err != nil {
		c.t.Fatalf("read the terminal: %v\noutput so far:\n%s", err, c.out.String())
	}
	var m termMessage
	if f.text {
		if err := json.Unmarshal(f.data, &m); err != nil {
			c.t.Fatalf("a control frame is not JSON: %q", f.data)
		}
		return m, string(f.data), true
	}
	// Colour and cursor sequences are dropped, so what is matched is the
	// text a person reads.
	c.out.WriteString(ansi.ReplaceAllString(string(f.data), ""))
	return m, "", false
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07`)

// termFrame is a frame and whether it was text: the console sends control
// messages as text frames and terminal output as binary ones.
type termFrame struct {
	text bool
	data []byte
}

var termCodec = websocket.Codec{
	Unmarshal: func(data []byte, payloadType byte, v any) error {
		f := v.(*termFrame)
		f.text, f.data = payloadType == websocket.TextFrame, append([]byte(nil), data...)
		return nil
	},
}

// until reads until the output matches re, and returns the output since
// the call.
func (c *consoleTerminal) until(re *regexp.Regexp, within time.Duration) string {
	c.t.Helper()
	return c.untilFrom(c.out.Len(), re, within)
}

// untilFrom is until, matching from an earlier point in the output.
func (c *consoleTerminal) untilFrom(start int, re *regexp.Regexp, within time.Duration) string {
	c.t.Helper()
	_ = c.ws.SetReadDeadline(time.Now().Add(within))
	for !re.MatchString(c.out.String()[start:]) {
		if m, text, ok := c.frame(); ok && m.Type != "project" {
			c.t.Fatalf("unexpected control frame %s", text)
		}
	}
	return c.out.String()[start:]
}

// run types a command and returns its output and exit status. The marker
// is printed by the command rather than typed, so the echo of the typed
// line cannot match it.
func (c *consoleTerminal) run(cmd string, within time.Duration) (string, int) {
	c.t.Helper()
	if err := websocket.Message.Send(c.ws, []byte(cmd+"; printf '%s_%s_\\n' __CBDONE $?\r")); err != nil {
		c.t.Fatal(err)
	}
	out := c.until(regexp.MustCompile(`__CBDONE_(\d+)_`), within)
	m := regexp.MustCompile(`__CBDONE_(\d+)_`).FindStringSubmatch(out)
	var code int
	fmt.Sscan(m[1], &code)
	c.t.Logf("$ %s\n%s", cmd, out)
	return out, code
}

// TestConsoleTerminalReachesThisInstanceAndNotGoogle (#781): in the
// console's terminal — a shell in a pod in the instance's cluster, reached
// over the console's WebSocket — gcloud storage, gcloud pubsub and kubectl
// answer from this instance with no configuration typed, and the shell
// follows the toolbar's project.
//
// Behind an egress guard: the shell's proxy is set to a closed port on the
// pod's own loopback with only cluster addresses exempt, so a request for
// anywhere else fails. The gcloud commands passing therefore shows they
// reached the instance; a command for a service with no local override is
// checked to fail, which shows the guard was in force.
func TestConsoleTerminalReachesThisInstanceAndNotGoogle(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	bkt := bucket(t, h, sc).BucketName()
	ps := pubsubClient(t, h)
	topicName := topic(t, h, ps, "terminal-topic")

	term := openConsoleTerminal(t, addr, h.Project())
	term.untilFrom(0, regexp.MustCompile(`cloudburrow \(`+regexp.QuoteMeta(h.Project())+`\):`), time.Minute)

	guard := "export HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 " +
		"https_proxy=http://127.0.0.1:9 http_proxy=http://127.0.0.1:9 " +
		"NO_PROXY=.svc.cluster.local,.svc,10.0.0.0/8,localhost,127.0.0.1 " +
		"no_proxy=.svc.cluster.local,.svc,10.0.0.0/8,localhost,127.0.0.1"
	if _, code := term.run(guard, time.Minute); code != 0 {
		t.Fatalf("setting the egress guard exited %d", code)
	}

	if out, code := term.run("gcloud version 2>&1 | head -1; kubectl version --client 2>&1 | head -1", time.Minute); code != 0 ||
		!strings.Contains(out, "Google Cloud SDK") {
		t.Errorf("gcloud version: exit %d", code)
	}
	if out, code := term.run(`test -z "$GOOGLE_APPLICATION_CREDENTIALS" && gcloud config get auth/disable_credentials 2>/dev/null`, time.Minute); code != 0 ||
		!strings.Contains(strings.ToLower(out), "true") {
		t.Errorf("the shell has a credential or credentials enabled: exit %d", code)
	}
	if out, code := term.run("gcloud storage ls", 3*time.Minute); code != 0 || !strings.Contains(out, "gs://"+bkt+"/") {
		t.Errorf("gcloud storage ls: exit %d; want gs://%s/ listed", code, bkt)
	}
	if out, code := term.run("gcloud pubsub topics list --format='value(name)'", 3*time.Minute); code != 0 ||
		!strings.Contains(out, topicName) {
		t.Errorf("gcloud pubsub topics list: exit %d; want %s listed", code, topicName)
	}
	if out, code := term.run("kubectl get pods", 2*time.Minute); code != 0 || !strings.Contains(out, "cloudburrow-terminal") {
		t.Errorf("kubectl get pods: exit %d; want the instance's pods, the terminal's among them", code)
	}
	// A gcloud request for a host outside the cluster meets the guard. The
	// host is under .invalid rather than a Google name, so a guard that was
	// not in force would fail on DNS instead of reaching anyone.
	if out, code := term.run("CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE=http://egress-check.invalid/compute/v1/ "+
		"gcloud compute zones list --quiet >/tmp/egress.log 2>&1; e=$?; echo refused-by-proxy=$(grep -c 'Unable to connect to proxy' /tmp/egress.log); (exit $e)", 3*time.Minute); code == 0 ||
		!regexp.MustCompile(`refused-by-proxy=[1-9]`).MatchString(out) {
		t.Errorf("a request for a host outside the cluster exited %d without meeting the egress guard", code)
	}

	// The toolbar switches project: the console says so, and the shell's
	// project follows from the next prompt.
	other := h.Project() + "-b"
	if err := websocket.JSON.Send(term.ws, map[string]string{"type": "project", "project": other}); err != nil {
		t.Fatal(err)
	}
	_ = term.ws.SetReadDeadline(time.Now().Add(time.Minute))
	for {
		m, text, ok := term.frame()
		if !ok {
			continue
		}
		if m.Type != "project" || m.Project != other || !strings.Contains(m.Message, "switched to "+other) {
			t.Fatalf("switching project answered %s", text)
		}
		break
	}
	// The prompt drawn before the switch has run; the next one picks it up.
	term.run("true", time.Minute)
	if out, _ := term.run(`echo "project=$CLOUDSDK_CORE_PROJECT"`, time.Minute); !strings.Contains(out, "project="+other) {
		t.Errorf("CLOUDSDK_CORE_PROJECT did not follow the toolbar to %s", other)
	}
}
