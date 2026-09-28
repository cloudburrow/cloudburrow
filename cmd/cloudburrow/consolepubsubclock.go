package main

// Advance clock (CloudBurrow extension) on a subscription's page (#1040).
//
// The Pub/Sub front (internal/pubsubfront, #873) deletes a subscription
// idle for its expiration period by its own clock, the wall clock plus an
// offset that only grows, and serves one CloudBurrow method that moves it:
// EmulatorClock/Advance, which advances the clock and sweeps before it
// answers. Google has no such call; it is a test extension, and the action
// says so in its label. The clock is the whole instance's: advancing it ages
// every subscription of every project, which the confirmation says before
// anything is sent.
//
// The method answers with the new time only. Which subscriptions the sweep
// deleted is read around it: the front's activity records (which subscriptions
// it tracks) and ListSubscriptions of each project they belong to, before and
// after. A subscription that existed before, was tracked, and is neither
// after, expired. Neither read is activity on a subscription (the front
// counts only calls naming one), so the reads do not change what expires.
// Opening the page and offering its actions read this subscription
// (GetSubscription), which is activity, so its own clock restarted then: it
// expires only when the advance is at least its expiration period.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

const actAdvanceClock = "advance-clock"

// advanceConfirm is what advancing the clock puts at stake, shown in the
// confirmation that asks for the subscription's name back.
const advanceConfirm = "This is a CloudBurrow test extension, not a Pub/Sub API: it moves the Pub/Sub front's clock " +
	"forward, and it can never be moved back. The clock is the whole instance's, so every subscription of every " +
	"project ages by this much, and each one idle for its expiration period by then is deleted at once, with its " +
	"messages. Opening this page was activity on this subscription, so it expires only if the advance is at least " +
	"its expiration period."

// advancePattern is a duration the form takes: days, hours, minutes and
// seconds, largest first, each at most once, such as 1d, 31d or 1d12h30m.
const advancePattern = `^\s*(?:[0-9]+d)?(?:[0-9]+h)?(?:[0-9]+m)?(?:[0-9]+s)?\s*$`

var advanceRE = regexp.MustCompile(`^(?:([0-9]+)d)?(?:([0-9]+)h)?(?:([0-9]+)m)?(?:([0-9]+)s)?$`)

// advanceClockAction is the action on a subscription's page.
func advanceClockAction() console.Action {
	return console.Action{
		ID: actAdvanceClock, Label: "Advance clock (CloudBurrow extension)", Confirm: advanceConfirm,
		Fields: []console.Field{{
			Name: "duration", Label: "Advance by", Type: "text", Required: true, Default: "1d",
			Pattern: advancePattern,
			Help: "Days, hours, minutes and seconds, such as 1d, 31d (the default expiration period) or 1d12h. " +
				"A CloudBurrow test extension: Google's Pub/Sub has no such call. It moves the whole instance's " +
				"Pub/Sub clock forward, for every subscription of every project, and never back.",
		}},
	}
}

// parseAdvance reads the form's duration, which must be more than zero.
func parseAdvance(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	m := advanceRE.FindStringSubmatch(s)
	if s == "" || m == nil {
		return 0, fmt.Errorf("%q is not a duration such as 1d, 36h or 1d12h30m", raw)
	}
	var total time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > int64((1<<63-1)/unit) || total > time.Duration(1<<63-1)-time.Duration(n)*unit {
			return 0, fmt.Errorf("%q is longer than the clock can be advanced by", raw)
		}
		total += time.Duration(n) * unit
	}
	if total <= 0 {
		return 0, errors.New("the clock only moves forward: advance it by more than 0s")
	}
	return total, nil
}

// ActAtResult implements console.ResultActor: Advance clock answers with
// the subscriptions it expired; every other action with nothing.
func (p pubsubSubscriptionsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if action != actAdvanceClock {
		return nil, p.ActAt(ctx, project, path, action, values)
	}
	if len(path) != 1 || !strings.HasPrefix(path[0], "projects/"+project+"/subscriptions/") {
		return nil, fmt.Errorf("the clock is advanced from a subscription of project %s", project)
	}
	d, err := parseAdvance(values["duration"])
	if err != nil {
		return nil, err
	}
	return advancePubSubClock(ctx, p.endpoint, path[0], d)
}

// advancePubSubClock advances the front's clock by d, and reports the new
// time and the subscriptions its sweep deleted. self is the page's
// subscription, which the result names if it was one of them.
func advancePubSubClock(ctx context.Context, endpoint, self string, d time.Duration) (*console.Listing, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	admin, err := vkit.NewSubscriptionAdminClient(ctx, localOpts(endpoint)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = admin.Close() }()

	tracked, err := trackedSubscriptions(ctx, conn)
	if err != nil {
		return nil, err
	}
	existed, err := projectSubscriptions(ctx, admin, tracked)
	if err != nil {
		return nil, err
	}
	var now timestamppb.Timestamp
	if err := conn.Invoke(ctx, pubsubfront.ClockMethod, durationpb.New(d), &now); err != nil {
		return nil, clockRefusal(err)
	}
	stillTracked, err := trackedSubscriptions(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("the clock was advanced to %s, but reading what expired failed: %w",
			now.AsTime().UTC().Format(time.RFC3339), err)
	}
	remain, err := projectSubscriptions(ctx, admin, tracked)
	if err != nil {
		return nil, fmt.Errorf("the clock was advanced to %s, but reading what expired failed: %w",
			now.AsTime().UTC().Format(time.RFC3339), err)
	}
	var expired []string
	for name := range tracked {
		if existed[name] && !stillTracked[name] && !remain[name] {
			expired = append(expired, name)
		}
	}
	sort.Strings(expired)

	out := &console.Listing{Columns: []string{"Project"}, NameColumn: "Expired subscription", Noun: "subscriptions"}
	for _, name := range expired {
		out.Items = append(out.Items, console.Resource{Name: name, Fields: map[string]string{"Project": subscriptionProject(name)}})
	}
	out.Total = len(out.Items)
	at := now.AsTime().UTC()
	note := fmt.Sprintf("The Pub/Sub front's clock is now %s: advanced by %s, %s ahead of this machine's.",
		at.Format(time.RFC3339), formatIdle(d), formatIdle(time.Until(at).Round(time.Second)))
	switch n := len(expired); {
	case n == 0:
		note += " No subscription expired."
	case n == 1:
		note += " 1 subscription was idle for its expiration period and expired: it is deleted."
	default:
		note += fmt.Sprintf(" %d subscriptions were idle for their expiration period and expired: they are deleted.", n)
	}
	for _, name := range expired {
		if name == self {
			note += " This subscription is one of them, so its page is gone."
		}
	}
	out.Note = note
	return out, nil
}

// clockRefusal says why the clock could not be advanced.
func clockRefusal(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errors.New("this Pub/Sub endpoint has no CloudBurrow front, which keeps the clock that expiration uses")
	}
	if s, ok := status.FromError(err); ok {
		return errors.New(s.Message())
	}
	return err
}

// trackedSubscriptions are the subscriptions the front keeps a clock for.
func trackedSubscriptions(ctx context.Context, conn *grpc.ClientConn) (map[string]bool, error) {
	var idle structpb.Struct
	if err := conn.Invoke(ctx, pubsubfront.ActivityExportMethod, &emptypb.Empty{}, &idle); err != nil {
		return nil, clockRefusal(err)
	}
	out := map[string]bool{}
	for name := range idle.GetFields() {
		out[name] = true
	}
	return out, nil
}

// projectSubscriptions are the subscriptions that exist in the projects of
// names, through ListSubscriptions, which names no subscription and so is not
// activity on one.
func projectSubscriptions(ctx context.Context, admin *vkit.SubscriptionAdminClient, names map[string]bool) (map[string]bool, error) {
	projects := map[string]bool{}
	for name := range names {
		if pr := subscriptionProject(name); pr != "" {
			projects[pr] = true
		}
	}
	out := map[string]bool{}
	for pr := range projects {
		it := admin.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + pr})
		for {
			s, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("list the subscriptions of %s: %w", pr, err)
			}
			out[s.GetName()] = true
		}
	}
	return out, nil
}

// subscriptionProject is the project of projects/{project}/subscriptions/{id}.
func subscriptionProject(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) == 4 && parts[0] == "projects" && parts[2] == "subscriptions" {
		return parts[1]
	}
	return ""
}

var _ console.ResultActor = pubsubSubscriptionsProvider{}
