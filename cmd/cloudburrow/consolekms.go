package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
)

// kmsProvider is the Cloud KMS screen (#593).
//
// Every read and every change is a call on the same kms.Server the gRPC and
// JSON transports serve, so a ring created here is one an SDK lists, and a
// version disabled here is one an SDK sees DISABLED. The server never returns
// key material through any method, and this provider calls nothing else, so
// the console has no way to show it.
//
// Paths are [ring resource name, key ID, version number]: the ring's full name
// because a ring ID is unique only within its location.
type kmsProvider struct{ svc *kmsService }

func (kmsProvider) ID() string    { return "kms" }
func (kmsProvider) Title() string { return "Cloud KMS" }

const kmsNotStarted = "Cloud KMS has not started"

func (p kmsProvider) api() *kms.Server { return p.svc.API() }

func (p kmsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	api := p.api()
	if api == nil {
		return console.Listing{}, errors.New(kmsNotStarted)
	}
	listing := console.Listing{
		Columns:    []string{"Location", "Keys", "Created"},
		NameColumn: "Key ring",
		Noun:       "key rings",
	}
	if project == "" {
		listing.Prompt = "Cloud KMS lists key rings per project. Choose one in the toolbar."
		return listing, nil
	}
	rings, err := api.KeyRingsInProject(project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, r := range rings {
		keys := "—"
		if resp, err := api.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: r.GetName(), PageSize: 1}); err == nil {
			keys = fmt.Sprint(resp.GetTotalSize())
		}
		listing.Items = append(listing.Items, console.Resource{
			Name: r.GetName(),
			Fields: map[string]string{
				"Location": kmsLocation(r.GetName()),
				"Keys":     keys,
				"Created":  kmsTime(r.GetCreateTime()),
			},
		})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	// Rings and keys cannot be deleted in Cloud KMS, so there is no delete
	// here either; saying so stops the absent button reading as a gap.
	listing.Note = "Key rings and keys cannot be deleted in Cloud KMS; destroy a key's versions instead."
	return listing, nil
}

// kmsLocation returns the location segment of a KMS resource name.
func kmsLocation(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) >= 4 && parts[2] == "locations" {
		return parts[3]
	}
	return "—"
}

func kmsTime(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() || ts.AsTime().IsZero() {
		return "—"
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// kmsState is a version state as the API spells it, without the enum prefix.
func kmsState(v *kmspb.CryptoKeyVersion) string {
	if v == nil {
		return "—"
	}
	return v.GetState().String()
}

// kmsRing checks that a path's ring belongs to the project the screen is scoped
// to. Without it a hand-written URL would open another project's ring from
// this one's page.
func kmsRing(project string, path []string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("choose a project first")
	}
	ring := path[0]
	if !strings.HasPrefix(ring, "projects/"+project+"/locations/") {
		return "", fmt.Errorf("%s is not a key ring of project %s", ring, project)
	}
	return ring, nil
}

func (p kmsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 3 {
		return console.DeeperThan(3, path), nil
	}
	if p.api() == nil {
		return console.Detail{Unavailable: kmsNotStarted}, nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	ring, err := kmsRing(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	switch len(path) {
	case 1:
		return p.ringDetail(ctx, ring)
	case 2:
		return p.keyDetail(ctx, ring+"/cryptoKeys/"+path[1])
	default:
		return p.versionDetail(ctx, ring+"/cryptoKeys/"+path[1]+"/cryptoKeyVersions/"+path[2])
	}
}

func (p kmsProvider) ringDetail(ctx context.Context, ring string) (console.Detail, error) {
	api := p.api()
	r, err := api.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the key ring: " + consoleErr(err)}, nil
	}
	keys := console.Listing{
		Columns:      []string{"Purpose", "Primary version", "Protection level", "Algorithm", "Created"},
		NameColumn:   "Key",
		Noun:         "keys",
		AlwaysStatus: true,
		RowsOpenable: true,
	}
	resp, err := api.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring, PageSize: detailLimit})
	if err != nil {
		keys.Unavailable = "cannot list keys: " + consoleErr(err)
	} else {
		for _, k := range resp.GetCryptoKeys() {
			primary, state := "—", "NO PRIMARY"
			if pv := k.GetPrimary(); pv != nil {
				primary = lastSegment(pv.GetName())
				state = kmsState(pv)
			}
			t := k.GetVersionTemplate()
			keys.Items = append(keys.Items, console.Resource{
				Name:   lastSegment(k.GetName()),
				Status: state,
				Fields: map[string]string{
					"Purpose":          k.GetPurpose().String(),
					"Primary version":  primary,
					"Protection level": t.GetProtectionLevel().String(),
					"Algorithm":        t.GetAlgorithm().String(),
					"Created":          kmsTime(k.GetCreateTime()),
				},
			})
		}
		keys.Total = len(keys.Items)
		if int(resp.GetTotalSize()) > len(keys.Items) {
			keys.Note = truncatedNote(len(keys.Items), "keys")
		}
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Location", Value: kmsLocation(r.GetName())},
			{Label: "Keys", Value: fmt.Sprint(len(keys.Items))},
			{Label: "Created", Value: kmsTime(r.GetCreateTime())},
		},
		Sections: []console.Section{{ID: "keys", Label: "Keys", Listing: keys}},
	}, nil
}

func (p kmsProvider) keyDetail(ctx context.Context, key string) (console.Detail, error) {
	api := p.api()
	k, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the key: " + consoleErr(err)}, nil
	}
	primary := ""
	if pv := k.GetPrimary(); pv != nil {
		primary = pv.GetName()
	}
	versions := console.Listing{
		Columns:      []string{"State", "Primary", "Algorithm", "Protection level", "Created", "Destroy time"},
		NameColumn:   "Version",
		Noun:         "versions",
		AlwaysStatus: true,
		RowsOpenable: true,
	}
	resp, err := api.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{
		Parent: key, PageSize: detailLimit, OrderBy: "name desc",
	})
	if err != nil {
		versions.Unavailable = "cannot list versions: " + consoleErr(err)
	} else {
		for _, v := range resp.GetCryptoKeyVersions() {
			when := kmsTime(v.GetDestroyTime())
			if when == "—" {
				when = kmsTime(v.GetDestroyEventTime())
			}
			isPrimary := "—"
			if v.GetName() == primary {
				isPrimary = "Primary"
			}
			versions.Items = append(versions.Items, console.Resource{
				Name:   lastSegment(v.GetName()),
				Status: kmsState(v),
				Fields: map[string]string{
					"State":            kmsState(v),
					"Primary":          isPrimary,
					"Algorithm":        v.GetAlgorithm().String(),
					"Protection level": v.GetProtectionLevel().String(),
					"Created":          kmsTime(v.GetCreateTime()),
					"Destroy time":     when,
				},
			})
		}
		versions.Total = len(versions.Items)
		if int(resp.GetTotalSize()) > len(versions.Items) {
			versions.Note = truncatedNote(len(versions.Items), "versions")
		}
	}

	primaryLabel := "none"
	if primary != "" {
		primaryLabel = lastSegment(primary) + " (" + kmsState(k.GetPrimary()) + ")"
	}
	t := k.GetVersionTemplate()
	config := console.Section{
		ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{
			Heading: "Key",
			Properties: []console.Property{
				{Label: "Resource name", Value: k.GetName()},
				{Label: "Purpose", Value: k.GetPurpose().String()},
				{Label: "Protection level", Value: t.GetProtectionLevel().String()},
				{Label: "Algorithm", Value: t.GetAlgorithm().String()},
				{Label: "Destroy scheduled duration", Value: k.GetDestroyScheduledDuration().AsDuration().String()},
			},
		}},
		Note: "Only symmetric ENCRYPT_DECRYPT keys at SOFTWARE protection exist locally. " +
			"Key material is never shown: no Cloud KMS method returns it.",
	}
	if len(k.GetLabels()) > 0 {
		config.Groups = append(config.Groups, console.PropertyGroup{
			Heading: "Labels", Properties: sortedPairs(k.GetLabels()),
		})
	}
	return console.Detail{
		Edit: kmsKeyEditForm(k),
		Summary: []console.Property{
			{Label: "Purpose", Value: k.GetPurpose().String()},
			{Label: "Primary version", Value: primaryLabel},
			{Label: "Versions", Value: fmt.Sprint(len(versions.Items))},
			{Label: "Created", Value: kmsTime(k.GetCreateTime())},
		},
		Sections: []console.Section{
			{ID: "versions", Label: "Versions", Listing: versions},
			config,
		},
	}, nil
}

func (p kmsProvider) versionDetail(ctx context.Context, name string) (console.Detail, error) {
	api := p.api()
	v, err := api.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the version: " + consoleErr(err)}, nil
	}
	isPrimary := "No"
	if k, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: kmsKeyOf(name)}); err == nil &&
		k.GetPrimary().GetName() == name {
		isPrimary = "Yes"
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: kmsState(v)},
			{Label: "Primary", Value: isPrimary},
			{Label: "Created", Value: kmsTime(v.GetCreateTime())},
		},
		Sections: []console.Section{{
			ID: "state", Label: "State", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{
				Heading: "Version " + lastSegment(name),
				Properties: []console.Property{
					{Label: "Resource name", Value: v.GetName()},
					{Label: "State", Value: kmsState(v)},
					{Label: "Algorithm", Value: v.GetAlgorithm().String()},
					{Label: "Protection level", Value: v.GetProtectionLevel().String()},
					{Label: "Generated", Value: kmsTime(v.GetGenerateTime())},
					{Label: "Destroy time", Value: kmsTime(v.GetDestroyTime())},
					{Label: "Destroyed", Value: kmsTime(v.GetDestroyEventTime())},
				},
			}},
			Note: kmsVersionNote(v.GetState()),
		}},
	}, nil
}

// kmsKeyOf returns the key a version name belongs to.
func kmsKeyOf(version string) string {
	if i := strings.Index(version, "/cryptoKeyVersions/"); i >= 0 {
		return version[:i]
	}
	return version
}

func kmsVersionNote(st kmspb.CryptoKeyVersion_CryptoKeyVersionState) string {
	switch st {
	case kmspb.CryptoKeyVersion_DISABLED:
		return "A disabled version cannot encrypt or decrypt. Enable it to use it again."
	case kmspb.CryptoKeyVersion_DESTROY_SCHEDULED:
		return "Scheduled for destruction at the destroy time. Until then it can be " +
			"restored, which leaves it DISABLED."
	case kmspb.CryptoKeyVersion_DESTROYED:
		return "The key material is gone. Ciphertext made with this version can no " +
			"longer be decrypted, and destroying is terminal."
	}
	return ""
}

// Create makes a key ring. A ring holds keys and nothing else, so its form is
// a name and a location; keys are created from the ring's own page.
func (kmsProvider) CreateForm() (string, []console.Field) {
	return "Create key ring", []console.Field{
		{Name: "keyRingId", Label: "Key ring name", Type: "text", Required: true,
			Help:    "1-63 letters, digits, hyphens and underscores. Cannot be deleted or renamed.",
			Pattern: `^[A-Za-z0-9_\-]{1,63}$`},
		{Name: "location", Label: "Location", Type: "text", Required: true, Default: "global",
			Help: "Any location name; CloudBurrow does not place keys geographically."},
	}
}

func (p kmsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.api()
	if api == nil {
		return "", errors.New(kmsNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a key ring")
	}
	location := strings.TrimSpace(values["location"])
	if location == "" {
		location = "global"
	}
	r, err := api.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
		Parent:    "projects/" + project + "/locations/" + location,
		KeyRingId: strings.TrimSpace(values["keyRingId"]),
	})
	if err != nil {
		return "", err
	}
	return r.GetName(), nil
}

// kmsKeyHelp names what a key created here is, and what is refused, so the
// form says it before the API has to.
const kmsKeyHelp = "Creates a symmetric ENCRYPT_DECRYPT key at SOFTWARE protection " +
	"(GOOGLE_SYMMETRIC_ENCRYPTION) with version 1 as its primary. Asymmetric and MAC " +
	"keys, HSM and EXTERNAL protection, import and automatic rotation are not " +
	"implemented locally and are not offered."

// kmsCreateKeyFields is the Create key form on a ring's page. A function so
// the pattern check in consolepatterns_test.go reads the fields shipped.
func kmsCreateKeyFields() []console.Field {
	return []console.Field{
		{Name: "cryptoKeyId", Label: "Key name", Type: "text", Required: true,
			Help: kmsKeyHelp, Pattern: `^[A-Za-z0-9_\-]{1,63}$`},
		{Name: "labels", Label: "Labels", Type: "map",
			Help: "Optional. Lowercase keys and values."},
	}
}

// DetailActions offers what the API can do at each level: a key on a ring;
// a version, encryption and decryption on a key; and a version's lifecycle,
// drawn from its current state so no action exists only to fail.
func (p kmsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	api := p.api()
	if api == nil {
		return nil
	}
	ring, err := kmsRing(project, path)
	if err != nil {
		return nil
	}
	switch len(path) {
	case 1:
		return []console.Action{{ID: "createkey", Label: "Create key", Fields: kmsCreateKeyFields()}}
	case 2:
		return []console.Action{
			{ID: "addversion", Label: "Add version"},
			{ID: "encrypt", Label: "Encrypt", Fields: []console.Field{
				{Name: "plaintext", Label: "Plaintext", Type: "textarea", Required: true,
					Help: "Encrypted as UTF-8 with the primary version. The ciphertext is shown " +
						"base64-encoded in the dialog and recorded nowhere."},
				{Name: "aad", Label: "Additional authenticated data", Type: "text",
					Help: "Optional. The same value must be given to decrypt."},
			}},
			{ID: "decrypt", Label: "Decrypt", Fields: []console.Field{
				{Name: "ciphertext", Label: "Ciphertext (base64)", Type: "textarea", Required: true,
					Help: "As Encrypt returned it. The plaintext is shown in the dialog and recorded nowhere."},
				{Name: "aad", Label: "Additional authenticated data", Type: "text",
					Help: "Optional. Must match what was given to encrypt."},
			}},
		}
	case 3:
		name := ring + "/cryptoKeys/" + path[1] + "/cryptoKeyVersions/" + path[2]
		v, err := api.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: name})
		if err != nil {
			return nil
		}
		switch v.GetState() {
		case kmspb.CryptoKeyVersion_ENABLED:
			out := []console.Action{}
			if k, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: kmsKeyOf(name)}); err == nil &&
				k.GetPrimary().GetName() != name {
				out = append(out, console.Action{ID: "makeprimary", Label: "Make primary"})
			}
			return append(out,
				console.Action{ID: "disable", Label: "Disable"},
				console.Action{ID: "destroy", Label: "Schedule destruction", Destructive: true})
		case kmspb.CryptoKeyVersion_DISABLED:
			return []console.Action{
				{ID: "enable", Label: "Enable"},
				{ID: "destroy", Label: "Schedule destruction", Destructive: true},
			}
		case kmspb.CryptoKeyVersion_DESTROY_SCHEDULED:
			return []console.Action{{ID: "restore", Label: "Restore"}}
		}
	}
	return nil
}

func (p kmsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

// ActAtResult performs one action. Encrypt and decrypt answer with a row; the
// server returns it to the dialog only, never to the ledger.
func (p kmsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	api := p.api()
	if api == nil {
		return nil, errors.New(kmsNotStarted)
	}
	ring, err := kmsRing(project, path)
	if err != nil {
		return nil, err
	}
	key := ""
	if len(path) >= 2 {
		key = ring + "/cryptoKeys/" + path[1]
	}
	version := ""
	if len(path) == 3 {
		version = key + "/cryptoKeyVersions/" + path[2]
	}

	switch {
	case action == "createkey" && len(path) == 1:
		labels, err := console.ParseMap(values["labels"])
		if err != nil {
			return nil, fmt.Errorf("labels: %w", err)
		}
		_, err = api.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{
			Parent:      ring,
			CryptoKeyId: strings.TrimSpace(values["cryptoKeyId"]),
			CryptoKey: &kmspb.CryptoKey{
				Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT,
				Labels:  labels,
				VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
					ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
					Algorithm:       kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION,
				},
			},
		})
		return nil, err

	case action == "addversion" && len(path) == 2:
		_, err := api.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{
			Parent: key, CryptoKeyVersion: &kmspb.CryptoKeyVersion{},
		})
		return nil, err

	case action == "encrypt" && len(path) == 2:
		resp, err := api.Encrypt(ctx, &kmspb.EncryptRequest{
			Name:                        key,
			Plaintext:                   []byte(values["plaintext"]),
			AdditionalAuthenticatedData: []byte(values["aad"]),
		})
		if err != nil {
			return nil, err
		}
		return kmsResult("Ciphertext", base64.StdEncoding.EncodeToString(resp.GetCiphertext()),
			lastSegment(resp.GetName())), nil

	case action == "decrypt" && len(path) == 2:
		ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(values["ciphertext"]))
		if err != nil {
			return nil, fmt.Errorf("the ciphertext is not base64: %v", err)
		}
		resp, err := api.Decrypt(ctx, &kmspb.DecryptRequest{
			Name:                        key,
			Ciphertext:                  ct,
			AdditionalAuthenticatedData: []byte(values["aad"]),
		})
		if err != nil {
			return nil, err
		}
		pt := resp.GetPlaintext()
		shown, label := string(pt), "Plaintext"
		if !utf8.Valid(pt) {
			shown, label = base64.StdEncoding.EncodeToString(pt), "Plaintext (base64)"
		}
		return kmsResult(label, shown, ""), nil

	case (action == "enable" || action == "disable") && len(path) == 3:
		state := kmspb.CryptoKeyVersion_ENABLED
		if action == "disable" {
			state = kmspb.CryptoKeyVersion_DISABLED
		}
		_, err := api.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: state},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}},
		})
		return nil, err

	case action == "destroy" && len(path) == 3:
		_, err := api.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: version})
		return nil, err

	case action == "restore" && len(path) == 3:
		_, err := api.RestoreCryptoKeyVersion(ctx, &kmspb.RestoreCryptoKeyVersionRequest{Name: version})
		return nil, err

	case action == "makeprimary" && len(path) == 3:
		_, err := api.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{
			Name: key, CryptoKeyVersionId: path[2],
		})
		return nil, err
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

// kmsResult is the one-row answer an encrypt or decrypt shows in its dialog.
func kmsResult(label, value, version string) *console.Listing {
	fields := map[string]string{label: value}
	columns := []string{label}
	if version != "" {
		fields["Version"] = version
		columns = append(columns, "Version")
	}
	return &console.Listing{
		Columns: columns, NameColumn: "Result", Noun: "results",
		Items: []console.Resource{{Name: "result", Fields: fields}},
		Total: 1,
	}
}

// consoleErr is an in-process API error as a page shows it: the service's own
// message, without the gRPC envelope around it.
func consoleErr(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}
