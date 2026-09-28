package main

// Cloud KMS: Edit key (#794).
//
// UpdateCryptoKey is verified with the official client, and the console
// offered no way to call it: a key's labels were fixed once it was created.
// The edit goes through the in-process service's own UpdateCryptoKey, the
// method an SDK's call reaches, so the console accepts what the API accepts
// and refuses what it refuses, with its message.
//
// The form offers only what that UpdateCryptoKey applies. Of the mask paths
// it takes, labels is the one a key can change: the version template it
// accepts is the one every key already has (GOOGLE_SYMMETRIC_ENCRYPTION at
// SOFTWARE), so a field for it could only be saved unchanged. rotation_period
// and next_rotation_time are UNIMPLEMENTED here, because automatic rotation
// is, so they are not offered; purpose and destroy_scheduled_duration are
// immutable in Cloud KMS.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// kmsKeyEditNote names what Edit key does not change, and why.
const kmsKeyEditNote = "Saved through UpdateCryptoKey with the update mask labels. " +
	"The name, purpose, protection level, algorithm and destroy scheduled duration cannot be changed. " +
	"A rotation period and next rotation time are not offered: automatic rotation is not implemented " +
	"on this instance, and UpdateCryptoKey refuses them. Rotate with Add version and Make primary."

// kmsKeyEditForm is Edit key, prefilled from the key as GetCryptoKey returns
// it: its name, shown and fixed, and its labels.
func kmsKeyEditForm(k *kmspb.CryptoKey) *console.EditForm {
	return &console.EditForm{
		Label: "Edit key",
		Fields: []console.Field{
			{Name: "cryptoKeyId", Label: "Key name", Type: "text", Default: lastSegment(k.GetName()), Immutable: true,
				Help: "A key's name cannot be changed."},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(k.GetLabels()),
				Help: "One key=value per line; empty removes every label. A key starts with a lowercase letter; " +
					"keys and values are lowercase letters, digits, - and _, up to 63 characters, and at most 64 labels."},
		},
		Note: kmsKeyEditNote,
	}
}

// Edit implements console.Editor: UpdateCryptoKey with the form's labels, the
// update mask naming labels and nothing else. The labels are always sent, so
// clearing the field clears them.
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
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	_, err = api.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{
		CryptoKey:  &kmspb.CryptoKey{Name: name, Labels: labels},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	})
	return err
}

var _ console.Editor = kmsProvider{}
