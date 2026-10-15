package main

// Rotation schedules and delayed version destruction on the Secret Manager
// pages (#1278): the Rotation and Delayed destruction fields of Create
// secret and Edit secret, the schedule on the secret's and the version's
// pages, Destroy saying when a delayed destruction happens, and Advance
// clock, the CloudBurrow extension the compat tests use
// (secrets.ClockMethod), on a secret's page.

import (
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

const (
	secretRotationTimeHelp = "Optional. An RFC 3339 time at least 5 minutes from now, such as 2026-12-31T00:00:00Z: " +
		"when a SECRET_ROTATE message is first published to the secret's Pub/Sub topics, which rotation needs. " +
		"Secret Manager does not change the value; your subscriber adds the new version."
	secretRotationPeriodHelp = "Optional. How often after that, such as 720h (30 days); at least 1h. Needs a next rotation time."
	secretDestroyTTLHelp     = "Optional. Delay destruction: destroying a version then disables it and destroys it this " +
		"much later, such as 24h (1 day, the least) up to 24000h (1000 days). Enabling or disabling it before " +
		"then cancels the destruction."
	secretDurationPattern = `^(\d+(\.\d+)?(s|m|h))+$`

	actSecretAdvanceClock = "advance-clock"
	secretAdvanceConfirm  = "This is a CloudBurrow test extension, not a Secret Manager API: it moves Secret Manager's " +
		"clock forward, and it can never be moved back. The clock is the whole instance's, so every rotation, " +
		"scheduled version destruction and expiry of every project due by then happens at once."
)

// secretScheduleFields are the Rotation and Delayed destruction fields; sec
// gives their current values, nil for the Create form.
func secretScheduleFields(sec *secrets.Secret) []console.Field {
	var next, period, ttl string
	if sec != nil {
		next, period, ttl = rotationTimeDefault(*sec), durationDefault(sec.RotationPeriod), durationDefault(sec.VersionDestroyTTL)
	}
	clear := ""
	if sec != nil {
		clear = " Clear it to remove it."
	}
	return []console.Field{
		{Name: "nextRotationTime", Label: "Next rotation time", Type: "text", Section: "Rotation",
			Default: next, Pattern: secretExpireTimePattern, Help: secretRotationTimeHelp + clear},
		{Name: "rotationPeriod", Label: "Rotation period", Type: "text", Section: "Rotation",
			Default: period, Pattern: secretDurationPattern, Help: secretRotationPeriodHelp + clear},
		{Name: "versionDestroyTtl", Label: "Destruction delay", Type: "text", Section: "Delayed destruction",
			Default: ttl, Pattern: secretDurationPattern, Help: secretDestroyTTLHelp + clear},
	}
}

func rotationTimeDefault(sec secrets.Secret) string {
	if sec.NextRotation.IsZero() {
		return ""
	}
	return sec.NextRotation.Format(time.RFC3339)
}

func durationDefault(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

// secretScheduleValue reads the Rotation and Delayed destruction fields.
func secretScheduleValue(values map[string]string) (secrets.Schedule, error) {
	var sch secrets.Schedule
	if at := strings.TrimSpace(values["nextRotationTime"]); at != "" {
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return sch, fmt.Errorf("next rotation time %q is not an RFC 3339 time such as 2026-12-31T00:00:00Z", at)
		}
		sch.NextRotation = t
	}
	for _, f := range []struct {
		name, label string
		into        *time.Duration
	}{{"rotationPeriod", "rotation period", &sch.RotationPeriod}, {"versionDestroyTtl", "destruction delay", &sch.VersionDestroyTTL}} {
		raw := strings.TrimSpace(values[f.name])
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return sch, fmt.Errorf("%s %q is not a positive duration such as 24h", f.label, raw)
		}
		*f.into = d
	}
	return sch, nil
}

// secretScheduleProperties are the schedule as the secret's page shows it.
func secretScheduleProperties(sec secrets.Secret) []console.PropertyGroup {
	next, period, ttl := "None", "None", "None (destroyed at once)"
	if !sec.NextRotation.IsZero() {
		next = sec.NextRotation.Format(time.RFC3339)
	}
	if sec.RotationPeriod > 0 {
		period = sec.RotationPeriod.String()
	}
	if sec.VersionDestroyTTL > 0 {
		ttl = sec.VersionDestroyTTL.String()
	}
	return []console.PropertyGroup{
		{Heading: "Rotation", Properties: []console.Property{
			{Label: "Next rotation time", Value: next}, {Label: "Rotation period", Value: period}}},
		{Heading: "Delayed destruction", Properties: []console.Property{{Label: "Destruction delay", Value: ttl}}},
	}
}

// secretDestroyAction is a version's Destroy, which says what happens on a
// secret with a destruction delay.
func secretDestroyAction(sec secrets.Secret, now time.Time) console.Action {
	a := console.Action{ID: "destroy", Label: "Destroy", Destructive: true}
	if sec.VersionDestroyTTL > 0 {
		a.Label = "Destroy (delayed)"
		a.Confirm = fmt.Sprintf("This secret has a destruction delay of %s: the version is disabled now and destroyed "+
			"at %s, and a SECRET_VERSION_DESTROY_SCHEDULED message goes to its topics. Enable or disable it "+
			"before then to cancel the destruction.", sec.VersionDestroyTTL, now.Add(sec.VersionDestroyTTL).Format(time.RFC3339))
	}
	return a
}

// secretAdvanceClockAction is Advance clock on a secret's page.
func secretAdvanceClockAction() console.Action {
	return console.Action{
		ID: actSecretAdvanceClock, Label: "Advance clock (CloudBurrow extension)", Confirm: secretAdvanceConfirm,
		Fields: []console.Field{{
			Name: "duration", Label: "Advance by", Type: "text", Required: true, Default: "1d",
			Pattern: advancePattern,
			Help: "Days, hours, minutes and seconds, such as 1d or 1h30m. A CloudBurrow test extension: Google's " +
				"Secret Manager has no such call. It moves the whole instance's Secret Manager clock forward and never back.",
		}},
	}
}
