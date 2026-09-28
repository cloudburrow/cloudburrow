package main

import (
	"context"
	"strings"
	"testing"
	"time"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// kmsEditFixture is a started KMS service, the console's provider over it and
// the official client against its port.
func kmsEditFixture(t *testing.T) (kmsProvider, *kmsapi.KeyManagementClient) {
	t.Helper()
	ctx := context.Background()
	svc := &kmsService{cfg: config.Config{BindAddress: "127.0.0.1"}, db: store.NewMemory()}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	c, err := kmsapi.NewKeyManagementClient(ctx, clientOpts(svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return kmsProvider{svc: svc}, c
}

// kmsSubmitted is what the browser submits for an edit form: every field's
// default except the immutable ones, which it does not send.
func kmsSubmitted(form *console.EditForm) map[string]string {
	out := map[string]string{}
	for _, f := range form.Fields {
		if !f.Immutable {
			out[f.Name] = f.Default
		}
	}
	return out
}

// A key's page carries Edit key, prefilled from the key, with the name shown
// and not sent and no field UpdateCryptoKey refuses here (#794, #816). Saving it is what
// the official client's GetCryptoKey reads; clearing the labels clears them.
// A label UpdateCryptoKey refuses is refused with its own message and changes
// nothing (#794).
func TestKMSEditKeyThroughUpdateCryptoKey(t *testing.T) {
	ctx := context.Background()
	p, c := kmsEditFixture(t)
	const project = "edit-proj"
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, Labels: map[string]string{"env": "dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	path := []string{ring.GetName(), "k"}

	d, err := p.Detail(ctx, project, path)
	if err != nil || d.Edit == nil {
		t.Fatalf("the key's page offers no edit form: %v %+v", err, d)
	}
	if d.Edit.Label != "Edit key" || !strings.Contains(d.Edit.Note, "rotation") {
		t.Errorf("edit form %q, note %q", d.Edit.Label, d.Edit.Note)
	}
	var names []string
	for _, f := range d.Edit.Fields {
		names = append(names, f.Name)
		if (f.Name == "cryptoKeyId") != f.Immutable {
			t.Errorf("field %s immutable = %v", f.Name, f.Immutable)
		}
	}
	if strings.Join(names, ",") != "cryptoKeyId,labels,rotationPeriod,nextRotationTime" {
		t.Errorf("Edit key offers %v; want the name (fixed), labels and the rotation schedule, the paths UpdateCryptoKey applies here", names)
	}
	values := kmsSubmitted(d.Edit)
	if values["labels"] != `{"env":"dev"}` || values["rotationPeriod"] != "" || values["nextRotationTime"] != "" {
		t.Errorf("prefilled with %v; want the labels and no rotation schedule", values)
	}

	// What the form leaves out, UpdateCryptoKey refuses.
	for mask, k := range map[string]*kmspb.CryptoKey{
		"destroy_scheduled_duration": {Name: key.GetName(), DestroyScheduledDuration: durationpb.New(48 * time.Hour)},
		"purpose":                    {Name: key.GetName(), Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT},
	} {
		if _, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: k,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{mask}}}); err == nil {
			t.Errorf("UpdateCryptoKey accepted %s, which the form does not offer", mask)
		}
	}

	values["labels"] = `{"env":"prod","team":"payments"}`
	if err := p.Edit(ctx, project, path, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if l := got.GetLabels(); len(l) != 2 || l["env"] != "prod" || l["team"] != "payments" {
		t.Errorf("GetCryptoKey reads labels %v after the edit", l)
	}
	if got.GetPrimary().GetName() != key.GetPrimary().GetName() {
		t.Errorf("the edit moved the primary from %s to %s", key.GetPrimary().GetName(), got.GetPrimary().GetName())
	}
	if d, _ := p.Detail(ctx, project, path); kmsSubmitted(d.Edit)["labels"] != `{"env":"prod","team":"payments"}` {
		t.Errorf("the form after saving is prefilled with %v", kmsSubmitted(d.Edit))
	}

	// Refused by UpdateCryptoKey with the message the official client gets.
	_, sdkErr := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{
		CryptoKey:  &kmspb.CryptoKey{Name: key.GetName(), Labels: map[string]string{"Env": "x"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	want, _ := status.FromError(sdkErr)
	if want.Code() != codes.InvalidArgument {
		t.Fatalf("the official client's UpdateCryptoKey with label key Env = %v; want INVALID_ARGUMENT", sdkErr)
	}
	bad := map[string]string{"labels": `{"Env":"x"}`}
	err = p.Edit(ctx, project, path, bad)
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != want.Message() {
		t.Errorf("label key Env refused with %v; want UpdateCryptoKey's %q", err, want.Message())
	}
	if got, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); got.GetLabels()["env"] != "prod" {
		t.Errorf("after a refused edit GetCryptoKey reads labels %v", got.GetLabels())
	}

	// Refused before UpdateCryptoKey: a rename, a map that is not one,
	// another project's key, a version rather than a key.
	for what, v := range map[string]map[string]string{
		"a rename":       {"cryptoKeyId": "other", "labels": ""},
		"malformed JSON": {"labels": "env=prod"},
	} {
		if err := p.Edit(ctx, project, path, v); err == nil {
			t.Errorf("an edit with %s was accepted", what)
		}
	}
	if err := p.Edit(ctx, "other-proj", path, values); err == nil {
		t.Error("an edit of another project's key was accepted")
	}
	if err := p.Edit(ctx, project, []string{ring.GetName(), "k", "1"}, values); err == nil {
		t.Error("an edit of a version was accepted")
	}
	if err := p.Edit(ctx, project, []string{ring.GetName(), "missing"}, values); status.Code(err) != codes.NotFound {
		t.Errorf("editing a key that does not exist = %v; want NOT_FOUND", err)
	}

	// Empty labels clear them.
	if err := p.Edit(ctx, project, path, map[string]string{"labels": ""}); err != nil {
		t.Fatalf("Edit with no labels: %v", err)
	}
	if got, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); len(got.GetLabels()) != 0 {
		t.Errorf("clearing the labels left %v", got.GetLabels())
	}
}

// Only a key's page offers the form: a ring's and a version's do not, since
// UpdateCryptoKey edits a key (#794).
func TestKMSEditOfferedOnlyOnAKey(t *testing.T) {
	ctx := context.Background()
	p, c := kmsEditFixture(t)
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/proj-one/locations/global", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{{ring.GetName()}, {ring.GetName(), "k", "1"}} {
		if d, err := p.Detail(ctx, "proj-one", path); err != nil || d.Edit != nil {
			t.Errorf("%v offers an edit form (%v)", path, err)
		}
	}
	if d, err := p.Detail(ctx, "proj-one", []string{ring.GetName(), "k"}); err != nil || d.Edit == nil || kmsSubmitted(d.Edit)["labels"] != "" {
		t.Errorf("a key with no labels: edit %+v (%v)", d.Edit, err)
	}
}

// Edit key sets, changes and clears the rotation schedule through
// UpdateCryptoKey, and GetCryptoKey through the official client reads what
// was saved; the form is prefilled with it. A period below the minimum, and a
// period with no next rotation time, are refused with the message the
// official client's own UpdateCryptoKey receives, and change nothing (#816).
func TestKMSEditKeyRotationThroughUpdateCryptoKey(t *testing.T) {
	ctx := context.Background()
	p, c := kmsEditFixture(t)
	const project = "rot-proj"
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	path := []string{ring.GetName(), "k"}
	next := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	read := func() *kmspb.CryptoKey {
		t.Helper()
		k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	sdkRefusal := func(k *kmspb.CryptoKey, paths ...string) *status.Status {
		t.Helper()
		k.Name = key.GetName()
		_, err := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{CryptoKey: k, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		st, _ := status.FromError(err)
		if st.Code() != codes.InvalidArgument {
			t.Fatalf("the official client's UpdateCryptoKey %v = %v; want INVALID_ARGUMENT", paths, err)
		}
		return st
	}

	// A period with no next rotation time: the API's refusal.
	want := sdkRefusal(&kmspb.CryptoKey{RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(30 * 24 * time.Hour)}}, "rotation_period")
	err = p.Edit(ctx, project, path, map[string]string{"labels": "", "rotationPeriod": "30d", "nextRotationTime": ""})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != want.Message() {
		t.Errorf("a period with no next rotation time = %v; want UpdateCryptoKey's %q", err, want.Message())
	}

	values := map[string]string{"labels": "", "rotationPeriod": "30d", "nextRotationTime": next.Format(time.RFC3339)}
	if err := p.Edit(ctx, project, path, values); err != nil {
		t.Fatalf("Edit setting the schedule: %v", err)
	}
	if k := read(); k.GetRotationPeriod().AsDuration() != 30*24*time.Hour || !k.GetNextRotationTime().AsTime().Equal(next) {
		t.Errorf("GetCryptoKey reads rotation_period %v, next_rotation_time %v; want 720h and %v", k.GetRotationPeriod(), k.GetNextRotationTime(), next)
	}
	d, err := p.Detail(ctx, project, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := kmsSubmitted(d.Edit); got["rotationPeriod"] != "30d" || got["nextRotationTime"] != next.Format(time.RFC3339) {
		t.Errorf("the form after saving is prefilled with %v", got)
	}

	// Below the minimum: the API's refusal, from the SDK and from the form.
	want = sdkRefusal(&kmspb.CryptoKey{RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(23 * time.Hour)}}, "rotation_period")
	values["rotationPeriod"] = "23h"
	err = p.Edit(ctx, project, path, values)
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != want.Message() {
		t.Errorf("a period of 23h = %v; want UpdateCryptoKey's %q", err, want.Message())
	}
	if !strings.Contains(want.Message(), "at least 24 hours") {
		t.Errorf("the API's refusal %q does not name the minimum", want.Message())
	}
	if k := read(); k.GetRotationPeriod().AsDuration() != 30*24*time.Hour {
		t.Errorf("a refused edit left rotation_period %v", k.GetRotationPeriod())
	}

	// A Go duration is accepted too; clearing both clears the schedule.
	values["rotationPeriod"] = "48h"
	if err := p.Edit(ctx, project, path, values); err != nil || read().GetRotationPeriod().AsDuration() != 48*time.Hour {
		t.Errorf("a period of 48h: %v, reads %v", err, read().GetRotationPeriod())
	}
	if err := p.Edit(ctx, project, path, map[string]string{"labels": "", "rotationPeriod": "", "nextRotationTime": ""}); err != nil {
		t.Fatalf("clearing the schedule: %v", err)
	}
	if k := read(); k.GetRotationPeriod() != nil || k.GetNextRotationTime() != nil {
		t.Errorf("after clearing: rotation_period %v, next_rotation_time %v", k.GetRotationPeriod(), k.GetNextRotationTime())
	}

	// Refused before UpdateCryptoKey: what is not a period or a time.
	for what, v := range map[string]map[string]string{
		"a period that is not one": {"rotationPeriod": "monthly"},
		"fractional days":          {"rotationPeriod": "1.5d"},
		"a time that is not one":   {"nextRotationTime": "tomorrow"},
	} {
		if err := p.Edit(ctx, project, path, v); err == nil {
			t.Errorf("an edit with %s was accepted", what)
		}
	}
}

// The update mask names exactly the fields the form changed (#816).
func TestKMSEditKeyMaskNamesOnlyWhatChanged(t *testing.T) {
	next := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cur := &kmspb.CryptoKey{Name: "projects/p/locations/global/keyRings/r/cryptoKeys/k", Labels: map[string]string{"env": "dev"},
		RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(30 * 24 * time.Hour)},
		NextRotationTime: timestamppb.New(next)}
	form := kmsSubmitted(kmsKeyEditForm(cur))
	with := func(k, v string) map[string]string {
		out := map[string]string{}
		for a, b := range form {
			out[a] = b
		}
		out[k] = v
		return out
	}
	for what, c := range map[string]struct {
		values map[string]string
		mask   string
	}{
		"nothing":                 {form, ""},
		"the period in hours":     {with("rotationPeriod", "720h"), ""},
		"the labels":              {with("labels", `{"env":"prod"}`), "labels"},
		"the period":              {with("rotationPeriod", "90d"), "rotation_period"},
		"the next rotation time":  {with("nextRotationTime", "2026-11-01T00:00:00Z"), "next_rotation_time"},
		"the same time elsewhere": {with("nextRotationTime", "2026-10-01T02:00:00+02:00"), ""},
		"cleared period":          {with("rotationPeriod", ""), "rotation_period"},
		"only the period sent":    {map[string]string{"rotationPeriod": "7d"}, "rotation_period"},
	} {
		req, err := kmsKeyUpdate(cur, c.values)
		if err != nil {
			t.Errorf("%s: %v", what, err)
			continue
		}
		if got := strings.Join(req.GetUpdateMask().GetPaths(), ","); got != c.mask {
			t.Errorf("changing %s sends the mask %q; want %q", what, got, c.mask)
		}
		if c.mask == "" && req != nil {
			t.Errorf("changing %s sends a request", what)
		}
	}
	req, _ := kmsKeyUpdate(cur, with("rotationPeriod", "90d"))
	if req.GetCryptoKey().GetRotationPeriod().AsDuration() != 90*24*time.Hour || req.GetCryptoKey().GetName() != cur.GetName() {
		t.Errorf("the request for a period of 90d is %v", req)
	}
}
