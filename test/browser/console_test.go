//go:build browser

// Package browser drives the console of a running instance in a real headless
// Chrome (#594), through chromedp and the DevTools protocol, so what it
// asserts is what a browser renders and does rather than what the assets
// contain. It is the automated form of docs/console-verification.md.
//
// It needs a running instance and a Chrome or Chromium on the machine:
//
//	CLOUDBURROW_TEST_CONSOLE      the console, host:port (loopback only)
//	CLOUDBURROW_TEST_CONTROL      the control port, for /admin/faults
//	CLOUDBURROW_TEST_ADMIN_TOKEN  the instance's admin token
//	CLOUDBURROW_TEST_CHROME       optional: the browser binary; otherwise
//	                              chromedp looks for google-chrome, chromium
//	                              or Chrome.app in the usual places
//	CLOUDBURROW_TEST_SCREENSHOTS  optional: where a failing test's screenshot
//	                              is written (default: the temp directory)
//
// The instance needs storage and tasks; CI runs it against the compat job's
// storage shard. Every test skips when CLOUDBURROW_TEST_CONSOLE is unset, and
// CI fails the step unless each one passed.
//
// No request may leave loopback. The browser is started with a proxy that is
// a sinkhole on loopback, so a request for any other host reaches the test
// and nothing else; loopback itself is never proxied. Every request each page
// makes is also read from the Network domain, and a test fails on any that
// is not for a loopback host, and on any error the page logs.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/emulation"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const (
	envConsole     = "CLOUDBURROW_TEST_CONSOLE"
	envControl     = "CLOUDBURROW_TEST_CONTROL"
	envAdminToken  = "CLOUDBURROW_TEST_ADMIN_TOKEN"
	envChrome      = "CLOUDBURROW_TEST_CHROME"
	envNoSandbox   = "CLOUDBURROW_TEST_CHROME_NO_SANDBOX"
	envScreenshots = "CLOUDBURROW_TEST_SCREENSHOTS"
)

// bucketHelp is the Cloud Storage create form's help text, which the console
// shows under the name field when the value does not match its pattern.
const bucketHelp = "Lowercase letters, numbers, hyphens and underscores; 3-63 characters."

// TestConsoleShellLandmarksAndLocalBadge: the dashboard at / exposes the
// banner, navigation and main landmarks to assistive technology, read from
// Chrome's accessibility tree, and the LOCAL badge is rendered in the
// toolbar, so a real console and this one cannot be mistaken for each other.
func TestConsoleShellLandmarksAndLocalBadge(t *testing.T) {
	p := open(t)
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)

	nodes, err := p.axTree()
	if err != nil {
		t.Fatalf("read the accessibility tree: %v", err)
	}
	roles := map[string]bool{}
	for _, n := range nodes {
		if n.Ignored || n.Role == nil {
			continue
		}
		roles[strings.Trim(string(n.Role.Value), `"`)] = true
	}
	for _, want := range []string{"banner", "navigation", "main"} {
		if !roles[want] {
			t.Errorf("the accessibility tree has no %q landmark", want)
		}
	}

	var badge struct {
		Text    string
		Width   float64
		Height  float64
		Visible bool
		InBar   bool
	}
	p.eval(`(() => {
		const b = document.querySelector(".local-badge");
		if (!b) return {};
		const r = b.getBoundingClientRect(), s = getComputedStyle(b);
		return { Text: b.innerText.trim(), Width: r.width, Height: r.height,
		         Visible: s.display !== "none" && s.visibility !== "hidden" && Number(s.opacity) > 0,
		         InBar: b.closest('[role="banner"]') !== null };
	})()`, &badge)
	if badge.Text != "LOCAL" || !badge.Visible || badge.Width == 0 || badge.Height == 0 || !badge.InBar {
		t.Errorf("the LOCAL badge = %+v; want the text LOCAL, rendered, inside the banner", badge)
	}
}

// TestThemeSwitchesWithoutNavigation: Light, Dark and Same as device apply
// to the page in place, as the documented console behaviour is. The main
// frame navigates only for the initial load, a value set on window survives
// every switch, and the background actually changes: Same as device follows
// an emulated dark preference.
func TestThemeSwitchesWithoutNavigation(t *testing.T) {
	p := open(t)
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.eval(`window.__cbKept = "kept"; true`, nil)
	loads := p.navigations()

	choose := func(value string) (theme, bg string) {
		t.Helper()
		p.run(chromedp.Click("#settings", chromedp.ByQuery))
		p.waitFor(`!document.querySelector("#settings-panel").hidden`)
		p.run(chromedp.Click(fmt.Sprintf(`#settings-panel input[name="theme"][value=%q]`, value), chromedp.ByQuery))
		want := value
		if value == "system" {
			want = ""
		}
		p.waitFor(fmt.Sprintf(`(document.documentElement.getAttribute("data-theme") || "") === %q`, want))
		var out struct{ Theme, Bg, Stored, Kept string }
		p.eval(`({ Theme: document.documentElement.getAttribute("data-theme") || "",
		           Bg: getComputedStyle(document.body).backgroundColor,
		           Stored: localStorage.getItem("cb-theme") || "",
		           Kept: window.__cbKept || "" })`, &out)
		if out.Stored != value {
			t.Errorf("after choosing %s the stored theme is %q", value, out.Stored)
		}
		if out.Kept != "kept" {
			t.Errorf("choosing %s reloaded the page: window state was lost", value)
		}
		// Close the panel the way a user would, so the next choice opens it.
		p.run(chromedp.KeyEvent(kb.Escape))
		return out.Theme, out.Bg
	}

	_, dark := choose("dark")
	_, light := choose("light")
	if dark == light {
		t.Errorf("dark and light render the same background %s", dark)
	}
	p.run(emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: "dark"}}))
	if _, system := choose("system"); system != dark {
		t.Errorf("Same as device on a dark device renders %s; dark renders %s", system, dark)
	}
	if n := p.navigations(); n != loads {
		t.Errorf("the main frame navigated %d times while the theme changed; want none", n-loads)
	}
}

// TestCreateBucketThroughTheForm: the Cloud Storage create form, opened from
// the empty state on its own page since it holds the bucket's options (#852),
// refuses a name its pattern rejects before anything is
// sent, with the constraint under the field; then a valid name is created
// through it and its row appears.
//
// The refusal is the guard for the pattern bug found in #49: an HTML pattern
// is compiled with the RegExp v flag, where an unescaped "-" in a character
// class makes the whole pattern invalid and the browser silently stops
// matching it. The invalid name is then accepted and posted, and the browser
// logs the invalid pattern, so both this test's assertions and its error log
// check fail.
func TestCreateBucketThroughTheForm(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	bucket := project + "-bucket"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "")
	})

	p.navigate("/storage/browser?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create")`)
	p.run(chromedp.Click(`#view .state button.primary`, chromedp.ByQuery))
	p.run(chromedp.WaitVisible(`#view #f-name`, chromedp.ByQuery))

	p.run(chromedp.SendKeys(`#view #f-name`, "Not-A-Valid-Name!", chromedp.ByQuery))
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	// Either the form refused it, or it was sent and the server's rejection
	// is on the form: the assertions below say which.
	p.waitFor(`document.querySelector("#view #f-name").getAttribute("aria-invalid") === "true" ||
		!document.querySelector("#view .form-error").hidden`)
	var refused struct {
		Message   string
		RowClass  string
		DialogUp  bool
		Validates bool
	}
	p.eval(`({ Message: document.querySelector("#view #e-name").textContent,
	           RowClass: document.querySelector("#view #f-name").closest(".form-row").className,
	           DialogUp: document.querySelector("#view form.create-page") !== null,
	           Validates: document.querySelector("#view #f-name").validity.patternMismatch })`, &refused)
	if refused.Message != bucketHelp || !strings.Contains(refused.RowClass, "is-invalid") || !refused.DialogUp || !refused.Validates {
		t.Errorf("an invalid bucket name was not refused by its pattern: %+v", refused)
	}
	if posts := p.posts("/api/resources/storage"); len(posts) != 0 {
		t.Fatalf("an invalid bucket name was posted: %v", posts)
	}

	p.eval(`(() => { const f = document.querySelector("#view #f-name");
		f.value = ""; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.SendKeys(`#view #f-name`, bucket, chromedp.ByQuery),
		chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`location.pathname === "/storage/browser" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, bucket))
	if posts := p.posts("/api/resources/storage"); len(posts) != 1 {
		t.Errorf("the create was posted %d times, want once: %v", len(posts), posts)
	}

	// The row is a real bucket: the listing the server serves has it.
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/storage?project="+project, ""); code != http.StatusOK || !strings.Contains(body, `"`+bucket+`"`) {
		t.Errorf("the console API does not list the bucket created in the browser: %d %s", code, body)
	}
}

// TestInjectedFaultShowsTheServicesMessage: a fault rule on Cloud Tasks'
// ListQueues, installed through /admin/faults and scoped to this test's
// project, turns the Cloud Tasks screen into its error card carrying the
// service's own message, not an empty table; once the rule is removed, Retry
// brings the screen back.
func TestInjectedFaultShowsTheServicesMessage(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	code, body := adminDo(t, http.MethodPost, "/admin/faults",
		`{"service":"tasks","method":"ListQueues","project":"`+project+`","code":"UNAVAILABLE"}`)
	if code != http.StatusCreated {
		t.Fatalf("add fault rule = %d: %s", code, body)
	}
	var rule struct{ ID string }
	if err := json.Unmarshal([]byte(body), &rule); err != nil || rule.ID == "" {
		t.Fatalf("fault rule response %s: %v", body, err)
	}
	removed := false
	remove := func() {
		if !removed {
			removed = true
			adminDo(t, http.MethodDelete, "/admin/faults?id="+url.QueryEscape(rule.ID), "")
		}
	}
	t.Cleanup(remove)

	p.navigate("/tasks/queues?project=" + project)
	p.waitFor(`document.querySelector("#view .state.error") !== null`)
	var card struct{ Role, Title, Detail string }
	p.eval(`(() => { const c = document.querySelector("#view .state.error");
		return { Role: c.getAttribute("role"), Title: c.querySelector("h2").textContent,
		         Detail: c.querySelector("pre").textContent }; })()`, &card)
	want := "Unavailable: injected fault (rule " + rule.ID + "): UNAVAILABLE"
	if card.Role != "alert" || card.Title != "Cloud Tasks unavailable" || card.Detail != want {
		t.Errorf("the error card = %+v; want role alert, title %q and the service's message %q", card, "Cloud Tasks unavailable", want)
	}

	remove()
	p.run(chromedp.Click(`#view .state.error button`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view .state.error") === null && document.querySelector("#view .state h2") !== null`)
}

// TestKeyboardOpensAListRow: with no pointer, Tab reaches the list screen's
// "Show info panel" button and Enter shows the panel; Tab then reaches a row,
// and Enter on it opens that row in the panel.
func TestKeyboardOpensAListRow(t *testing.T) {
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/us-central1/queues/browser-queue"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks?project="+project, `{"name":"browser-queue","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a queue through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/tasks?project="+project+"&name="+url.QueryEscape(name), "")
	})

	p.navigate("/tasks/queues?project=" + project)
	rowSel := fmt.Sprintf(`tr[role="button"][aria-label=%q]`, "Inspect "+name)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, rowSel))

	// The info panel is shown by its toolbar button, also from the keyboard.
	p.tabTo(`a.textContent === "Show info panel"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector("#info-panel").hidden`)
	p.tabTo(fmt.Sprintf(`a === document.querySelector(%q)`, rowSel))
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`(() => { const panel = document.querySelector("#info-panel");
		return !panel.hidden && panel.querySelector("h2") && panel.querySelector("h2").textContent === %q; })()`, name))
	var pressed string
	p.eval(fmt.Sprintf(`document.querySelector(%q).getAttribute("aria-pressed")`, rowSel), &pressed)
	if pressed != "true" {
		t.Errorf("the row opened by Enter has aria-pressed=%q, want true", pressed)
	}
}

// TestLoopbackGuardCatchesAnOffLoopbackRequest keeps the loopback assertion
// every other test makes from passing vacuously: a request the page makes to
// a documentation address (TEST-NET-1, never routed) is reported by the
// Network domain and lands on the sinkhole proxy, so it never left the
// machine. The console's CSP would refuse it before the network, which is
// its own protection, so it is bypassed for this page only.
func TestLoopbackGuardCatchesAnOffLoopbackRequest(t *testing.T) {
	p := open(t)
	p.run(page.SetBypassCSP(true))
	p.navigate("/")
	const target = "http://192.0.2.1/cloudburrow-browser-guard"
	p.eval(fmt.Sprintf(`fetch(%q, { mode: "no-cors" }).then(() => true, () => true)`, target), nil, awaitPromise)

	deadline := time.Now().Add(10 * time.Second)
	for {
		seen, sunk := p.offLoopbackSeen(target), p.sunk("192.0.2.1")
		if seen && sunk {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the off-loopback request was not caught: seen by the Network domain %v, received by the sinkhole %v", seen, sunk)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Expected here, so not reported when the test ends.
	p.forgive()
}

// --- the harness -------------------------------------------------------

// tab is one browser, one page and what it has been seen to do.
type tab struct {
	t      *testing.T
	ctx    context.Context
	origin string

	mu        sync.Mutex
	requests  []string // "METHOD URL", every request the page made
	offLoop   []string // the ones not for a loopback host
	errors    []string // exceptions, console.error and error log entries
	mainNavs  int      // main-frame navigations
	sinkholed []string // requests the sinkhole proxy received
}

// open starts a headless Chrome for one test, behind the sinkhole proxy, with
// every listener attached. A failing test leaves a screenshot.
func open(t *testing.T) *tab {
	t.Helper()
	refuseCloudCredentials(t)
	consoleAddr := endpoint(t, envConsole)
	p := &tab{t: t, origin: "http://" + consoleAddr}

	// The proxy for everything that is not loopback: it records the request
	// and answers 502, so nothing the browser asks for leaves the machine.
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.sinkholed = append(p.sinkholed, r.Method+" "+r.Host+" "+r.URL.String())
		p.mu.Unlock()
		http.Error(w, "cloudburrow browser test: no request may leave loopback", http.StatusBadGateway)
	}))
	t.Cleanup(sink.Close)

	// Chrome's profile directory. Not t.TempDir: its cleanup fails the
	// test when removal finds the directory not empty, and Chrome's helper
	// processes can still be writing to it for a moment after the browser is
	// cancelled (seen on ubuntu-latest). Registered before the browser's
	// cleanups, so it runs after them; removal is retried briefly.
	profile, err := os.MkdirTemp("", "cloudburrow-browser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeEventually(t, profile) })

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(1440, 900),
		chromedp.ProxyServer(sink.URL),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-domain-reliability", true),
		chromedp.Flag("no-pings", true),
		chromedp.UserDataDir(profile),
	)
	if path := os.Getenv(envChrome); path != "" {
		opts = append(opts, chromedp.ExecPath(path))
	}
	if os.Getenv(envNoSandbox) == "1" {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	t.Cleanup(cancelAlloc)
	ctx, cancelTab := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelTab)
	ctx, cancelTimeout := context.WithTimeout(ctx, 3*time.Minute)
	t.Cleanup(cancelTimeout)
	p.ctx = ctx

	chromedp.ListenTarget(ctx, func(ev any) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			p.requests = append(p.requests, ev.Request.Method+" "+ev.Request.URL)
			if !loopbackURL(ev.Request.URL) {
				p.offLoop = append(p.offLoop, ev.Request.URL)
			}
		case *runtime.EventExceptionThrown:
			p.errors = append(p.errors, "exception: "+exceptionText(ev.ExceptionDetails))
		case *runtime.EventConsoleAPICalled:
			if ev.Type == runtime.APITypeError {
				var parts []string
				for _, a := range ev.Args {
					if a.Description != "" {
						parts = append(parts, a.Description)
					} else {
						parts = append(parts, string(a.Value))
					}
				}
				p.errors = append(p.errors, "console.error: "+strings.Join(parts, " "))
			}
		case *cdplog.EventEntryAdded:
			if ev.Entry.Level == cdplog.LevelError {
				p.errors = append(p.errors, fmt.Sprintf("log (%s): %s %s", ev.Entry.Source, ev.Entry.Text, ev.Entry.URL))
			}
		case *page.EventFrameNavigated:
			if ev.Frame.ParentID == "" {
				p.mainNavs++
			}
		}
	})
	if err := chromedp.Run(ctx, network.Enable(), cdplog.Enable()); err != nil {
		t.Fatalf("start Chrome (set %s to its binary if it is not found): %v", envChrome, err)
	}

	// Runs before the browser is closed: cleanups run last-registered first.
	t.Cleanup(func() {
		p.mu.Lock()
		off, errs, sunk := p.offLoop, p.errors, p.sinkholed
		p.mu.Unlock()
		for _, u := range off {
			t.Errorf("the page requested %s, which is not loopback", u)
		}
		// Chrome's own services (component updates, sign-in, autofill)
		// call home from the browser process, not from the page; the
		// sinkhole is what stops them leaving. A request the page made is
		// in off, and fails the test above.
		if len(sunk) > 0 {
			t.Logf("the sinkhole proxy refused %d requests Chrome made off loopback, none of which left the machine: %s",
				len(sunk), strings.Join(sunk, "; "))
		}
		for _, e := range errs {
			t.Errorf("the page reported an error: %s", e)
		}
		if t.Failed() {
			p.screenshot()
		}
	})
	return p
}

func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

func (p *tab) run(actions ...chromedp.Action) {
	p.t.Helper()
	if err := chromedp.Run(p.ctx, actions...); err != nil {
		p.t.Fatalf("browser: %v", err)
	}
}

func (p *tab) navigate(path string) {
	p.t.Helper()
	p.run(chromedp.Navigate(p.origin + path))
}

func (p *tab) eval(js string, out any, opts ...chromedp.EvaluateOption) {
	p.t.Helper()
	p.run(chromedp.Evaluate(js, out, opts...))
}

// waitFor polls a JavaScript expression until it is true.
func (p *tab) waitFor(expr string) {
	p.t.Helper()
	var ok bool
	if err := chromedp.Run(p.ctx, chromedp.Poll(expr, &ok, chromedp.WithPollingTimeout(30*time.Second))); err != nil || !ok {
		p.t.Fatalf("waiting for %s: %v", strings.Join(strings.Fields(expr), " "), err)
	}
}

// tabTo presses Tab until the focused element, bound to a, satisfies pred.
func (p *tab) tabTo(pred string) {
	p.t.Helper()
	check := fmt.Sprintf(`(() => { const a = document.activeElement; return !!a && (%s); })()`, pred)
	for i := 0; i < 200; i++ {
		p.run(chromedp.KeyEvent(kb.Tab))
		var ok bool
		p.eval(check, &ok)
		if ok {
			return
		}
	}
	var active string
	p.eval(`document.activeElement ? document.activeElement.outerHTML.slice(0, 200) : ""`, &active)
	p.t.Fatalf("200 presses of Tab did not reach the element where %s; focus is on %s", pred, active)
}

func (p *tab) axTree() ([]*accessibility.Node, error) {
	var nodes []*accessibility.Node
	err := chromedp.Run(p.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		nodes, err = accessibility.GetFullAXTree().Do(ctx)
		return err
	}))
	return nodes, err
}

func (p *tab) navigations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mainNavs
}

// posts are the POST requests the page made to a path on the console.
func (p *tab) posts(path string) []string {
	return p.sent(http.MethodPost, path)
}

func (p *tab) offLoopbackSeen(u string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range p.offLoop {
		if o == u {
			return true
		}
	}
	return false
}

func (p *tab) sunk(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sinkholed {
		if strings.Contains(s, " "+host+" ") || strings.Contains(s, " "+host+":") {
			return true
		}
	}
	return false
}

// forgive drops what a test provoked on purpose.
func (p *tab) forgive() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offLoop, p.sinkholed, p.errors = nil, nil, nil
}

// screenshot writes the page as it was when the test failed.
func (p *tab) screenshot() {
	dir := os.Getenv(envScreenshots)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "cloudburrow-browser")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.t.Logf("screenshot: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
	defer cancel()
	var png []byte
	if err := chromedp.Run(ctx, chromedp.FullScreenshot(&png, 90)); err != nil {
		p.t.Logf("screenshot: %v", err)
		return
	}
	name := filepath.Join(dir, regexp.MustCompile(`[^A-Za-z0-9_.-]+`).ReplaceAllString(p.t.Name(), "_")+".png")
	if err := os.WriteFile(name, png, 0o644); err != nil {
		p.t.Logf("screenshot: %v", err)
		return
	}
	p.t.Logf("screenshot of the failing page: %s", name)
}

func exceptionText(d *runtime.ExceptionDetails) string {
	if d == nil {
		return ""
	}
	if d.Exception != nil && d.Exception.Description != "" {
		return d.Exception.Description
	}
	return d.Text
}

// loopbackURL says whether a request stays on this machine: an http(s) or
// ws(s) URL for a loopback host, or one that never touches the network.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "data", "blob", "about":
		return true
	case "http", "https", "ws", "wss":
	default:
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// endpoint is a loopback address from the environment, or a skip when the
// variable is unset: this suite needs an instance, as the compat suite does.
func endpoint(t *testing.T, env string) string {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv(env))
	if addr == "" {
		t.Skipf("%s is not set; start an instance and export its endpoints (see test/browser)", env)
	}
	addr = strings.TrimSuffix(strings.TrimPrefix(addr, "http://"), "/")
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%s=%q is not host:port: %v", env, addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("%s=%q is not loopback; the browser suite never leaves this machine", env, addr)
	}
	return addr
}

func refuseCloudCredentials(t *testing.T) {
	t.Helper()
	for _, v := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"} {
		if os.Getenv(v) != "" {
			t.Fatalf("%s is set; the browser suite refuses to run with cloud credentials in the environment", v)
		}
	}
}

// uniqueProject registers a project no other test uses, so fault rules and
// resources scoped to it cannot touch anything else. It is registered
// through the console, because the console switches away from a project its
// instance's registry does not hold, and removed when the test ends.
func uniqueProject(t *testing.T) string {
	t.Helper()
	id := fmt.Sprintf("cb-browser-%d", time.Now().UnixNano()%1e12)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/projects", `{"projectId":"`+id+`"}`); code != http.StatusOK {
		t.Fatalf("register project %s through the console API = %d: %s", id, code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/projects?name="+id, "") })
	return id
}

func consoleDo(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	return do(t, method, "http://"+endpoint(t, envConsole)+path, body, "")
}

// consoleDeploy is consoleDo for a request that waits on a Cloud Run
// rollout. The console answers once the revision is ready, and on an
// instance just brought up again (the run shard's browser step) that took
// longer than do's minute: the first CI run hit its deadline (#765).
func consoleDeploy(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	return doWithin(t, 5*time.Minute, method, "http://"+endpoint(t, envConsole)+path, body, "")
}

func adminDo(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	token := strings.TrimSpace(os.Getenv(envAdminToken))
	if token == "" {
		t.Fatalf("%s is not set; /admin/faults needs the instance's admin token", envAdminToken)
	}
	return do(t, method, "http://"+endpoint(t, envControl)+path, body, token)
}

func do(t *testing.T, method, u, body, token string) (int, string) {
	t.Helper()
	return doWithin(t, 60*time.Second, method, u, body, token)
}

func doWithin(t *testing.T, timeout time.Duration, method, u, body, token string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// removeEventually removes dir, retrying while something is still writing to
// it, and logs rather than fails if it never empties: a leftover profile is
// not a console regression.
func removeEventually(t *testing.T, dir string) {
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
	}
	t.Logf("could not remove Chrome's profile %s: %v", dir, err)
}
