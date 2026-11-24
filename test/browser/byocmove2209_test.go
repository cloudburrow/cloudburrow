//go:build browser

package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Compute capacity's Move (#2209): an instance on this machine's Docker
// moves onto a VM made in the user's AWS account (the local fake awsfake).
// The move itself is `cloudburrow cloud move` in a process of its own (the
// stand-in tests carry real state, internal/remotecompute); here a mover
// plays its progress file, so the page's start, progress, outcome and
// failure are what is checked.

// scriptedMover plays a move's steps, or a rolled-back failure.
type scriptedMover struct {
	mu    sync.Mutex
	moves map[string]console.MoveStatus
	fail  bool
}

func (m *scriptedMover) StartMove(instance, vm string) error {
	m.mu.Lock()
	if st, ok := m.moves[instance]; ok && !st.Done {
		m.mu.Unlock()
		return errors.New(instance + " is being moved already")
	}
	m.moves[instance] = console.MoveStatus{Instance: instance, To: vm, Started: time.Now(), Steps: []console.MoveStep{{Time: time.Now(), Step: "queued", Message: "Starting the move of " + instance + " to " + vm}}}
	fail := m.fail
	m.mu.Unlock()
	go func() {
		steps := [][2]string{{"stop", "Stopping " + instance + " on this machine"}, {"connect", "Connected to ssh://ubuntu@203.0.113.10:22"},
			{"copy", "Copying volume 3f2a: 1.2 GiB so far"}, {"start", "Starting " + instance + " on ssh://ubuntu@203.0.113.10:22"}}
		if fail {
			steps = append(steps[:3], [2]string{"rollback", "The move failed: node is not running"}, [2]string{"rollback", "Rolled back"})
		} else {
			steps = append(steps, [2]string{"verify", "Checking the cluster"}, [2]string{"done", instance + " runs on ssh://ubuntu@203.0.113.10:22"})
		}
		for _, s := range steps {
			time.Sleep(300 * time.Millisecond)
			m.mu.Lock()
			st := m.moves[instance]
			st.Steps = append(st.Steps, console.MoveStep{Time: time.Now(), Step: s[0], Message: s[1]})
			if s[0] == "rollback" {
				st.RolledBack = true
			}
			m.moves[instance] = st
			m.mu.Unlock()
		}
		m.mu.Lock()
		st := m.moves[instance]
		st.Done = true
		if fail {
			st.Error = "node cloudburrow-byoc-demo-control-plane is not running on the VM after the move"
		}
		m.moves[instance] = st
		m.mu.Unlock()
	}()
	return nil
}

func (m *scriptedMover) MoveStatus(instance string) (console.MoveStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.moves[instance]
	return st, ok
}

// localCompute is an instance whose Docker is this machine's.
type localCompute struct{}

func (localCompute) Compute() any                    { return map[string]any{"location": "local", "connected": true} }
func (localCompute) Reconnect(context.Context) error { return errors.New("local") }
func (localCompute) Local() bool                     { return true }

func openByocMove(t *testing.T, mover *scriptedMover) (*byocConsole, *byoc.Manager) {
	t.Helper()
	b := &byocConsole{aws: awsfake.New(), keys: keychain.NewMemory()}
	t.Cleanup(b.aws.Close)
	m, err := byoc.NewManager(byoc.Options{Dir: t.TempDir(), Keychain: b.keys,
		Drivers:     []byoc.Driver{awsprov.NewDriver(b.aws.Options())},
		IPLookup:    func(context.Context) (string, error) { return "203.0.113.77", nil },
		OpenBrowser: func(string) error { return nil },
		Location:    func() (float64, float64, bool) { return 47.6, -122.3, true }})
	if err != nil {
		t.Fatal(err)
	}
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetCloudCompute(m, "byoc-demo", func(_ context.Context, authz string) error {
		if authz != "Bearer "+byocToken {
			return &console.InstanceRefusal{Status: http.StatusUnauthorized, Message: "missing or wrong admin token"}
		}
		return nil
	})
	s.SetCompute(localCompute{})
	s.SetMover(mover)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	b.p = openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	t.Cleanup(b.p.forgive)
	return b, m
}

func TestByocMoveAnInstanceOntoTheVM(t *testing.T) {
	mover := &scriptedMover{moves: map[string]console.MoveStatus{}}
	b, m := openByocMove(t, mover)
	p := b.p
	p.navigate("/settings/compute")
	p.waitFor(`document.getElementById("compute-token") !== null`)
	p.setField("#compute-token", byocToken)
	p.eval(`document.getElementById("compute-token-form").requestSubmit(), true`, nil)
	p.waitFor(`document.getElementById("compute-aws-card") !== null`)
	b.connectAWS()
	if _, err := m.Create(context.Background(), byoc.CreateRequest{Instance: "demo-vm", Connection: "aws-" + awsfake.DefaultAccount,
		Region: "us-west-2", Preset: byoc.Small, AutoStopMinutes: 60}, true, nil); err != nil {
		t.Fatal(err)
	}
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-demo-vm-move") !== null && !document.getElementById("capacity-demo-vm-move").disabled`)

	// Move: the dialog says what happens, and nothing moves until it is
	// confirmed.
	p.run(chromedp.Click(chromedp.CSS(`#capacity-demo-vm-move`)))
	p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null`)
	var detail string
	p.eval(`document.querySelector(".modal").textContent`, &detail)
	for _, want := range []string{"Move byoc-demo to demo-vm", "rolls back", "Nothing is removed from this machine"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the dialog lacks %q: %s", want, detail)
		}
	}
	if _, ok := mover.MoveStatus("byoc-demo"); ok {
		t.Fatal("the move started before it was confirmed")
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type=submit]`)))
	p.waitForWithin(`document.getElementById("capacity-demo-vm-move-steps-outcome") !== null`, 30*time.Second)
	var got struct{ Steps, Outcome string }
	p.eval(`({ Steps: [...document.querySelectorAll("#capacity-demo-vm-move-steps li")].map((li) => li.dataset.step).join(","),
		Outcome: document.getElementById("capacity-demo-vm-move-steps-outcome").textContent })`, &got)
	if got.Steps != "queued,stop,connect,copy,start,verify,done" || !strings.Contains(got.Outcome, "byoc-demo runs on demo-vm") {
		t.Fatalf("the move's progress reads %+v", got)
	}
	if st, _ := mover.MoveStatus("byoc-demo"); st.To != "demo-vm" {
		t.Fatalf("the move went to %q", st.To)
	}

	// A failure says it was rolled back, in an alert.
	mover.mu.Lock()
	mover.fail = true
	delete(mover.moves, "byoc-demo")
	mover.mu.Unlock()
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-demo-vm-move") !== null && !document.getElementById("capacity-demo-vm-move").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-demo-vm-move`)))
	p.waitFor(`document.querySelector(".modal button[type=submit]") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type=submit]`)))
	p.waitForWithin(`document.getElementById("capacity-demo-vm-move-steps-outcome") !== null`, 30*time.Second)
	var failed struct{ Role, Text, Error string }
	p.eval(`({ Role: document.getElementById("capacity-demo-vm-move-steps-outcome").getAttribute("role"),
		Text: document.getElementById("capacity-demo-vm-move-steps-outcome").textContent,
		Error: document.getElementById("capacity-demo-vm-error").textContent })`, &failed)
	if failed.Role != "alert" || !strings.Contains(failed.Text, "rolled back") || !strings.Contains(failed.Error, "Move failed") {
		t.Fatalf("the failed move reads %+v", failed)
	}

	// A stopped VM cannot take the instance; the button says why.
	if _, err := m.Stop(context.Background(), "demo-vm", true, nil); err != nil {
		t.Fatal(err)
	}
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-demo-vm-state") !== null && document.getElementById("capacity-demo-vm-state").textContent === "Stopped"`)
	var why string
	p.eval(`(() => { const b = document.getElementById("capacity-demo-vm-move");
		return b.disabled + ":" + document.getElementById(b.getAttribute("aria-describedby")).textContent })()`, &why)
	if why != "true:Start the VM first" {
		t.Fatalf("the stopped VM's Move reads %q", why)
	}
	if _, err := m.Start(context.Background(), "demo-vm", true, nil); err != nil {
		t.Fatal(err)
	}

	t.Run("WCAG", func(t *testing.T) {
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-demo-vm-move") !== null && !document.getElementById("capacity-demo-vm-move").disabled`)
		for _, u := range p.unnamedControls() {
			t.Errorf("an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("%s", c)
		}
		p.run(chromedp.Click(chromedp.CSS(`#capacity-demo-vm-move`)))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null`)
		for _, u := range p.unnamedControls() {
			t.Errorf("the Move dialog: an unnamed %s", u)
		}
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") === null`)
	})

	t.Run("keyboard", func(t *testing.T) {
		p.run(reducedMotion())
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-demo-vm-move") !== null && !document.getElementById("capacity-demo-vm-move").disabled`)
		for _, f := range p.keyboardFailures() {
			t.Errorf("Compute capacity: %s", f)
		}
		// Move opens by keyboard, with focus on Cancel: one reflexive
		// Enter does not move the instance.
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-demo-vm-move") !== null && !document.getElementById("capacity-demo-vm-move").disabled`)
		p.tabTo(`a.id === "capacity-demo-vm-move"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null && document.activeElement.textContent === "Cancel"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") === null`)
	})
}
