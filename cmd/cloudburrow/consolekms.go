package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/kms/apiv1/kmspb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
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

// kmsPeriodOrDash is a rotation period as Edit key shows it, or a dash for
// none.
func kmsPeriodOrDash(d *durationpb.Duration) string {
	if d == nil {
		return "—"
	}
	return kmsFormatPeriod(d)
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
	}
	switch path[1] {
	case kmsImportJobsSegment:
		return kmsImportJobDetail(ctx, p.api(), ring, path[2]), nil
	case kmsRetiredSegment:
		return kmsRetiredDetail(ctx, p.api(), ring, path[2]), nil
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
		Sections: []console.Section{
			{ID: "keys", Label: "Keys", Listing: keys},
			kmsImportJobsSection(ctx, api, ring),
			kmsRetiredSection(ctx, api, ring),
		},
	}, nil
}

// keyDetail is a key's page, as Google's (#1851): the key's properties in
// the header, then Overview (its versions, checked for Disable, Enable,
// Destroy and Restore, each row's menu with Make primary version and its
// lifecycle), Permissions and Logs.
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
		Columns:       []string{"State", "Primary", "Algorithm", "Protection level", "Created", "Destroy time"},
		NameColumn:    "Version",
		Noun:          "versions",
		AlwaysStatus:  true,
		RowsOpenable:  true,
		SelectActions: kmsVersionSelectActions(k),
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
				Name:       lastSegment(v.GetName()),
				Status:     kmsState(v),
				Selectable: true,
				Actions:    kmsVersionRowActions(k, v),
				Fields: map[string]string{
					"State":            kmsState(v),
					"Primary":          isPrimary,
					"Algorithm":        kmsAlgorithmWords(v.GetAlgorithm().String()),
					"Protection level": kmsProtectionWords(v.GetProtectionLevel().String()),
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
	rotation := "Never (manual rotation)"
	if rp := k.GetRotationPeriod(); rp != nil {
		rotation = kmsPeriodWords(rp.AsDuration())
	}
	summary := []console.Property{
		{Label: "Purpose", Value: kmsPurposeWords(k.GetPurpose().String())},
		{Label: "Protection level", Value: kmsProtectionWords(t.GetProtectionLevel().String())},
		{Label: "Algorithm", Value: kmsAlgorithmWords(t.GetAlgorithm().String())},
		{Label: "Primary version", Value: primaryLabel},
		{Label: "Rotation period", Value: rotation},
		{Label: "Next rotation", Value: kmsTime(k.GetNextRotationTime())},
		{Label: "Duration of 'scheduled for destruction' state", Value: kmsPeriodWords(k.GetDestroyScheduledDuration().AsDuration())},
		{Label: "Created", Value: kmsTime(k.GetCreateTime())},
		{Label: "Resource name", Value: k.GetName()},
	}
	if b := k.GetCryptoKeyBackend(); b != "" {
		summary = append(summary, console.Property{Label: "Key backend", Value: b})
	}
	if len(k.GetLabels()) > 0 {
		labels := make([]string, 0, len(k.GetLabels()))
		for _, l := range sortedPairs(k.GetLabels()) {
			labels = append(labels, l.Label+": "+l.Value)
		}
		summary = append(summary, console.Property{Label: "Labels", Value: strings.Join(labels, ", ")})
	}
	versions.Note = strings.TrimSpace(versions.Note + " " + kmsProtectionNote +
		" Key material is never shown: no Cloud KMS method returns it; a public key is, through Get public key.")
	return console.Detail{
		Edit:    kmsKeyEditForm(k),
		Summary: summary,
		Sections: []console.Section{
			{ID: "overview", Label: "Overview", Listing: versions},
			{ID: "permissions", Kind: console.KindPermissions},
			{ID: "logs", Label: "Logs", Kind: console.KindLogs},
		},
	}, nil
}

// kmsVersionLifecycle is what a version in its current state offers, as its
// page and its row's menu draw it (#1851): Make primary version for an
// enabled version of a symmetric key that is not primary, Get public key
// beside it, then Disable or Enable, Destroy, Restore, Delete version.
func kmsVersionLifecycle(k *kmspb.CryptoKey, v *kmspb.CryptoKeyVersion) []console.Action {
	destroy := console.Action{ID: "destroy", Label: "Destroy", Destructive: true,
		Confirm: "The version is scheduled for destruction: it can be restored for " +
			kmsPeriodWords(k.GetDestroyScheduledDuration().AsDuration()) + ", then its key material is destroyed."}
	switch v.GetState() {
	case kmspb.CryptoKeyVersion_ENABLED:
		var out []console.Action
		if k.GetPurpose() == kmspb.CryptoKey_ENCRYPT_DECRYPT && k.GetPrimary().GetName() != v.GetName() {
			out = append(out, console.Action{ID: "makeprimary", Label: "Make primary version",
				Confirm: "Encrypt uses this version from now on; the other versions still decrypt."})
		}
		return append(out, console.Action{ID: "disable", Label: "Disable"}, destroy)
	case kmspb.CryptoKeyVersion_DISABLED:
		return []console.Action{{ID: "enable", Label: "Enable"}, destroy}
	case kmspb.CryptoKeyVersion_DESTROY_SCHEDULED:
		return []console.Action{{ID: "restore", Label: "Restore",
			Confirm: "The version is restored DISABLED; enable it to use it again."}}
	case kmspb.CryptoKeyVersion_DESTROYED:
		// Only a DESTROYED version that was not imported can be deleted
		// (service.proto DeleteCryptoKeyVersion).
		if v.GetImportJob() == "" {
			return []console.Action{{ID: "deleteversion", Label: "Delete version", Destructive: true,
				Confirm: "The version's record is deleted permanently; its number is never reused."}}
		}
	}
	return nil
}

// kmsVersionRowActions is a version row's menu (Google's View More): the
// lifecycle, and Get public key on an enabled asymmetric version.
func kmsVersionRowActions(k *kmspb.CryptoKey, v *kmspb.CryptoKeyVersion) []console.Action {
	out := kmsVersionLifecycle(k, v)
	if v.GetState() == kmspb.CryptoKeyVersion_ENABLED {
		for _, a := range kmsVersionCryptoActions(k.GetPurpose()) {
			if a.ID == "publickey" {
				out = append([]console.Action{a}, out...)
			}
		}
	}
	return out
}

// kmsVersionsField is the selection field of the key page's version actions.
const kmsVersionsField = "versions"

// kmsVersionSelectActions are the key page's Disable, Enable, Destroy and
// Restore over the checked versions, as Google's header buttons act on the
// version checkboxes (https://docs.cloud.google.com/kms/docs/enable-disable,
// https://docs.cloud.google.com/kms/docs/destroy-restore). Destroy asks for
// the key's name, then schedules the destruction.
func kmsVersionSelectActions(k *kmspb.CryptoKey) []console.Action {
	sel := []console.Field{{Name: kmsVersionsField, Type: "hidden"}}
	return []console.Action{
		{ID: "disableversions", Label: "Disable", SelectionField: kmsVersionsField, Fields: sel,
			Confirm: "The checked versions stop encrypting and decrypting until they are enabled."},
		{ID: "enableversions", Label: "Enable", SelectionField: kmsVersionsField, Fields: sel,
			Confirm: "The checked versions can encrypt and decrypt again."},
		{ID: "destroyversions", Label: "Destroy", Destructive: true, SelectionField: kmsVersionsField, Fields: sel,
			Confirm: "The checked versions are scheduled for destruction: they can be restored for " +
				kmsPeriodWords(k.GetDestroyScheduledDuration().AsDuration()) + ", then their key material is destroyed."},
		{ID: "restoreversions", Label: "Restore", SelectionField: kmsVersionsField, Fields: sel,
			Confirm: "The checked versions are restored DISABLED; enable them to use them again."},
	}
}

// kmsEditRotationAction is Google's Edit Rotation Period
// (https://docs.cloud.google.com/kms/docs/rotate-key): Rotation period,
// with Never (manual rotation), and Starting on, saved with UpdateCryptoKey.
func kmsEditRotationAction(k *kmspb.CryptoKey) console.Action {
	cur := kmsFormatPeriod(k.GetRotationPeriod())
	opts := kmsRotationField(cur)
	return console.Action{ID: "editrotation", Label: "Edit Rotation Period", Fields: []console.Field{
		{Name: "rotationPeriod", Label: "Rotation period", Type: "select", Default: cur, Options: opts,
			OptionLabels: kmsLabelsFor(opts, kmsPeriodLabel), Help: kmsRotationPeriodHelp},
		{Name: "nextRotationTime", Label: "Starting on", Type: "text", Default: kmsFormatTime(k.GetNextRotationTime()),
			Help: kmsNextRotationHelp + " Never (manual rotation) clears it."},
	}}
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
					{Label: "Trusted wrapping enabled", Value: strconv.FormatBool(v.GetTrustedWrappingEnabled())},
					{Label: "HSM trusted", Value: strconv.FormatBool(v.GetHsmTrusted())},
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
		{Name: "location", Label: "Key ring location", Type: "select", Required: true, Default: "global", Options: kmsLocations(),
			Help: "The locations Locations.ListLocations lists (#1291); CloudBurrow does not place keys geographically. " +
				"An SDK may name any other location."},
	}
}

// CreateOnPage puts Create key ring on a page of its own, as Google's
// console opens it (#1842).
func (kmsProvider) CreateOnPage() bool { return true }

// CreateSubmit is Create key ring's button, Google's Create
// (https://docs.cloud.google.com/kms/docs/create-key-ring).
func (kmsProvider) CreateSubmit() string { return "Create" }

// CreateThen goes on from Create key ring to the new ring's Create key, as
// Google's flow continues to the key (#1851).
func (kmsProvider) CreateThen() string { return "createkey" }

// kmsLocations is what the Create key ring form offers as its Location: what
// the service's own ListLocations answers for a project with no key rings
// (#1291).
func kmsLocations() []string {
	resp, err := kms.NewServer(store.NewMemory()).Locations().ListLocations(
		context.Background(), &locationpb.ListLocationsRequest{Name: "projects/console-regions", PageSize: 1000})
	if err != nil {
		return []string{"global"}
	}
	out := make([]string, 0, len(resp.GetLocations()))
	for _, l := range resp.GetLocations() {
		out = append(out, l.GetLocationId())
	}
	return out
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
const kmsKeyHelp = "Creates a key with version 1, of the protection level, purpose and " +
	"algorithm chosen below; an ENCRYPT_DECRYPT key gets version 1 as its primary. An EXTERNAL key, " +
	"or an EXTERNAL_VPC key on a MANUAL EKM connection, is created without a version: add one naming " +
	"its external key URI or key path."

// kmsProtectionNote says what HSM means here (#1452).
const kmsProtectionNote = "HSM keys behave as SOFTWARE keys do, with the same algorithms: their key material " +
	"is not in a hardware security module, and no version has an attestation."

// kmsExternalNote says what EXTERNAL and EXTERNAL_VPC mean here (#1469).
const kmsExternalNote = "EXTERNAL and EXTERNAL_VPC keys are kept by CloudBurrow acting as their external key manager: " +
	"the key URI or key path a version names is stored, never dialled, and the key material is the instance's."

// kmsProtectionField is Create key's and Create import job's Protection
// level: the levels CreateCryptoKey and CreateImportJob accept (#1452,
// #1469).
func kmsProtectionField(help string, levels ...string) console.Field {
	return console.Field{Name: "protectionLevel", Label: "Protection level", Type: "select", Default: "SOFTWARE",
		Options: append([]string{"SOFTWARE", "HSM"}, levels...), Help: help + " " + kmsProtectionNote}
}

// The Create key form's Key material choices (#1851): Google's Generated
// key and Imported key, and CloudBurrow's External key for an EXTERNAL key,
// or an EXTERNAL_VPC key on a MANUAL connection, which is created without a
// version: each version names its external key.
const (
	kmsMaterialGenerated = "generated"
	kmsMaterialImported  = "imported"
	kmsMaterialExternal  = "external"
)

// kmsCreateKeyFields is the Create key form on a ring's page, in Google's
// order (https://docs.cloud.google.com/kms/docs/create-key): Key name,
// Protection level (Software, HSM, Single-tenant HSM with its instance,
// External, External via VPC), Key material, Purpose, Algorithm, Rotation
// period and Starting on, then Additional settings, whose Duration of
// 'scheduled for destruction' state is 30 days. A function so the pattern
// check in consolepatterns_test.go reads the fields shipped.
func kmsCreateKeyFields() []console.Field {
	return kmsCreateKeyFieldsAt(time.Now())
}

// kmsCreateKeyFieldsAt is kmsCreateKeyFields with Starting on one default
// rotation period after now, as Google's form starts.
func kmsCreateKeyFieldsAt(now time.Time) []console.Field {
	levels := []string{"SOFTWARE", "HSM", "HSM_SINGLE_TENANT", "EXTERNAL", "EXTERNAL_VPC"}
	purpose := kmsPurposeField()
	alg := kmsAlgorithmField()
	materials := []string{kmsMaterialGenerated, kmsMaterialImported, kmsMaterialExternal}
	return []console.Field{
		{Name: "cryptoKeyId", Label: "Key name", Type: "text", Required: true,
			Help: kmsKeyHelp, Pattern: `^[A-Za-z0-9_\-]{1,63}$`},
		{Name: "protectionLevel", Label: "Protection level", Type: "radio", Default: "SOFTWARE", Options: levels,
			OptionLabels: kmsLabelsFor(levels, kmsProtectionWords),
			Help:         "Fixed once the key exists; new versions get it. " + kmsProtectionNote + " " + kmsExternalNote},
		{Name: "cryptoKeyBackend", Label: "Single-tenant HSM instance", Type: "text", ShowWhen: "protectionLevel=HSM_SINGLE_TENANT",
			Help: "projects/P/locations/L/singleTenantHsmInstances/I: an ACTIVE instance in the key ring's location, listed under Single-tenant HSM."},
		{Name: "ekmConnection", Label: "EKM connection", Type: "text", ShowWhen: "protectionLevel=EXTERNAL_VPC",
			Help: "projects/P/locations/L/ekmConnections/C, in the key ring's location; empty is the location's default connection."},
		{Name: "keyMaterial", Label: "Key material", Type: "radio", Default: kmsMaterialGenerated, Options: materials,
			OptionLabels: map[string]string{kmsMaterialGenerated: "Generated key", kmsMaterialImported: "Imported key",
				kmsMaterialExternal: "External key (no first version)"},
			Help: "Generated key makes version 1 now. Imported key makes the key without a version, for key material " +
				"imported through an import job (Import version on the key's page). External key is for an External key, " +
				"or External via VPC on a MANUAL connection, whose versions each name their external key."},
		{Name: "purpose", Label: purpose.Label, Type: "select", Default: purpose.Default, Options: purpose.Options,
			OptionLabels: kmsLabelsFor(purpose.Options, kmsPurposeWords), Help: purpose.Help},
		{Name: "algorithm", Label: alg.Label, Type: "select", Options: alg.Options,
			OptionLabels: kmsLabelsFor(alg.Options, kmsAlgorithmWords), Help: alg.Help},
		kmsTrustedWrappingField("the first version"),
		{Name: "rotationPeriod", Label: "Rotation period", Type: "select", Default: kmsFormatPeriod(durationpb.New(kmsDefaultRotation)),
			Options: kmsRotationPresets, OptionLabels: kmsLabelsFor(kmsRotationPresets, kmsPeriodLabel),
			Help: "Symmetric keys only: Cloud KMS rotates other keys by hand. " + kmsRotationPeriodHelp},
		{Name: "nextRotationTime", Label: "Starting on", Type: "text",
			Default: now.UTC().Add(kmsDefaultRotation).Truncate(time.Second).Format(time.RFC3339),
			Help:    "The first automatic rotation, one rotation period from now unless changed. " + kmsNextRotationHelp},
		{Name: "labels", Label: "Labels", Type: "map", Section: "Additional settings",
			Help: "Optional. Lowercase keys and values."},
		// Read back by GetCryptoKey as CreateCryptoKey set it
		// (TestKMSCreateCryptoKeyFields), and never changeable afterwards,
		// so the create form is the only place it can be chosen (#852).
		{Name: "destroyScheduledDays", Label: "Duration of 'scheduled for destruction' state", Type: "text",
			Section: "Additional settings", Default: "30", Pattern: `^[0-9]{1,3}$`,
			Help: "Days, from 1 to 120: how long a version stays scheduled for destruction, restorable, before its " +
				"material is destroyed. It cannot be changed once the key exists."},
	}
}

// kmsCreateKeyRequest is the CreateCryptoKey request for a Create key
// submission on ring. The ranges are not checked here: CreateCryptoKey
// refuses a destroy scheduled duration or rotation period out of range, and
// its message is the one the form shows.
func kmsCreateKeyRequest(ring string, values map[string]string) (*kmspb.CreateCryptoKeyRequest, error) {
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	purpose := kmspb.CryptoKey_ENCRYPT_DECRYPT
	if p := strings.TrimSpace(values["purpose"]); p != "" {
		v, ok := kmspb.CryptoKey_CryptoKeyPurpose_value[p]
		if !ok {
			return nil, fmt.Errorf("purpose: %q is not a key purpose", p)
		}
		purpose = kmspb.CryptoKey_CryptoKeyPurpose(v)
	}
	// An empty algorithm is sent as unset: CreateCryptoKey implies
	// GOOGLE_SYMMETRIC_ENCRYPTION for ENCRYPT_DECRYPT and refuses the rest,
	// with the message the form shows.
	var alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm
	if a := strings.TrimSpace(values["algorithm"]); a != "" {
		v, ok := kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm_value[a]
		if !ok {
			return nil, fmt.Errorf("algorithm: %q is not a key algorithm", a)
		}
		alg = kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm(v)
	}
	level := kmspb.ProtectionLevel_SOFTWARE
	if pl := strings.TrimSpace(values["protectionLevel"]); pl != "" {
		v, ok := kmspb.ProtectionLevel_value[pl]
		if !ok {
			return nil, fmt.Errorf("protection level: %q is not a protection level", pl)
		}
		level = kmspb.ProtectionLevel(v)
	}
	// Unset, an external ENCRYPT_DECRYPT key gets EXTERNAL_SYMMETRIC_ENCRYPTION
	// from CreateCryptoKey.
	external := level == kmspb.ProtectionLevel_EXTERNAL || level == kmspb.ProtectionLevel_EXTERNAL_VPC
	if alg == 0 && purpose == kmspb.CryptoKey_ENCRYPT_DECRYPT && !external {
		alg = kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION
	}
	backend := strings.TrimSpace(values["cryptoKeyBackend"])
	if level == kmspb.ProtectionLevel_EXTERNAL_VPC && strings.TrimSpace(values["ekmConnection"]) != "" {
		backend = strings.TrimSpace(values["ekmConnection"])
	}
	if level != kmspb.ProtectionLevel_EXTERNAL_VPC && level != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		// The form keeps a hidden choice's value; only these two levels
		// name a backend.
		backend = ""
	}
	// Symmetric keys alone rotate automatically; the form's default
	// rotation is not sent for another purpose.
	if purpose != kmspb.CryptoKey_ENCRYPT_DECRYPT && values["keyMaterial"] != "" {
		values = maps.Clone(values)
		values["rotationPeriod"], values["nextRotationTime"] = "", ""
	}
	if raw := strings.TrimSpace(values["rotationPeriod"]); raw == "" && values["keyMaterial"] != "" {
		values = maps.Clone(values)
		values["nextRotationTime"] = ""
	}
	key := &kmspb.CryptoKey{
		Purpose:          purpose,
		Labels:           labels,
		CryptoKeyBackend: backend,
		VersionTemplate: &kmspb.CryptoKeyVersionTemplate{
			ProtectionLevel: level,
			Algorithm:       alg,
		},
	}
	if raw := strings.TrimSpace(values["destroyScheduledDays"]); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("duration of 'scheduled for destruction' state: %q is not a whole number of days", raw)
		}
		key.DestroyScheduledDuration = durationpb.New(time.Duration(n) * 24 * time.Hour)
	}
	if raw := strings.TrimSpace(values["destroyScheduledDuration"]); raw != "" {
		d, _, err := kmsParsePeriod(raw)
		if err != nil {
			return nil, fmt.Errorf("destroy scheduled duration: %q is not a duration such as 30d or 36h", raw)
		}
		key.DestroyScheduledDuration = durationpb.New(d)
	}
	d, set, err := kmsParsePeriod(values["rotationPeriod"])
	if err != nil {
		return nil, err
	}
	if set {
		key.RotationSchedule = &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(d)}
	}
	if raw := strings.TrimSpace(values["nextRotationTime"]); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, fmt.Errorf("next rotation time: %q is not an RFC 3339 time such as 2026-10-01T00:00:00Z", raw)
		}
		key.NextRotationTime = timestamppb.New(t)
	}
	return &kmspb.CreateCryptoKeyRequest{
		Parent: ring, CryptoKeyId: strings.TrimSpace(values["cryptoKeyId"]), CryptoKey: key,
		SkipInitialVersionCreation: checked(values, "skipInitialVersionCreation") ||
			values["keyMaterial"] == kmsMaterialImported || values["keyMaterial"] == kmsMaterialExternal,
		TrustedWrappingEnabled: checked(values, "trustedWrappingEnabled"),
	}, nil
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
		out := []console.Action{{ID: "createkey", Label: "Create key", Fields: kmsCreateKeyFields()},
			kmsCreateImportJobAction(), kmsRandomBytesAction}
		// DeleteKeyRing needs every key deleted first, and every import
		// job expired (delete-kms-resources, #1595): offered on an empty
		// ring, where the API names an import job still in the way.
		if ks, err := api.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring, PageSize: 1}); err == nil && len(ks.GetCryptoKeys()) == 0 {
			out = append(out, console.Action{ID: "deletering", Label: "Delete", Destructive: true,
				Confirm: "The key ring is deleted permanently; its name can be used again after about 5 to 10 minutes."})
		}
		return out
	case 2:
		key := ring + "/cryptoKeys/" + path[1]
		k, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
		if err != nil {
			return nil
		}
		addFields := kmsExternalFields(k.GetVersionTemplate().GetProtectionLevel(), nil)
		trustable := kmsTrustable(k)
		if trustable {
			addFields = append(addFields, kmsTrustedWrappingField("the new version"))
		}
		// Rotate, as Google's header has it, makes a version and, on a
		// symmetric key, its primary; Add version only makes one.
		out := []console.Action{{ID: "rotate", Label: "Rotate", Fields: addFields,
			Confirm: "A new version is created" + map[bool]string{true: " and made the primary version.", false: "."}[k.GetPurpose() == kmspb.CryptoKey_ENCRYPT_DECRYPT]}}
		if k.GetPurpose() == kmspb.CryptoKey_ENCRYPT_DECRYPT {
			out = append(out, kmsEditRotationAction(k))
		}
		out = append(out, console.Action{ID: "addversion", Label: "Add version", Fields: addFields})
		out = append(out, kmsVersionSelectActions(k)...)
		if a, ok := kmsImportVersionAction(ctx, api, ring, k); ok {
			out = append(out, a)
		}
		if trustable {
			out = append(out, kmsImportTrustedAction(k))
		}
		// DeleteCryptoKey needs every version deleted first (service.proto).
		if vs, err := api.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key, PageSize: 1}); err == nil && len(vs.GetCryptoKeyVersions()) == 0 {
			out = append(out, console.Action{ID: "deletekey", Label: "Delete key", Destructive: true,
				Confirm: "The key is deleted permanently, and its name can never be used again."})
		}
		if k.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT {
			// The other purposes' operations name a version: they are on
			// each version's page.
			return out
		}
		return append(out,
			console.Action{ID: "encrypt", Label: "Encrypt", Fields: []console.Field{
				{Name: "plaintext", Label: "Plaintext", Type: "textarea", Required: true,
					Help: "Encrypted as UTF-8 with the primary version. The ciphertext is shown " +
						"base64-encoded in the dialog and recorded nowhere."},
				{Name: "aad", Label: "Additional authenticated data", Type: "text",
					Help: "Optional. The same value must be given to decrypt."},
			}},
			console.Action{ID: "decrypt", Label: "Decrypt", Fields: []console.Field{
				{Name: "ciphertext", Label: "Ciphertext (base64)", Type: "textarea", Required: true,
					Help: "As Encrypt returned it. The plaintext is shown in the dialog and recorded nowhere."},
				{Name: "aad", Label: "Additional authenticated data", Type: "text",
					Help: "Optional. Must match what was given to encrypt."},
			}})
	case 3:
		if path[1] == kmsImportJobsSegment || path[1] == kmsRetiredSegment {
			return nil
		}
		name := ring + "/cryptoKeys/" + path[1] + "/cryptoKeyVersions/" + path[2]
		v, err := api.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: name})
		if err != nil {
			return nil
		}
		k, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: kmsKeyOf(name)})
		if err != nil {
			return nil
		}
		var out []console.Action
		if v.GetState() == kmspb.CryptoKeyVersion_ENABLED {
			out = kmsVersionCryptoActions(k.GetPurpose())
			if v.GetTrustedWrappingEnabled() {
				out = append(out, kmsExportTrustedAction)
			}
		}
		out = append(out, kmsVersionLifecycle(k, v)...)
		if st := v.GetState(); st == kmspb.CryptoKeyVersion_ENABLED || st == kmspb.CryptoKeyVersion_DISABLED {
			out = append(out, kmsEditReference(v)...)
		}
		return out
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

	if len(path) == 3 && (path[1] == kmsImportJobsSegment || path[1] == kmsRetiredSegment) {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	if len(path) == 3 {
		if out, ok, err := kmsVersionCryptoAct(ctx, api, version, action, values); ok {
			return out, err
		}
	}
	switch {
	case action == "randombytes" && len(path) == 1:
		return kmsRandomBytes(ctx, api, ring, values)

	case action == "deletekey" && len(path) == 2:
		op, err := api.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: key})
		if err == nil && op.GetError() != nil {
			err = fmt.Errorf("%s", op.GetError().GetMessage())
		}
		return nil, err

	case action == "deletering" && len(path) == 1:
		op, err := api.DeleteKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring})
		if err == nil && op.GetError() != nil {
			err = fmt.Errorf("%s", op.GetError().GetMessage())
		}
		return nil, err

	case action == "createimportjob" && len(path) == 1:
		return nil, kmsCreateImportJob(ctx, api, ring, values)

	case action == "importversion" && len(path) == 2:
		material, err := kmsB64("the key material", values["material"])
		if err != nil {
			return nil, err
		}
		return nil, kmsImportVersion(ctx, api, key, values, material)

	case action == "createkey" && len(path) == 1:
		req, err := kmsCreateKeyRequest(ring, values)
		if err != nil {
			return nil, err
		}
		_, err = api.CreateCryptoKey(ctx, req)
		return nil, err

	case action == "rotate" && len(path) == 2:
		v, err := api.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{
			Parent: key, CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: kmsExternalOptions(values),
				TrustedWrappingEnabled: checked(values, "trustedWrappingEnabled")},
		})
		if err != nil {
			return nil, err
		}
		k, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
		if err != nil || k.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT || v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
			return nil, err
		}
		_, err = api.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{
			Name: key, CryptoKeyVersionId: lastSegment(v.GetName())})
		return nil, err

	case action == "editrotation" && len(path) == 2:
		k, err := api.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
		if err != nil {
			return nil, err
		}
		vals := map[string]string{"rotationPeriod": values["rotationPeriod"], "nextRotationTime": values["nextRotationTime"]}
		if strings.TrimSpace(vals["rotationPeriod"]) == "" {
			vals["nextRotationTime"] = ""
		}
		req, err := kmsKeyUpdate(k, vals)
		if err != nil || req == nil {
			return nil, err
		}
		_, err = api.UpdateCryptoKey(ctx, req)
		return nil, err

	case strings.HasSuffix(action, "versions") && len(path) == 2:
		return nil, kmsActOnVersions(ctx, api, key, strings.TrimSuffix(action, "versions"), values[kmsVersionsField])

	case action == "addversion" && len(path) == 2:
		_, err := api.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{
			Parent: key, CryptoKeyVersion: &kmspb.CryptoKeyVersion{ExternalProtectionLevelOptions: kmsExternalOptions(values),
				TrustedWrappingEnabled: checked(values, "trustedWrappingEnabled")},
		})
		return nil, err

	case action == "importtrusted" && len(path) == 2:
		return nil, kmsImportTrusted(ctx, api, key, values)

	case action == "exporttrusted" && len(path) == 3:
		return kmsExportTrusted(ctx, api, version, values)

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

	case action == "editreference" && len(path) == 3:
		opts := kmsExternalOptions(values)
		if opts == nil {
			opts = &kmspb.ExternalProtectionLevelOptions{}
		}
		_, err := api.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, ExternalProtectionLevelOptions: opts},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"external_protection_level_options"}},
		})
		return nil, err

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

// kmsExternalFields are the external key reference a version of an EXTERNAL
// or EXTERNAL_VPC key names (#1469): Add version asks for it, and Edit key
// reference on a version's page starts from the version's own. Other levels
// have none.
func kmsExternalFields(level kmspb.ProtectionLevel, cur *kmspb.ExternalProtectionLevelOptions) []console.Field {
	switch level {
	case kmspb.ProtectionLevel_EXTERNAL:
		return []console.Field{{Name: "externalKeyUri", Label: "External key URI", Type: "text", Required: true, Default: cur.GetExternalKeyUri(),
			Help: "The https URI of the key on the external key manager. " + kmsExternalNote}}
	case kmspb.ProtectionLevel_EXTERNAL_VPC:
		return []console.Field{
			{Name: "ekmConnectionKeyPath", Label: "Key path", Type: "text", Default: cur.GetEkmConnectionKeyPath(),
				Help: "The key's path on the EKM, such as v0/my/key. Leave empty on a CLOUD_KMS (coordinated) connection, whose EKM chooses it. " + kmsExternalNote},
			{Name: "ekmConnectionBackendOverride", Label: "EKM connection override", Type: "text", Default: cur.GetEkmConnectionBackendOverride(),
				Help: "Optional: another EKM connection in the key's location, in place of the key's."},
		}
	}
	return nil
}

// kmsExternalOptions reads kmsExternalFields' values; nil when none is set.
func kmsExternalOptions(values map[string]string) *kmspb.ExternalProtectionLevelOptions {
	o := &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: strings.TrimSpace(values["externalKeyUri"]),
		EkmConnectionKeyPath: strings.TrimSpace(values["ekmConnectionKeyPath"]), EkmConnectionBackendOverride: strings.TrimSpace(values["ekmConnectionBackendOverride"])}
	if o.ExternalKeyUri == "" && o.EkmConnectionKeyPath == "" && o.EkmConnectionBackendOverride == "" {
		return nil
	}
	return o
}

// kmsEditReference is Edit key reference on an external version's page:
// UpdateCryptoKeyVersion's external_protection_level_options, which moves
// the version to another key URI or path without rotating the key (#1469).
func kmsEditReference(v *kmspb.CryptoKeyVersion) []console.Action {
	f := kmsExternalFields(v.GetProtectionLevel(), v.GetExternalProtectionLevelOptions())
	if f == nil {
		return nil
	}
	return []console.Action{{ID: "editreference", Label: "Edit key reference", Fields: f}}
}

// kmsActOnVersions performs one lifecycle operation on each checked version
// of a key (#1851): disable, enable, destroy or restore. Each is its own
// call, as Google's console makes one per version; every version is tried,
// and the refusals are reported together, each naming its version.
func kmsActOnVersions(ctx context.Context, api *kms.Server, key, op, list string) error {
	var ids []string
	for _, id := range strings.Fields(list) {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return fmt.Errorf("check one or more versions first")
	}
	var failed []string
	for _, id := range ids {
		if _, err := strconv.Atoi(id); err != nil {
			return fmt.Errorf("%q is not a version number", id)
		}
		name := key + "/cryptoKeyVersions/" + id
		var err error
		switch op {
		case "disable", "enable":
			state := kmspb.CryptoKeyVersion_ENABLED
			if op == "disable" {
				state = kmspb.CryptoKeyVersion_DISABLED
			}
			_, err = api.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
				CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: state},
				UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}},
			})
		case "destroy":
			_, err = api.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: name})
		case "restore":
			_, err = api.RestoreCryptoKeyVersion(ctx, &kmspb.RestoreCryptoKeyVersionRequest{Name: name})
		default:
			return fmt.Errorf("unknown action %q", op+"versions")
		}
		if err != nil {
			failed = append(failed, "version "+id+": "+consoleErr(err))
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}
