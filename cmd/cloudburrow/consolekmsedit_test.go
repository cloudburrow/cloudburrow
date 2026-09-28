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
// and not sent and no field UpdateCryptoKey refuses here. Saving it is what
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
	if strings.Join(names, ",") != "cryptoKeyId,labels" {
		t.Errorf("Edit key offers %v; want the name (fixed) and labels, the one path UpdateCryptoKey applies here", names)
	}
	values := kmsSubmitted(d.Edit)
	if values["labels"] != `{"env":"dev"}` {
		t.Errorf("labels prefilled as %q", values["labels"])
	}

	// What the form leaves out, UpdateCryptoKey refuses.
	for mask, k := range map[string]*kmspb.CryptoKey{
		"rotation_period":            {Name: key.GetName(), RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(30 * 24 * time.Hour)}},
		"next_rotation_time":         {Name: key.GetName()},
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
