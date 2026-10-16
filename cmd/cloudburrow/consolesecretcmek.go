package main

import (
	"fmt"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// Customer-managed encryption keys on the Secret Manager pages (#1453): the
// Create secret form takes them, a secret's page shows them and changes or
// removes them (Change encryption keys, as `gcloud secrets replication
// update --set-kms-key` and `--remove-cmek` do), and a version's page shows
// the key version each replica was encrypted with.

const (
	actSecretKeys     = "kmskeys"
	secretKMSKeysHelp = "Optional. Cloud KMS CryptoKeys of this instance, one per line or comma-separated: " +
		"for automatic replication one key in the global location; for user-managed replication one key per replica " +
		"that should have one, each in its replica's location. Empty is Google-managed encryption."
)

// secretKMSKeysField is the keys field of the Create secret form.
func secretKMSKeysField() console.Field {
	return console.Field{Name: "kmsKeys", Label: "Customer-managed encryption keys", Type: "textarea",
		Section: "Encryption", Help: secretKMSKeysHelp}
}

// secretKMSKeysValue is the store's keys for the form's value: one entry
// under automatic replication, one per replica under user-managed, each
// key placed by its own location. nil is none.
func secretKMSKeysValue(raw, kind string, locations []string) ([]string, error) {
	var given []string
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if f = strings.TrimSpace(f); f != "" {
			given = append(given, f)
		}
	}
	if len(given) == 0 {
		return nil, nil
	}
	if kind != "user-managed" {
		if len(given) != 1 {
			return nil, fmt.Errorf("automatic replication takes one key; %d were given", len(given))
		}
		return given, nil
	}
	keys := make([]string, len(locations))
	for _, k := range given {
		p := strings.Split(k, "/")
		if len(p) != 8 || p[2] != "locations" {
			return nil, fmt.Errorf("%q is not a CryptoKey name, projects/P/locations/L/keyRings/R/cryptoKeys/K", k)
		}
		placed := false
		for i, l := range locations {
			if l == p[3] && keys[i] == "" {
				keys[i], placed = k, true
				break
			}
		}
		if !placed {
			return nil, fmt.Errorf("the key %s is in %s, which is not a replica location without a key (%s)",
				k, p[3], strings.Join(locations, ", "))
		}
	}
	return keys, nil
}

// secretKMSKeysText is a secret's keys, one per line.
func secretKMSKeysText(sec secrets.Secret) string {
	var out []string
	for _, k := range sec.KMSKeys {
		if k != "" {
			out = append(out, k)
		}
	}
	return strings.Join(out, "\n")
}

// secretEncryptionProperties shows a secret's keys.
func secretEncryptionProperties(sec secrets.Secret) []console.Property {
	if len(sec.KMSKeys) == 0 {
		return []console.Property{{Label: "Encryption", Value: "Google-managed"}}
	}
	if sec.Replication != "user-managed" {
		return []console.Property{{Label: "Customer-managed key", Value: sec.KMSKeys[0]}}
	}
	var out []console.Property
	for i, l := range sec.ReplicaLocations {
		v := "Google-managed"
		if i < len(sec.KMSKeys) && sec.KMSKeys[i] != "" {
			v = sec.KMSKeys[i]
		}
		out = append(out, console.Property{Label: "Key in " + l, Value: v})
	}
	return out
}

// secretKeysAction is Change encryption keys on a secret's page.
func secretKeysAction(sec secrets.Secret) console.Action {
	return console.Action{ID: actSecretKeys, Label: "Change encryption keys",
		Fields: []console.Field{{Name: "kmsKeys", Label: "Customer-managed encryption keys", Type: "textarea",
			Default: secretKMSKeysText(sec),
			Help: secretKMSKeysHelp + " New versions use these keys; existing versions keep the keys they were " +
				"encrypted with. Clear it to remove customer-managed encryption."}}}
}

// setSecretKeys performs Change encryption keys.
func setSecretKeys(st *secrets.Store, project, id string, values map[string]string) error {
	sec, err := st.GetSecret(project, id)
	if err != nil {
		return err
	}
	keys, err := secretKMSKeysValue(values["kmsKeys"], sec.Replication, sec.ReplicaLocations)
	if err != nil {
		return err
	}
	if keys == nil {
		n := 1
		if sec.Replication == "user-managed" {
			n = len(sec.ReplicaLocations)
		}
		keys = make([]string, n)
	}
	_, err = st.SetReplicationKeys(project, id, keys)
	return err
}

// versionEncryptionProperties shows the key version that encrypted each of a
// version's replicas.
func versionEncryptionProperties(v secrets.Version) []console.Property {
	var out []console.Property
	for _, r := range v.Replicas {
		label := "Key version"
		if r.Location != "" {
			label = "Key version in " + r.Location
		}
		value := "Google-managed"
		if r.KeyVersion != "" {
			value = r.KeyVersion
		}
		out = append(out, console.Property{Label: label, Value: value})
	}
	return out
}
