package logging

// Default customer-managed keys (#1972), as
// https://docs.cloud.google.com/logging/docs/routing/managed-encryption and
// the UpdateSettings and UpdateCmekSettings references describe them:
//
//   - Settings.kms_key_name and CmekSettings.kms_key_name are one setting
//     per parent, the "CMEK for the Log Router": GetSettings and
//     GetCmekSettings both report it, UpdateSettings (organizations and
//     folders, and projects here) and UpdateCmekSettings (organizations
//     only, "CMEK for the Log Router can currently only be configured for
//     Google Cloud organizations") set it, and an empty name clears it ("To
//     disable CMEK for the Log Router, set this field to an empty string").
//   - Setting it fails when the name is invalid (INVALID_ARGUMENT), the
//     key's location is not the settings' storage_location ("The default
//     storage location ... must equal the key's location"; INVALID_ARGUMENT),
//     or the key is not usable by kms_service_account_id: disabled, or
//     without roles/cloudkms.cryptoKeyEncrypterDecrypter when IAM is
//     enforced (FAILED_PRECONDITION, cmek.go's wording).
//   - CmekSettings.kms_key_version_name is the key's primary version when it
//     was set.
//   - "A custom bucket in a child resource uses the default key unless you
//     specify a different key when creating it": CreateBucket without
//     cmek_settings, in a parent whose own settings or nearest ancestor's
//     (folders, then the organization) have a key, gives the bucket that
//     key, and refuses a bucket in global ("You can't create log buckets in
//     the global region" once a CMEK default is set) or in another location
//     than the key's (INVALID_ARGUMENT, the bucket rule). Existing buckets
//     are not changed.
//
// Not served: the _Default and _Required buckets of new projects are not
// re-created in the key's location (CloudBurrow's are global and implicit),
// and the Log Router keeps no buffer of its own to encrypt.

import (
	"context"
	"errors"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func settingsKeyVersionKey(p parent) string { return "logging/settingscmek/" + p.seg() }

// settingsKeyVersion is the key version p's default key was set with, or
// "".
func (a *Admin) settingsKeyVersion(p parent) string {
	raw, err := a.db.Get(settingsKeyVersionKey(p))
	if err != nil {
		return ""
	}
	return string(raw)
}

// checkSettingsKey checks key can be p's default key, and returns its
// version: it is a CryptoKey that p's Logging service account can encrypt
// and decrypt with. The caller holds a.mu.
func (a *Admin) checkSettingsKey(ctx context.Context, p parent, key string) (string, error) {
	if _, ok := cmek.Location(key); !ok {
		return "", apierror.InvalidArgument("kms_key_name %q must be projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}", key)
	}
	agent := serviceAccount(p)
	if err := a.cmekKeys().Usable(ctx, p.id, agent, key); err != nil {
		return "", keyError(agent, err)
	}
	probe, err := a.cmekKeys().Seal(ctx, p.id, agent, key, nil, []byte(p.String()+"/settings"))
	if err != nil {
		return "", keyError(agent, err)
	}
	return probe.KeyVersion, nil
}

// putSettings stores st's settable fields for p, and the default key's
// version ("" when it has none). The caller holds a.mu.
func (a *Admin) putSettings(p parent, st *loggingpb.Settings, version string) error {
	raw, err := protojson.Marshal(&loggingpb.Settings{KmsKeyName: st.GetKmsKeyName(), StorageLocation: st.GetStorageLocation(),
		DisableDefaultSink: st.GetDisableDefaultSink()})
	if err != nil {
		return apierror.Internal(err, "encoding the settings")
	}
	if err := a.db.Put(settingsKey(p), raw); err != nil {
		return apierror.Internal(err, "writing the settings")
	}
	if version == "" {
		if err := a.db.Delete(settingsKeyVersionKey(p)); err != nil && !errors.Is(err, store.ErrNotFound) {
			return apierror.Internal(err, "writing the settings")
		}
		return nil
	}
	if err := a.db.Put(settingsKeyVersionKey(p), []byte(version)); err != nil {
		return apierror.Internal(err, "writing the settings")
	}
	return nil
}

// defaultKey is the default customer-managed key a new bucket of p gets:
// p's own settings', else its nearest ancestor's with one; "" for none. The
// caller holds a.mu.
func (a *Admin) defaultKey(p parent) string {
	for _, q := range append([]parent{p}, a.ancestors(p)...) {
		if st, err := a.settings(q); err == nil && st.GetKmsKeyName() != "" {
			return st.GetKmsKeyName()
		}
	}
	return ""
}

// applyDefaultKey gives a new bucket without cmek_settings its parent's
// default key. The caller holds a.mu.
func (a *Admin) applyDefaultKey(ref bucketRef, b *loggingpb.LogBucket) error {
	if b.GetCmekSettings() != nil {
		return nil
	}
	key := a.defaultKey(ref.parent)
	if key == "" {
		return nil
	}
	if ref.location == "global" {
		return apierror.InvalidArgument("a default CMEK key (%s) is set for %s: log buckets can't be created in the global location; create the bucket in %s", key, ref.parent, mustLocation(key))
	}
	b.CmekSettings = &loggingpb.CmekSettings{KmsKeyName: key}
	return nil
}

func mustLocation(key string) string {
	l, _ := cmek.Location(key)
	return l
}

// orNone is v, or "(none)" when it is empty.
func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}
