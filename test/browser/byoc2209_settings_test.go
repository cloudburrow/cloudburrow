//go:build browser

package browser

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/awsfake"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov/azurefake"
)

// TestByocConnectionModeAndAutoStopAfterCreate (#2209): Create VM's
// Connection choice (Session Manager on AWS, SSH from the user's address
// only, Azure SSH only), then Compute capacity's Connection row, Allow my
// address, and auto-stop changed after create, on the local fakes.
func TestByocConnectionModeAndAutoStopAfterCreate(t *testing.T) {
	b := openByoc(t)
	p := b.p
	b.signIn()
	b.connectAWS()
	b.connectAzure()

	p.run(chromedp.Click(chromedp.CSS(`#compute-create`)))
	p.waitFor(`location.pathname === "/settings/compute/create" && document.getElementById("compute-connect-ssm") !== null`)
	var form struct {
		SSM, SSMDisabled bool
		Help             string
	}
	readForm := func() {
		p.eval(`({ SSM: document.getElementById("compute-connect-ssm").checked, SSMDisabled: document.getElementById("compute-connect-ssm").disabled,
			Help: document.getElementById("compute-connect-help").textContent })`, &form)
	}
	// AWS: Session Manager first, nothing inbound.
	p.setField("#compute-connection", "aws-"+awsfake.DefaultAccount)
	readForm()
	if !form.SSM || form.SSMDisabled || !strings.Contains(form.Help, "allows nothing inbound") {
		t.Fatalf("AWS's connection choice reads %+v", form)
	}
	// Azure: SSH only, said in words.
	p.setField("#compute-connection", "azure-"+azurefake.Subscription2)
	readForm()
	if form.SSM || !form.SSMDisabled || !strings.Contains(form.Help, "Azure has no Session Manager") {
		t.Fatalf("Azure's connection choice reads %+v", form)
	}
	// Back to AWS, choosing SSH.
	p.setField("#compute-connection", "aws-"+awsfake.DefaultAccount)
	p.waitFor(`document.querySelectorAll("#compute-sizes input[name=compute-size]").length === 4 && document.getElementById("compute-permissions-ok") !== null`)
	p.clickExpr(`document.getElementById("compute-connect-ssh")`)
	readForm()
	if form.SSM || !strings.Contains(form.Help, "your public address only") {
		t.Fatalf("SSH chosen reads %+v", form)
	}
	p.run(chromedp.Click(chromedp.CSS(`#compute-create-submit`)))
	if failed := b.waitOp(); failed != "" {
		t.Fatalf("create failed: %s", failed)
	}
	var ingress []string
	for _, r := range b.aws.Live() {
		if r.Kind == "security-group" {
			ingress = r.Ingress
		}
	}
	if len(ingress) != 1 || ingress[0] != "tcp 22-22 203.0.113.77/32" {
		t.Fatalf("SSH mode's firewall admits %v", ingress)
	}

	// Compute capacity: the Connection row, Allow my address.
	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-byoc-demo-connect") !== null`)
	var connect string
	p.eval(`document.getElementById("capacity-byoc-demo-connect").textContent`, &connect)
	if !strings.HasPrefix(connect, "SSH from 203.0.113.77/32 only") {
		t.Fatalf("the Connection row reads %q", connect)
	}
	p.waitFor(`!document.getElementById("capacity-byoc-demo-allow-address").disabled`)
	if !strings.Contains(connect, "updated") {
		t.Errorf("the Connection row does not say when the address was set: %q", connect)
	}
	// The card is drawn again when the operation is done.
	p.eval(`document.getElementById("capacity-byoc-demo").dataset.old = "1", true`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-allow-address`)))
	p.waitForWithin(`document.getElementById("capacity-byoc-demo") && !document.getElementById("capacity-byoc-demo").dataset.old`, time.Minute)
	var allowErr string
	p.eval(`document.getElementById("capacity-byoc-demo-error").textContent`, &allowErr)
	if allowErr != "" {
		t.Errorf("Allow my address: %s", allowErr)
	}

	// Auto-stop after create: on a stopped VM it is recorded for the next
	// start, and the card says so.
	p.waitFor(`!document.getElementById("capacity-byoc-demo-stop").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-stop`)))
	p.waitForWithin(`document.getElementById("capacity-byoc-demo-state") && document.getElementById("capacity-byoc-demo-state").textContent === "Stopped"`, time.Minute)
	var help string
	p.eval(`document.getElementById("capacity-byoc-demo-autostop-help").textContent`, &help)
	if !strings.Contains(help, "when it next starts") {
		t.Errorf("a stopped VM's auto-stop help reads %q", help)
	}
	var saveDisabled bool
	p.eval(`document.getElementById("capacity-byoc-demo-autostop-save").disabled`, &saveDisabled)
	if !saveDisabled {
		t.Error("Save is enabled before anything changed")
	}
	p.setField("#capacity-byoc-demo-autostop", "240")
	p.waitFor(`!document.getElementById("capacity-byoc-demo-autostop-save").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-autostop-save`)))
	p.waitForWithin(`document.getElementById("capacity-byoc-demo-autostop-value") && document.getElementById("capacity-byoc-demo-autostop-value").textContent === "After 4 hours idle"`, time.Minute)

	t.Run("WCAG", func(t *testing.T) {
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-byoc-demo-autostop") !== null && document.querySelector("#view .skeleton") === null`)
		for _, u := range p.unnamedControls() {
			t.Errorf("Compute capacity: an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("Compute capacity: %s", c)
		}
		p.navigate("/settings/compute/create")
		p.waitFor(`document.getElementById("compute-connect-ssm") !== null && document.querySelectorAll("#compute-sizes input").length === 4`)
		for _, u := range p.unnamedControls() {
			t.Errorf("Create VM: an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("Create VM: %s", c)
		}
	})

	t.Run("keyboard", func(t *testing.T) {
		p.run(reducedMotion())
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-byoc-demo-autostop") !== null && !document.getElementById("capacity-byoc-demo-autostop").disabled`)
		for _, f := range p.keyboardFailures() {
			t.Errorf("Compute capacity: %s", f)
		}
		// Auto-stop changes by keyboard alone: the select, then Save.
		p.navigate("/compute-capacity")
		p.waitFor(`document.getElementById("capacity-byoc-demo-autostop") !== null && !document.getElementById("capacity-byoc-demo-autostop").disabled`)
		p.tabTo(`a.id === "capacity-byoc-demo-autostop"`)
		p.run(chromedp.KeyEvent(kb.ArrowUp))
		p.waitFor(`!document.getElementById("capacity-byoc-demo-autostop-save").disabled`)
		p.tabTo(`a.id === "capacity-byoc-demo-autostop-save"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitForWithin(`document.getElementById("capacity-byoc-demo-autostop-value") && document.getElementById("capacity-byoc-demo-autostop-value").textContent === "After 2 hours idle"`, time.Minute)
		// The connection choice by keyboard: arrows move between the radios.
		p.navigate("/settings/compute/create")
		p.waitFor(`document.getElementById("compute-connect-ssm") !== null && !document.getElementById("compute-connect-ssm").disabled`)
		p.tabTo(`a.id === "compute-connect-ssm"`)
		p.run(chromedp.KeyEvent(kb.ArrowDown))
		p.waitFor(`document.getElementById("compute-connect-ssh").checked && document.getElementById("compute-connect-help").textContent.includes("your public address only")`)
	})

	p.navigate("/compute-capacity")
	p.waitFor(`document.getElementById("capacity-byoc-demo-delete") !== null && !document.getElementById("capacity-byoc-demo-delete").disabled`)
	p.run(chromedp.Click(chromedp.CSS(`#capacity-byoc-demo-delete`)))
	p.confirmByName("byoc-demo")
	p.waitForWithin(`location.pathname === "/compute-capacity" && document.getElementById("capacity-byoc-demo") === null`, time.Minute)
	if live := b.aws.Live(); len(live) != 0 {
		t.Fatalf("Delete left %+v", live)
	}
}
