package secrets

// Rotation schedules and delayed version destruction (#1278), as
// docs.cloud.google.com/secret-manager/docs/secret-rotation and
// .../docs/delay-destruction-of-secret-versions document them.
//
// Secret.rotation: at next_rotation_time a SECRET_ROTATE notification is
// published to the secret's topics, and next_rotation_time advances by
// rotation_period; with no period it is cleared. Secret Manager does not
// change the secret itself: the subscriber adds the new version. Rotation
// needs topics. next_rotation_time must be at least five minutes ahead when
// set, rotation_period at least an hour, both at most 100 years, and a
// period needs a next_rotation_time (the Secret reference).
//
// Secret.version_destroy_ttl, 1 to 1000 days: DestroySecretVersion then
// disables the version, sets its scheduled_destroy_time and publishes
// SECRET_VERSION_DESTROY_SCHEDULED; at that time the version is destroyed
// (SECRET_VERSION_DESTROY). Enabling or disabling it before then cancels
// the destruction. Changing the ttl affects only later destroys. Deleting
// or expiring the secret destroys every version at once.
//
// Both run on the store's clock, which Advance moves forward (a CloudBurrow
// extension, ClockMethod), so a test can pass the documented minimums in a
// moment. Documented, not measured against Google.

import (
	"context"
	"sort"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The documented limits.
const (
	MinRotationLead      = 5 * time.Minute
	MinRotationPeriod    = time.Hour
	MaxRotation          = 3153600000 * time.Second
	MinVersionDestroyTTL = 24 * time.Hour
	MaxVersionDestroyTTL = 1000 * 24 * time.Hour

	EventSecretRotate                  = "SECRET_ROTATE"
	EventSecretVersionDestroyScheduled = "SECRET_VERSION_DESTROY_SCHEDULED"
)

var (
	errRotationTopics = apierror.InvalidArgument("Secret.rotation needs Secret.topics: " +
		"the SECRET_ROTATE notification is published to the secret's Pub/Sub topics")
	errPeriodNeedsNext = apierror.InvalidArgument("rotation.next_rotation_time must be set when rotation.rotation_period is")
)

// Schedule is a secret's rotation and version_destroy_ttl. Zero is unset.
type Schedule struct {
	NextRotation      time.Time
	RotationPeriod    time.Duration
	VersionDestroyTTL time.Duration
}

// check validates the parts being set: the rotation when rotation, the ttl
// when ttl.
func (sch Schedule) check(now time.Time, rotation, ttl bool) error {
	if rotation && !sch.NextRotation.IsZero() {
		if sch.NextRotation.Before(now.Add(MinRotationLead)) {
			return apierror.InvalidArgument("rotation.next_rotation_time %s must be at least 5 minutes in the future",
				sch.NextRotation.UTC().Format(time.RFC3339Nano))
		}
		if sch.NextRotation.After(now.Add(MaxRotation)) {
			return apierror.InvalidArgument("rotation.next_rotation_time must be at most 100 years in the future")
		}
	}
	if p := sch.RotationPeriod; p != 0 {
		if p < MinRotationPeriod || p > MaxRotation || p%time.Second != 0 {
			return apierror.InvalidArgument("rotation.rotation_period %s must be whole seconds, at least 1 hour and at most 100 years", p)
		}
		if sch.NextRotation.IsZero() && rotation {
			return errPeriodNeedsNext
		}
	}
	if d := sch.VersionDestroyTTL; ttl && d != 0 {
		if d < MinVersionDestroyTTL || d > MaxVersionDestroyTTL {
			return apierror.InvalidArgument("version_destroy_ttl %s must be at least 1 day and at most 1000 days", d)
		}
	}
	return nil
}

// scheduleDestroy disables v and schedules its destruction after sec's
// version_destroy_ttl. The caller holds s.mu.
func (s *Store) scheduleDestroy(sec Secret, v Version) (Version, error) {
	project, id := secretParts(sec.Name)
	at := s.now().Add(sec.VersionDestroyTTL).UTC()
	v.State = StateDisabled
	v.ScheduledDestroy = at
	v.Etag = s.etag()
	if err := s.put(versionKey(project, id, v.Number), v); err != nil {
		return Version{}, err
	}
	if sec.ScheduledDestroys == nil {
		sec.ScheduledDestroys = map[string]time.Time{}
	}
	sec.ScheduledDestroys[strconv.Itoa(v.Number)] = at
	if err := s.put(secretKey(project, id), sec); err != nil {
		return Version{}, err
	}
	s.notify(sec, EventSecretVersionDestroyScheduled, &v, "")
	s.wake()
	return v, nil
}

// unschedule forgets a cancelled destruction. The caller holds s.mu.
func (s *Store) unschedule(project, id string, number int) error {
	sec, err := s.getSecret(project, id)
	if err != nil {
		return err
	}
	delete(sec.ScheduledDestroys, strconv.Itoa(number))
	if len(sec.ScheduledDestroys) == 0 {
		sec.ScheduledDestroys = nil
	}
	return s.put(secretKey(project, id), sec)
}

// due runs what is due on sec at now: its rotation and its scheduled
// destructions. It returns the earliest time still to come on it, zero if
// none. The caller holds s.mu.
func (s *Store) due(sec Secret, now time.Time) (time.Time, error) {
	project, id := secretParts(sec.Name)
	var next time.Time
	soonest := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	changed := false
	if !sec.NextRotation.IsZero() && !now.Before(sec.NextRotation) {
		// One notification however many periods passed, then the next
		// rotation time still to come.
		s.notify(sec, EventSecretRotate, nil, "")
		if sec.RotationPeriod > 0 {
			for !sec.NextRotation.After(now) {
				sec.NextRotation = sec.NextRotation.Add(sec.RotationPeriod)
			}
		} else {
			sec.NextRotation = time.Time{}
		}
		changed = true
	}
	soonest(sec.NextRotation)
	numbers := make([]string, 0, len(sec.ScheduledDestroys))
	for n := range sec.ScheduledDestroys {
		numbers = append(numbers, n)
	}
	sort.Strings(numbers)
	for _, n := range numbers {
		at := sec.ScheduledDestroys[n]
		if now.Before(at) {
			soonest(at)
			continue
		}
		delete(sec.ScheduledDestroys, n)
		changed = true
		number, _ := strconv.Atoi(n)
		v, err := s.getVersion(project, id, number)
		if err != nil || v.ScheduledDestroy.IsZero() || v.State == StateDestroyed {
			continue
		}
		destroyed, err := s.destroyNowQuiet(project, id, v)
		if err != nil {
			return time.Time{}, err
		}
		s.notify(sec, EventSecretVersionDestroy, &destroyed, "")
	}
	if changed {
		if len(sec.ScheduledDestroys) == 0 {
			sec.ScheduledDestroys = nil
		}
		if err := s.put(secretKey(project, id), sec); err != nil {
			return time.Time{}, err
		}
	}
	return next, nil
}

// destroyNowQuiet destroys v without notifying; due notifies with the
// secret it holds. The caller holds s.mu.
func (s *Store) destroyNowQuiet(project, id string, v Version) (Version, error) {
	v.State = StateDestroyed
	v.ScheduledDestroy = time.Time{}
	v.Destroyed = s.now().UTC()
	v.Payload, v.PayloadSize = nil, 0
	// The wrapped data keys go with the payload; the key versions stay,
	// as replication_status (#1453).
	for i := range v.Replicas {
		v.Replicas[i].WrappedKey = nil
	}
	v.Etag = s.etag()
	return v, s.put(versionKey(project, id, v.Number), v)
}

// Advance moves the store's clock forward by d, runs everything that is due
// by then, and returns the new time (#1278). The clock never goes back.
func (s *Store) Advance(d time.Duration) (time.Time, error) {
	if d < 0 {
		return time.Time{}, apierror.InvalidArgument("the clock only moves forward: %s", d)
	}
	s.offset.Add(int64(d))
	_, err := s.Sweep()
	s.wake()
	return s.now(), err
}

// ClockMethod advances Secret Manager's clock, a CloudBurrow extension on
// the service's gRPC port with no Google equivalent. Its request is a
// google.protobuf.Duration, which must not be negative (zero reads the
// clock); its response is the time after the advance, a
// google.protobuf.Timestamp, once every rotation, destruction and expiry
// due by then has run.
const ClockMethod = "/cloudburrow.secretmanager.v1.EmulatorClock/Advance"

type clockServer interface {
	advance(context.Context, *durationpb.Duration) (*timestamppb.Timestamp, error)
}

func (g *GRPCServer) advance(_ context.Context, d *durationpb.Duration) (*timestamppb.Timestamp, error) {
	if err := d.CheckValid(); err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("the request is not a valid duration: %v", err))
	}
	now, err := g.store.Advance(d.AsDuration())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return timestamppb.New(now), nil
}

var clockServiceDesc = grpc.ServiceDesc{
	ServiceName: "cloudburrow.secretmanager.v1.EmulatorClock",
	HandlerType: (*clockServer)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Advance",
		Handler: func(srv any, ctx context.Context, dec func(any) error, icpt grpc.UnaryServerInterceptor) (any, error) {
			in := new(durationpb.Duration)
			if err := dec(in); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, req any) (any, error) {
				return srv.(clockServer).advance(ctx, req.(*durationpb.Duration))
			}
			if icpt == nil {
				return call(ctx, in)
			}
			return icpt(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: ClockMethod}, call)
		},
	}},
	Metadata: "cloudburrow/secretmanager/clock",
}
