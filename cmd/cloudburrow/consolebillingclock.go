package main

// Advance Cloud Billing clock (a CloudBurrow extension) on Budgets &
// alerts (#2109). A budget with a Pub/Sub topic gets its status message
// every notification interval of the Cloud Billing clock
// (internal/service/billing/notify.go): hours away, so tests and the
// console reach it by moving that clock, through
// POST /cloudburrow/v1/clock:advance on the Resource Manager port, and here
// by the UI parity rule, as a list action whose label and help say it is
// local only. Google's console has no such action.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const actBillingClock = "advance-billing-clock"

// ListActions implements console.ListActor.
func (p billingBudgetsProvider) ListActions(context.Context, string) []console.Action {
	return []console.Action{{ID: actBillingClock, Label: "Advance Cloud Billing clock (CloudBurrow extension)",
		Confirm: "This is a CloudBurrow test extension, not a Cloud Billing API: it moves the Cloud Billing clock forward, and it " +
			"can never be moved back. Every budget with a Pub/Sub topic whose next status message falls due is sent it now, once.",
		Fields: []console.Field{
			{Name: "duration", Label: "Advance by", Type: "text", Required: true, Default: "6h", Pattern: advancePattern,
				Help: "Days, hours, minutes and seconds, such as 6h (one notification interval) or 1d. " +
					"A CloudBurrow test extension (POST /cloudburrow/v1/clock:advance)."},
			{Name: "interval", Label: "Notification interval (optional)", Type: "text", Pattern: advancePattern,
				Help: "A new interval between a budget's status messages, such as 1m; empty keeps the current one (6h by default)."},
		}}}
}

// ActAtResult implements console.ResultActor: Advance clock says what it
// did; a budget's actions nothing.
func (p billingBudgetsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) != 0 {
		return nil, p.ActAt(ctx, project, path, action, values)
	}
	if action != actBillingClock {
		return nil, fmt.Errorf("%s is not a Budgets & alerts list action", action)
	}
	s, err := p.api()
	if err != nil {
		return nil, err
	}
	if iv := strings.TrimSpace(values["interval"]); iv != "" {
		d, err := parseAdvance(iv)
		if err != nil {
			return nil, err
		}
		if d < time.Second {
			return nil, fmt.Errorf("the notification interval must be at least 1s")
		}
		s.SetNotifyInterval(d)
	}
	d, err := parseAdvance(values["duration"])
	if err != nil {
		return nil, err
	}
	now, err := s.AdvanceClock(ctx, d)
	if err != nil {
		return nil, err
	}
	return &console.Listing{NameColumn: "Clock", Noun: "clocks", Columns: []string{"Now (UTC)", "Advanced by"}, Total: 1,
		Items: []console.Resource{{Name: "Cloud Billing clock", Fields: map[string]string{
			"Now (UTC)": now.UTC().Format(time.RFC3339), "Advanced by": formatIdle(s.AdvancedBy())}}},
		Note: fmt.Sprintf("The Cloud Billing clock is now %s. Budget notifications now due were published to their topics.",
			now.UTC().Format(time.RFC3339))}, nil
}
