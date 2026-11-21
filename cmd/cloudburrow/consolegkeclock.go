package main

// Advance Kubernetes Engine clock (a CloudBurrow extension) on the
// Kubernetes clusters list (#2076).
//
// Google's console has no such button: its clock is the real one. An
// automatic upgrade runs only inside a cluster's maintenance window, and
// one that runs past the window's end is paused until the next, hours or
// a day away; the API reaches them by moving the Kubernetes Engine clock
// (POST /cloudburrow/v1/clock:advance on its port), and here, by the UI
// parity rule, a list action whose label and help say it is local only.

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const actGKEClock = "advance-gke-clock"

const gkeClockConfirm = "This is a CloudBurrow test extension, not a Kubernetes Engine API: it moves the Kubernetes Engine clock " +
	"forward, and it can never be moved back. The clock is the whole instance's: every cluster of every project sees the time move, " +
	"the automatic upgrades whose maintenance window opens start at once, and those running past their window's end pause. " +
	"It lives in memory: cloudburrow up starts it at this machine's time."

// ListActions implements console.ListActor.
func (p gkeClustersProvider) ListActions(context.Context, string) []console.Action {
	a := console.Action{ID: actGKEClock, Label: "Advance Kubernetes Engine clock (CloudBurrow extension)", Confirm: gkeClockConfirm,
		Fields: []console.Field{{Name: "duration", Label: "Advance by", Type: "text", Required: true, Default: "1d",
			Pattern: advancePattern,
			Help: "Days, hours, minutes and seconds, such as 1d (the next daily maintenance window) or 4h. A CloudBurrow test extension: " +
				"Google's Kubernetes Engine has no such call (POST /cloudburrow/v1/clock:advance)."}}}
	if p.gkeAPI() == nil {
		a.Unavailable = gkeNoAPI
	}
	return []console.Action{a}
}

// ActAtResult implements console.ResultActor: the clock says where it is;
// every other action nothing.
func (p gkeClustersProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) != 0 {
		return nil, p.ActAt(ctx, project, path, action, values)
	}
	if action != actGKEClock {
		return nil, fmt.Errorf("%s is not a Kubernetes clusters list action", action)
	}
	api := p.gkeAPI()
	if api == nil {
		return nil, fmt.Errorf("%s", gkeNoAPI)
	}
	d, err := parseAdvance(values["duration"])
	if err != nil {
		return nil, err
	}
	now, err := api.AdvanceClock(d)
	if err != nil {
		return nil, err
	}
	return &console.Listing{NameColumn: "Clock", Noun: "clocks", Columns: []string{"Now (UTC)", "Advanced by"}, Total: 1,
		Items: []console.Resource{{Name: "Kubernetes Engine clock", Fields: map[string]string{
			"Now (UTC)": now.UTC().Format(time.RFC3339), "Advanced by": formatIdle(d)}}}, Note: fmt.Sprintf(
			"The Kubernetes Engine clock is now %s: advanced by %s, %s ahead of this machine's. Automatic upgrades now due have started.",
			now.UTC().Format(time.RFC3339), formatIdle(d), formatIdle(api.AdvancedBy().Round(time.Second)))}, nil
}
