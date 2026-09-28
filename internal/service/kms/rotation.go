package kms

// Automatic rotation (#816).
//
// The rules are resources.proto's, in cloud.google.com/go/kms v1.35.0
// (apiv1/kmspb/resources.pb.go, CryptoKey.next_rotation_time and
// CryptoKey.rotation_period):
//
//   - At next_rotation_time the service creates a new version of the key and
//     marks it primary.
//   - next_rotation_time is advanced by rotation_period when the service
//     rotates a key; the period "must be at least 24 hours and at most 876,000
//     hours" (the rotate-key documentation: "at least 1 day and at most 100
//     years").
//   - "If rotation_period is set, next_rotation_time must also be set."
//   - Rotations done by hand, with CreateCryptoKeyVersion and
//     UpdateCryptoKeyPrimaryVersion, do not affect next_rotation_time.
//   - Only ENCRYPT_DECRYPT keys support automatic rotation; for others both
//     fields must be omitted. Every key here is ENCRYPT_DECRYPT.
//
// Google's code and messages for a refusal are UNVERIFIED: the proto states
// the rules, not the wording, and nothing here calls Google. The refusals are
// INVALID_ARGUMENT naming the field and the rule. What is also UNVERIFIED,
// and chosen here: a next_rotation_time in the past is accepted and rotates
// at the next sweep; a key that was due several periods ago (the service was
// not running) rotates once, and next_rotation_time moves to the first time
// after now on its schedule; next_rotation_time without a period rotates
// once and is then cleared.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const (
	minRotationPeriod = 24 * time.Hour
	maxRotationPeriod = 876000 * time.Hour
)

// rotationPeriodOf validates crypto_key.rotation_period; unset is zero.
func rotationPeriodOf(k *kmspb.CryptoKey) (time.Duration, error) {
	d := k.GetRotationPeriod()
	if d == nil {
		return 0, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, apierror.InvalidArgument("crypto_key.rotation_period is not a valid duration: %v", err)
	}
	v := d.AsDuration()
	if v < minRotationPeriod || v > maxRotationPeriod {
		return 0, apierror.InvalidArgument("crypto_key.rotation_period %s is out of range: it must be at least 24 hours and at most 876,000 hours", v)
	}
	return v, nil
}

// nextRotationOf validates crypto_key.next_rotation_time; unset is zero.
func nextRotationOf(k *kmspb.CryptoKey) (time.Time, error) {
	t := k.GetNextRotationTime()
	if t == nil {
		return time.Time{}, nil
	}
	if err := t.CheckValid(); err != nil {
		return time.Time{}, apierror.InvalidArgument("crypto_key.next_rotation_time is not a valid timestamp: %v", err)
	}
	return t.AsTime().UTC(), nil
}

// checkSchedule refuses a period without a next_rotation_time.
func checkSchedule(period time.Duration, next time.Time) error {
	if period > 0 && next.IsZero() {
		return apierror.InvalidArgument("crypto_key.next_rotation_time must be set when crypto_key.rotation_period is set")
	}
	return nil
}

// advance returns the first time on the schedule from next by period that is
// after now; with no period, zero (the key stops rotating).
func advance(next time.Time, period time.Duration, now time.Time) time.Time {
	if period <= 0 {
		return time.Time{}
	}
	if behind := now.Sub(next); behind >= 0 {
		next = next.Add((behind/period + 1) * period)
	}
	// Sub saturates for spans past ~292 years; finish by stepping.
	for !next.After(now) {
		next = next.Add(period)
	}
	return next
}

// rotateDue rotates every key whose next_rotation_time is at or before now:
// a new ENABLED version from the key's template, made primary, and
// next_rotation_time advanced. It returns the earliest next_rotation_time
// still to come (zero if none). The caller holds s.mu.
func (s *Server) rotateDue(now time.Time) (time.Time, error) {
	keys, err := s.db.List(keyPrefix)
	if err != nil {
		return time.Time{}, fmt.Errorf("list keys: %w", err)
	}
	var next time.Time
	for _, key := range keys {
		b, err := s.db.Get(key)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("read %s: %w", key, err)
		}
		var k cryptoKey
		if err := json.Unmarshal(b, &k); err != nil {
			return time.Time{}, fmt.Errorf("decode %s: %w", key, err)
		}
		if k.NextRotation.IsZero() {
			continue
		}
		if now.Before(k.NextRotation) {
			if next.IsZero() || k.NextRotation.Before(next) {
				next = k.NextRotation
			}
			continue
		}
		mat, err := newMaterial()
		if err != nil {
			return time.Time{}, err
		}
		n := k.Next
		v := keyVersion{Name: versionName(k.Name, n), Created: now.UTC(), Material: mat}
		// The version is written before the key names it, so a failure
		// between leaves an extra version, never a primary that is missing.
		if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
			return time.Time{}, err
		}
		k.Next, k.Primary = n+1, n
		k.NextRotation = advance(k.NextRotation, k.RotationPeriod, now).UTC()
		if err := s.put(key, k); err != nil {
			return time.Time{}, err
		}
		if !k.NextRotation.IsZero() && (next.IsZero() || k.NextRotation.Before(next)) {
			next = k.NextRotation
		}
	}
	return next, nil
}
