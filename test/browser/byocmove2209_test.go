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
	mu       sync.Mutex
	moves    map[string]console.MoveStatus
	fail     bool
	replaced []string
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

// ResumeMove plays the rest of an interrupted move.
func (m *scriptedMover) ResumeMove(instance string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.moves[instance]
	if !ok || !st.Interrupted {
		return errors.New(instance + " has no unfinished move to resume")
	}
	st.Done, st.Interrupted, st.Error = true, false, ""
	st.Steps = append(st.Steps, console.MoveStep{Time: time.Now(), Step: "resume", Message: "Resuming the move of " + instance},
		console.MoveStep{Time: time.Now(), Step: "done", Message: instance + " runs on " + st.To})
	m.moves[instance] = st
	return nil
}

// RollbackMove undoes an interrupted move.
func (m *scriptedMover) RollbackMove(instance string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.moves[instance]
	if !ok || !st.Interrupted {
		return errors.New(instance + " has no unfinished move to roll back")
	}
	st.Done, st.Interrupted, st.RolledBack = true, false, true
	st.Error = "rolled back at the user's request"
	st.Steps = append(st.Steps, console.MoveStep{Time: time.Now(), Step: "rollback", Message: "Rolled back"})
	m.moves[instance] = st
	return nil
}

// StartReplace plays a change of VM: a new VM, the move, the old deleted.
func (m *scriptedMover) StartReplace(instance, vm, size string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	to := vm + "-" + size
	m.moves[instance] = console.MoveStatus{Instance: instance, Kind: "replace", To: to, Started: time.Now(), Done: true, Steps: []console.MoveStep{
		{Time: time.Now(), Step: "create", Message: "Making " + to},
		{Time: time.Now(), Step: "move", Message: "Moving " + instance + " from " + vm + " to " + to},
		{Time: time.Now(), Step: "delete", Message: "Deleting " + vm},
		{Time: time.Now(), Step: "done", Message: to + " is the GPU size now"}}}
	m.replaced = append(m.replaced, vm+":"+size)
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

// Compute capacity's Change VM (#2209): the slider reaches the GPU size,
// which no cloud resizes a CPU VM to in place, so the page shows the plan
// (a new VM, the move, the old VM deleted) and the price difference before
// it asks; and a move that was interrupted offers Resume and Roll back.
func TestByocChangeVMAndResumeAnInterruptedMove(t *testing.T) {
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
	p.waitFor(`document.getElementById("capacity-instance-where") !== null && !document.getElementById("capacity-demo-vm-size").disabled`)
	var where string
	p.eval(`document.getElementById("capacity-instance-where").textContent`, &where)
	if where != "This machine's Docker" {
		t.Errorf("the instance runs on %q", where)
	}

	// The slider's last stop is the GPU size: a new VM, with the price
	// difference.
	p.run(chromedp.Focus(chromedp.CSS(`#capacity-demo-vm-size`)), chromedp.KeyEvent(kb.End))
	p.waitFor(`document.getElementById("capacity-demo-vm-size-value").textContent.includes("g6.xlarge")`)
	var slider struct{ Value, Button string }
	p.eval(`({ Value: document.getElementById("capacity-demo-vm-size-value").textContent, Button: document.getElementById("capacity-demo-vm-resize").textContent })`, &slider)
	if !strings.Contains(slider.Value, "a new VM") || !strings.Contains(slider.Value, "+$") || slider.Button != "Change VM" {
		t.Fatalf("the slider at GPU reads %+v", slider)
	}
	p.run(chromedp.Click(chromedp.CSS(`#capacity-demo-vm-resize`)))
	p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null`)
	var detail string
	p.eval(`document.querySelector(".modal").textContent`, &detail)
	for _, want := range []string{"Change demo-vm to g6.xlarge", "cannot resize m7i.xlarge to g6.xlarge in place", "demo-vm-gpu", "/hour more", "deleted only once"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the Change VM dialog lacks %q: %s", want, detail)
		}
	}
	if len(mover.replaced) != 0 {
		t.Fatal("the change started before it was confirmed")
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type=submit]`)))
	p.waitForWithin(`document.getElementById("capacity-instance-move-steps-outcome") !== null`, 30*time.Second)
	var outcome, steps string
	p.eval(`document.getElementById("capacity-instance-move-steps-outcome").textContent`, &outcome)
	p.eval(`[...document.querySelectorAll("#capacity-instance-move-steps li")].map((li) => li.dataset.step).join(",")`, &steps)
	if steps != "create,move,delete,done" || !strings.Contains(outcome, "runs on demo-vm-gpu") || strings.Join(mover.replaced, ",") != "demo-vm:gpu" {
		t.Fatalf("the change reads %q %q (%v)", steps, outcome, mover.replaced)
	}

	// An interrupted move: Resume finishes it.
	interrupted := func() {
		mover.mu.Lock()
		mover.moves["byoc-demo"] = console.MoveStatus{Instance: "byoc-demo", To: "demo-vm", Started: time.Now(), Done: true, Interrupted: true,
			Error: "the move was interrupted", Steps: []console.MoveStep{{Time: time.Now(), Step: "copy", Message: "Copying volume 3f2a: 0.4 GiB so far"}}}
		mover.mu.Unlock()
	}
	interrupted()
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-instance-move-resume") !== null`)
	var note string
	p.eval(`document.getElementById("capacity-instance-move-interrupted").textContent`, &note)
	if !strings.Contains(note, "was interrupted") {
		t.Errorf("the interrupted move reads %q", note)
	}
	p.run(chromedp.Click(chromedp.CSS(`#capacity-instance-move-resume`)))
	p.waitForWithin(`document.getElementById("capacity-instance-move-steps-outcome") !== null`, 30*time.Second)
	p.eval(`document.getElementById("capacity-instance-move-steps-outcome").textContent`, &outcome)
	if !strings.Contains(outcome, "byoc-demo runs on demo-vm") {
		t.Fatalf("after Resume the move reads %q", outcome)
	}

	// Roll back undoes another, after a confirmation.
	interrupted()
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-instance-move-rollback") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-instance-move-rollback`)))
	p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type=submit]`)))
	p.waitForWithin(`document.getElementById("capacity-instance-move-steps-outcome") !== null`, 30*time.Second)
	var rolled struct{ Role, Text string }
	p.eval(`({ Role: document.getElementById("capacity-instance-move-steps-outcome").getAttribute("role"),
		Text: document.getElementById("capacity-instance-move-steps-outcome").textContent })`, &rolled)
	if rolled.Role != "alert" || !strings.Contains(rolled.Text, "rolled back") {
		t.Fatalf("after Roll back the move reads %+v", rolled)
	}

	t.Run("WCAG", func(t *testing.T) {
		interrupted()
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-instance-move-resume") !== null && !document.getElementById("capacity-demo-vm-size").disabled`)
		for _, u := range p.unnamedControls() {
			t.Errorf("an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("%s", c)
		}
		p.run(chromedp.Focus(chromedp.CSS(`#capacity-demo-vm-size`)), chromedp.KeyEvent(kb.End))
		p.run(chromedp.Click(chromedp.CSS(`#capacity-demo-vm-resize`)))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null`)
		for _, u := range p.unnamedControls() {
			t.Errorf("the Change VM dialog: an unnamed %s", u)
		}
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") === null`)
	})

	t.Run("keyboard", func(t *testing.T) {
		p.run(reducedMotion())
		interrupted()
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-instance-move-resume") !== null && !document.getElementById("capacity-demo-vm-size").disabled`)
		for _, f := range p.keyboardFailures() {
			t.Errorf("Compute capacity: %s", f)
		}
		// Roll back opens by keyboard with focus on Cancel.
		p.tabTo(`a.id === "capacity-instance-move-rollback"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") !== null && document.activeElement.textContent === "Cancel"`)
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal form[data-confirm=confirm]") === null`)
	})
}
