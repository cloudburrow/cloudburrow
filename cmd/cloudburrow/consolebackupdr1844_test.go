package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A backup rule's week day of the month (gcloud's week-day-of-month,
// FIRST-SUNDAY), for a monthly or yearly rule instead of its days (#1844).
func TestBackupDRRuleWeekDayOfMonth(t *testing.T) {
	rules, err := bdrRulesFrom(console.FormatGroup([]map[string]string{
		{"ruleId": "monthly", "recurrence": "MONTHLY", "weekDayOfMonth": "LAST-SUNDAY", "start": "0", "end": "24", "retention": "30"},
		{"ruleId": "yearly", "recurrence": "YEARLY", "weekDayOfMonth": "first mon", "months": "MAR", "start": "0", "end": "24", "retention": "30"},
	}), "UTC", false)
	if err != nil {
		t.Fatal(err)
	}
	sch := rules[0].(map[string]any)["standardSchedule"].(map[string]any)
	w, _ := sch["weekDayOfMonth"].(map[string]any)
	if w["weekOfMonth"] != "LAST" || w["dayOfWeek"] != "SUNDAY" || sch["daysOfMonth"] != nil {
		t.Errorf("LAST-SUNDAY: %+v", sch)
	}
	ysch := rules[1].(map[string]any)["standardSchedule"].(map[string]any)
	if yw, _ := ysch["weekDayOfMonth"].(map[string]any); yw["weekOfMonth"] != "FIRST" || yw["dayOfWeek"] != "MONDAY" || ysch["months"] == nil {
		t.Errorf("a yearly first Monday: %+v", ysch)
	}
	// What the plan's page reads.
	disp := map[string]any{"standardSchedule": map[string]any{"recurrenceType": "MONTHLY",
		"weekDayOfMonth": map[string]any{"weekOfMonth": "LAST", "dayOfWeek": "SUNDAY"}}}
	if got := bdrRecurrence(disp); got != "Monthly, on the last Sunday" {
		t.Errorf("the rule reads %q", got)
	}
	for _, bad := range []map[string]string{
		{"ruleId": "a", "recurrence": "WEEKLY", "daysOfWeek": "MON", "weekDayOfMonth": "FIRST-MONDAY", "start": "0", "end": "24", "retention": "7"},
		{"ruleId": "a", "recurrence": "MONTHLY", "daysOfMonth": "1", "weekDayOfMonth": "FIRST-MONDAY", "start": "0", "end": "24", "retention": "7"},
		{"ruleId": "a", "recurrence": "MONTHLY", "weekDayOfMonth": "FIFTH-MONDAY", "start": "0", "end": "24", "retention": "7"},
		{"ruleId": "a", "recurrence": "MONTHLY", "weekDayOfMonth": "LAST-FUNDAY", "start": "0", "end": "24", "retention": "7"},
	} {
		if _, err := bdrRulesFrom(console.FormatGroup([]map[string]string{bad}), "UTC", false); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// A backup's Delete is drawn disabled with the reason while its enforced
// retention lasts, and offered once it has passed (#1844).
func TestBackupDRBackupDeleteDisabledInsideEnforcedRetention(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b := map[string]any{"enforcedRetentionEndTime": now.Add(24 * time.Hour).Format(time.RFC3339Nano)}
	a := bdrBackupDelete(b, now)
	if !strings.Contains(a.Unavailable, "Nov 21, 2026, 12:00 UTC") || a.ConfirmStyle != "confirm" {
		t.Errorf("inside the enforced retention: %+v", a)
	}
	if a := bdrBackupDelete(b, now.Add(25*time.Hour)); a.Unavailable != "" {
		t.Errorf("after the enforced retention: %+v", a)
	}
}
