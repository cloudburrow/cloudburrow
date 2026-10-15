package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// TestConsoleSecretRotationAndDestroyDelay (#1278): Create secret and Edit
// secret take the rotation and the destruction delay, the pages show them,
// Destroy on a secret with a delay says so and schedules the destruction,
// Enable cancels it, and Advance clock runs what is due.
func TestConsoleSecretRotationAndDestroyDelay(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	st := p.svc.Store()
	var events []string
	st.SetPublisher(func(_ context.Context, _ string, _ []byte, attrs map[string]string) error {
		events = append(events, attrs["eventType"])
		return nil
	})
	next := st.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "r", "payload": "x",
		"topics": "projects/demo/topics/t", "nextRotationTime": next.Format(time.RFC3339),
		"rotationPeriod": "24h", "versionDestroyTtl": "48h"}); err != nil {
		t.Fatal(err)
	}
	sec, _ := st.GetSecret("demo", "r")
	if !sec.NextRotation.Equal(next) || sec.RotationPeriod != 24*time.Hour || sec.VersionDestroyTTL != 48*time.Hour {
		t.Fatalf("created schedule = %v %v %v", sec.NextRotation, sec.RotationPeriod, sec.VersionDestroyTTL)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "bad", "payload": "x",
		"nextRotationTime": next.Format(time.RFC3339)}); err == nil {
		t.Error("a rotation with no topics was accepted")
	}
	d, err := p.Detail(ctx, "demo", []string{sec.Name})
	if err != nil {
		t.Fatal(err)
	}
	if text := detailText(d); !strings.Contains(text, next.Format(time.RFC3339)) || !strings.Contains(text, "48h0m0s") {
		t.Errorf("the page does not show the schedule: %s", text)
	}
	for _, name := range []string{"nextRotationTime", "rotationPeriod", "versionDestroyTtl"} {
		if !hasField(d.Edit.Fields, name) {
			t.Errorf("Edit secret has no %s field", name)
		}
	}
	// Edit changes the period alone; the unchanged next rotation time is
	// not resent.
	if err := p.Edit(ctx, "demo", []string{sec.Name}, map[string]string{"topics": "projects/demo/topics/t",
		"nextRotationTime": next.Format(time.RFC3339), "rotationPeriod": "2h", "versionDestroyTtl": "48h"}); err != nil {
		t.Fatal(err)
	}
	if sec, _ = st.GetSecret("demo", "r"); sec.RotationPeriod != 2*time.Hour {
		t.Errorf("period after Edit = %v", sec.RotationPeriod)
	}

	path := []string{sec.Name, "1"}
	var destroy *console.Action
	for _, a := range p.DetailActions(ctx, "demo", path) {
		if a.ID == "destroy" {
			destroy = &a
		}
	}
	if destroy == nil || !strings.Contains(destroy.Confirm, "48h0m0s") {
		t.Fatalf("Destroy does not say the destruction is delayed: %+v", destroy)
	}
	if err := p.ActAt(ctx, "demo", path, "destroy", nil); err != nil {
		t.Fatal(err)
	}
	v, _ := st.GetVersion("demo", "r", "1")
	if v.State != secrets.StateDisabled || v.ScheduledDestroy.IsZero() {
		t.Fatalf("version after Destroy = %v %v", v.State, v.ScheduledDestroy)
	}
	vd, _ := p.Detail(ctx, "demo", path)
	if !strings.Contains(detailText(vd), v.ScheduledDestroy.Format(time.RFC3339)) {
		t.Error("the version's page does not show the scheduled destroy time")
	}
	if err := p.ActAt(ctx, "demo", path, "enable", nil); err != nil {
		t.Fatal(err)
	}
	if v, _ = st.GetVersion("demo", "r", "1"); v.State != secrets.StateEnabled || !v.ScheduledDestroy.IsZero() {
		t.Errorf("Enable did not cancel: %v %v", v.State, v.ScheduledDestroy)
	}
	if err := p.ActAt(ctx, "demo", []string{sec.Name}, actSecretAdvanceClock, map[string]string{"duration": "2h"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(events, ","), "SECRET_ROTATE") {
		t.Errorf("Advance clock sent no rotation: %v", events)
	}
}

func hasField(fields []console.Field, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}
