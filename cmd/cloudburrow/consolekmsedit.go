package main

// Cloud KMS: Edit key (#794, #816).
//
// UpdateCryptoKey is verified with the official client, and the console
// offered no way to call it: a key's labels were fixed once it was created.
// The edit goes through the in-process service's own UpdateCryptoKey, the
// method an SDK's call reaches, so the console accepts what the API accepts
// and refuses what it refuses, with its message.
//
// The form offers only what that UpdateCryptoKey applies: labels, and the
// automatic rotation schedule, rotation_period and next_rotation_time (#816).
// The version template it accepts is the one every key already has
// (GOOGLE_SYMMETRIC_ENCRYPTION at SOFTWARE), so a field for it could only be
// saved unchanged; purpose and destroy_scheduled_duration are immutable in
// Cloud KMS. The update mask names exactly the fields the form changed, so a
// save that changes only the period sends only rotation_period. The form does
// not check the period's range itself: a period below the minimum goes to
// UpdateCryptoKey, whose refusal is shown.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// kmsKeyEditNote names what Edit key sends and what it does not change.
const kmsKeyEditNote = "Saved through UpdateCryptoKey with an update mask naming the fields changed here " +
	"(labels, rotation_period, next_rotation_time). " +
	"The name, purpose, protection level, algorithm and destroy scheduled duration cannot be changed. " +
	"At the next rotation time a new version is created and made primary, and the next rotation time " +
	"moves on by the rotation period; Add version and Make primary rotate by hand without moving it."

const (
	kmsRotationPeriodHelp = "Optional. A period such as 30d, 90d or 720h, at least 24 hours (1d) and at most " +
		"876,000 hours; empty for no automatic rotation. A period needs a next rotation time."
	kmsNextRotationHelp = "Optional. An RFC 3339 time such as 2026-10-01T00:00:00Z: when the key next rotates. " +
		"Empty for none."
)

// kmsFormatPeriod shows a rotation period in days when it is whole days, as
// the form accepts it, and otherwise as a Go duration.
func kmsFormatPeriod(d *durationpb.Duration) string {
	if d == nil {
		return ""
	}
	v := d.AsDuration()
	if v > 0 && v%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(v/(24*time.Hour)), 10) + "d"
	}
	return v.String()
}

// kmsParsePeriod reads a rotation period: whole days as Nd, or a Go
// duration. Empty is none (ok false). The range is UpdateCryptoKey's to check.
func kmsParsePeriod(raw string) (d time.Duration, ok bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	if days, isDays := strings.CutSuffix(raw, "d"); isDays {
		n, err := strconv.ParseInt(days, 10, 32)
		if err != nil {
			return 0, false, fmt.Errorf("rotation period: %q is not a period such as 30d or 720h", raw)
		}
		return time.Duration(n) * 24 * time.Hour, true, nil
	}
	d, err = time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("rotation period: %q is not a period such as 30d or 720h", raw)
	}
	return d, true, nil
}

// kmsFormatTime shows a timestamp as the form accepts it, empty for none.
func kmsFormatTime(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// kmsKeyEditForm is Edit key, prefilled from the key as GetCryptoKey returns
// it: its name, shown and fixed, its labels and its rotation schedule.
func kmsKeyEditForm(k *kmspb.CryptoKey) *console.EditForm {
	return &console.EditForm{
		Label: "Edit key",
		Fields: []console.Field{
			{Name: "cryptoKeyId", Label: "Key name", Type: "text", Default: lastSegment(k.GetName()), Immutable: true,
				Help: "A key's name cannot be changed."},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(k.GetLabels()),
				Help: "One key=value per line; empty removes every label. A key starts with a lowercase letter; " +
					"keys and values are lowercase letters, digits, - and _, up to 63 characters, and at most 64 labels."},
			{Name: "rotationPeriod", Label: "Rotation period", Type: "text", Default: kmsFormatPeriod(k.GetRotationPeriod()),
				Section: "Automatic rotation", Help: kmsRotationPeriodHelp},
			{Name: "nextRotationTime", Label: "Next rotation time", Type: "text", Default: kmsFormatTime(k.GetNextRotationTime()),
				Section: "Automatic rotation", Help: kmsNextRotationHelp},
		},
		Note: kmsKeyEditNote,
	}
}

// kmsKeyUpdate is the UpdateCryptoKey request for an Edit key submission
// against cur, the key as it is: the mask names each submitted field whose
// value differs from cur's, and nothing else. A field not submitted is not
// changed. It returns nil when nothing changed.
func kmsKeyUpdate(cur *kmspb.CryptoKey, values map[string]string) (*kmspb.UpdateCryptoKeyRequest, error) {
	in := &kmspb.CryptoKey{Name: cur.GetName()}
	var paths []string
	if raw, ok := values["labels"]; ok {
		labels, err := console.ParseMap(raw)
		if err != nil {
			return nil, fmt.Errorf("labels: %w", err)
		}
		if !maps.Equal(labels, cur.GetLabels()) {
			in.Labels = labels
			paths = append(paths, "labels")
		}
	}
	if raw, ok := values["rotationPeriod"]; ok {
		d, set, err := kmsParsePeriod(raw)
		if err != nil {
			return nil, err
		}
		curPeriod := cur.GetRotationPeriod()
		if set != (curPeriod != nil) || (set && d != curPeriod.AsDuration()) {
			if set {
				in.RotationSchedule = &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(d)}
			}
			paths = append(paths, "rotation_period")
		}
	}
	if raw, ok := values["nextRotationTime"]; ok {
		var next *timestamppb.Timestamp
		if raw = strings.TrimSpace(raw); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return nil, fmt.Errorf("next rotation time: %q is not an RFC 3339 time such as 2026-10-01T00:00:00Z", raw)
			}
			next = timestamppb.New(t)
		}
		curNext := cur.GetNextRotationTime()
		if (next != nil) != (curNext != nil) || (next != nil && !next.AsTime().Equal(curNext.AsTime())) {
			in.NextRotationTime = next
			paths = append(paths, "next_rotation_time")
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	return &kmspb.UpdateCryptoKeyRequest{CryptoKey: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}, nil
}

// Edit implements console.Editor: UpdateCryptoKey with the fields the form
// changed, read against the key as GetCryptoKey returns it. Clearing a field
// clears it.
func (p kmsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	api := p.api()
	if api == nil {
		return errors.New(kmsNotStarted)
	}
	if len(path) != 2 {
		return errors.New("Cloud KMS edits apply to a key")
	}
	ring, err := kmsRing(project, path)
	if err != nil {
		return err
	}
	name := ring + "/cryptoKeys/" + path[1]
	if v, ok := values["cryptoKeyId"]; ok && strings.TrimSpace(v) != path[1] {
		return errors.New("a key's name cannot be changed; create a new key instead")
	}
	cur, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: name})
	if err != nil {
		return err
	}
	req, err := kmsKeyUpdate(cur, values)
	if err != nil || req == nil {
		return err
	}
	_, err = api.UpdateCryptoKey(ctx, req)
	return err
}

var _ console.Editor = kmsProvider{}
