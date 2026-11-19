package main

// Release an update and Advance Memorystore clock (CloudBurrow
// extensions) on the Memorystore for Redis, Redis Cluster and Valkey
// list pages (#1919).
//
// Google's Memorystore console has no equivalent of either: Google
// schedules maintenance itself when it releases a version, and its clock
// is the real one. CloudBurrow's pinned server images never change, so
// nothing releases an update, and a maintenance window a week away is
// reached only by moving the Memorystore clock (#1909). Both are on the
// control endpoint (/cloudburrow/v1/projects/P/locations/L:releaseUpdate
// and /cloudburrow/v1/clock:advance), and here, by the UI parity rule, as
// list actions whose labels and help say they are local only. The clock
// is the whole instance's: advancing it runs every instance's, cluster's
// and Valkey instance's due maintenance and weekly server certificate
// rotation (#1918), of every project, which the confirmation says.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

const (
	actMemorystoreRelease = "release-update"
	actMemorystoreClock   = "advance-memorystore-clock"
)

const memorystoreClockConfirm = "This is a CloudBurrow test extension, not a Memorystore API: it moves the Memorystore clock " +
	"forward, and it can never be moved back. The clock is the whole instance's: every Memorystore for Redis instance, " +
	"Redis cluster and Valkey instance of every project sees the time move, and each maintenance event and weekly server " +
	"certificate rotation it passes is applied at once. It lives in memory: cloudburrow up starts it at this machine's time."

// memorystoreLocalActions are the list pages' two local-only actions.
func memorystoreLocalActions() []console.Action {
	return []console.Action{
		{ID: actMemorystoreRelease, Label: "Release an update (CloudBurrow extension)",
			Confirm: "Local only: Google releases updates itself, and its console has no such button. Every instance, cluster and " +
				"Valkey instance of this project in the region with no maintenance scheduled gets one, at the first start of its " +
				"maintenance window at least a week away, or a week on without a window, as Google notifies a week ahead.",
			Fields: []console.Field{{Name: "region", Label: "Region", Type: "text", Required: true, Default: "-",
				Pattern: `^\s*(-|[a-z]+-[a-z]+[0-9]+)\s*$`,
				Help:    "A region, such as us-central1, or - for every region. A CloudBurrow test extension: POST /cloudburrow/v1/projects/P/locations/L:releaseUpdate."}}},
		{ID: actMemorystoreClock, Label: "Advance Memorystore clock (CloudBurrow extension)", Confirm: memorystoreClockConfirm,
			Fields: []console.Field{{Name: "duration", Label: "Advance by", Type: "text", Required: true, Default: "7d",
				Pattern: advancePattern,
				Help: "Days, hours, minutes and seconds, such as 7d (a week: the next maintenance window, the weekly certificate rotation) " +
					"or 1d12h. A CloudBurrow test extension: Google's Memorystore has no such call (POST /cloudburrow/v1/clock:advance)."}}},
	}
}

// memorystoreLocalAct performs a local-only action on api, and says what
// it did.
func memorystoreLocalAct(ctx context.Context, api *redisadmin.Server, project, action string, values map[string]string) (*console.Listing, error) {
	if api == nil {
		return nil, fmt.Errorf("the Memorystore API is not running: start the instance with --services memorystore")
	}
	switch action {
	case actMemorystoreRelease:
		region := strings.TrimSpace(values["region"])
		if region == "" {
			region = "-"
		}
		names, err := api.ReleaseUpdate(ctx, project, region)
		if err != nil {
			return nil, err
		}
		out := &console.Listing{NameColumn: "Scheduled maintenance on", Noun: "resources"}
		for _, n := range names {
			out.Items = append(out.Items, console.Resource{Name: n})
		}
		out.Total = len(out.Items)
		out.Note = fmt.Sprintf("Released an update: %d resources of %s with no maintenance scheduled now have one. Each one's page shows when.", len(names), project)
		return out, nil
	case actMemorystoreClock:
		d, err := parseAdvance(values["duration"])
		if err != nil {
			return nil, err
		}
		now, err := api.AdvanceClock(d)
		if err != nil {
			return nil, err
		}
		return &console.Listing{NameColumn: "Clock", Noun: "clocks", Columns: []string{"Now (UTC)", "Advanced by"}, Total: 1,
			Items: []console.Resource{{Name: "Memorystore clock", Fields: map[string]string{
				"Now (UTC)": now.UTC().Format(time.RFC3339), "Advanced by": formatIdle(d)}}}, Note: fmt.Sprintf(
				"The Memorystore clock is now %s: advanced by %s, %s ahead of this machine's. Due maintenance and certificate rotations are applied now.",
				now.UTC().Format(time.RFC3339), formatIdle(d), formatIdle(time.Until(now).Round(time.Second)))}, nil
	}
	return nil, fmt.Errorf("%s is not a Memorystore list action", action)
}

// ListActions implements console.ListActor.
func (p memorystoreProvider) ListActions(context.Context, string) []console.Action {
	return memorystoreLocalActions()
}

// ActAtResult implements console.ResultActor: the list actions say what
// they did; every other action nothing.
func (p memorystoreProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 {
		return memorystoreLocalAct(ctx, p.api(), project, action, values)
	}
	return nil, p.ActAt(ctx, project, path, action, values)
}

// ListActions implements console.ListActor.
func (p redisClusterProvider) ListActions(context.Context, string) []console.Action {
	return memorystoreLocalActions()
}

// ActAtResult implements console.ResultActor.
func (p redisClusterProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 {
		_, api, err := p.api()
		if err != nil {
			return nil, err
		}
		return memorystoreLocalAct(ctx, api, project, action, values)
	}
	return nil, p.ActAt(ctx, project, path, action, values)
}

// ListActions implements console.ListActor.
func (p valkeyProvider) ListActions(context.Context, string) []console.Action {
	return memorystoreLocalActions()
}

// ActAtResult implements console.ResultActor.
func (p valkeyProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) == 0 {
		_, api, err := p.api()
		if err != nil {
			return nil, err
		}
		return memorystoreLocalAct(ctx, api, project, action, values)
	}
	return nil, p.ActAt(ctx, project, path, action, values)
}

var (
	_ console.ListActor   = memorystoreProvider{}
	_ console.ResultActor = redisClusterProvider{}
	_ console.ResultActor = valkeyProvider{}
)
