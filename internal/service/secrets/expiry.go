package secrets

// Secret expiry (#1225). Secret.expire_time is "the time at which the
// Secret is scheduled to expire"; ttl is input only, "the TTL for the
// Secret", and is answered as the expire_time it gives
// (google.cloud.secretmanager.v1.Secret, resources.proto). Google deletes
// an expired secret with its versions; so does this store, on its clock:
// every read treats an expired secret as gone and deletes it, and Run
// deletes it at its expire_time, so the Kubernetes Secret holding it goes
// too. Documented, not measured against Google.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// expireOf is a request Secret's expiry: its expire_time, or now plus its
// ttl, zero for neither. Both set is refused: they are one oneof.
func expireOf(sec *secretmanagerpb.Secret, now time.Time) (time.Time, error) {
	if t := sec.GetExpireTime(); t != nil {
		if err := t.CheckValid(); err != nil {
			return time.Time{}, apierror.InvalidArgument("Secret.expire_time is not a valid timestamp: %v", err)
		}
		return t.AsTime().UTC(), nil
	}
	if d := sec.GetTtl(); d != nil {
		if err := d.CheckValid(); err != nil {
			return time.Time{}, apierror.InvalidArgument("Secret.ttl is not a valid duration: %v", err)
		}
		if d.AsDuration() <= 0 {
			return time.Time{}, apierror.InvalidArgument("Secret.ttl must be positive, got %s", d.AsDuration())
		}
		return now.Add(d.AsDuration()).UTC(), nil
	}
	return time.Time{}, nil
}

// wake tells Run an expire_time changed.
func (s *Store) wake() {
	if s.rearm == nil {
		return
	}
	select {
	case s.rearm <- struct{}{}:
	default:
	}
}

// secretParts splits projects/{p}/secrets/{s}.
func secretParts(name string) (project, id string) {
	parts := strings.Split(name, "/")
	switch len(parts) {
	case 4:
		return parts[1], parts[3]
	case 6:
		// A regional secret (#1519): its scope.
		return RegionalScope(parts[1], parts[3]), parts[5]
	}
	return "", ""
}

// Sweep deletes every secret whose expire_time has come, sends the
// rotations and runs the version destructions that are due (#1278), and
// returns the earliest of those times still to come, zero if none.
func (s *Store) Sweep() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.db.List("secret/")
	if err != nil {
		return time.Time{}, apierror.Internal(err, "list secrets")
	}
	now := s.now()
	var next time.Time
	for _, k := range keys {
		raw, err := s.db.Get(k)
		if err != nil {
			continue
		}
		var sec Secret
		if err := json.Unmarshal(raw, &sec); err != nil {
			continue
		}
		if sec.Expire.IsZero() && sec.NextRotation.IsZero() && len(sec.ScheduledDestroys) == 0 {
			continue
		}
		if sec.Expired(now) {
			project, id := secretParts(sec.Name)
			if err := s.deleteSecret(project, id); err != nil {
				return time.Time{}, err
			}
			s.notify(sec, EventSecretDelete, nil, deleteExpiration)
			continue
		}
		// Rotations and scheduled destructions (#1278).
		later, err := s.due(sec, now)
		if err != nil {
			return time.Time{}, err
		}
		for _, t := range []time.Time{sec.Expire, later} {
			if !t.IsZero() && (next.IsZero() || t.Before(next)) {
				next = t
			}
		}
	}
	return next, nil
}

// Run sweeps now and again at each next expire_time, until ctx ends. A
// failed sweep, such as before the cluster holding the store exists, is
// retried with backoff from a second up to a minute.
func (s *Store) Run(ctx context.Context) {
	// Mounted versions under customer-managed keys are re-checked apart
	// from the sweep (#1489).
	go s.runMounts(ctx)
	retry := time.Second
	for {
		next, err := s.Sweep()
		var wait <-chan time.Time
		switch {
		case err != nil:
			wait = s.clock.After(retry)
			if retry *= 2; retry > time.Minute {
				retry = time.Minute
			}
		case !next.IsZero():
			retry = time.Second
			wait = s.clock.After(next.Sub(s.now()))
		default:
			retry = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-s.rearm:
		case <-wait:
		}
	}
}

// Now is the time on the store's clock, from which a ttl counts.
func (s *Store) Now() time.Time { return s.now() }
