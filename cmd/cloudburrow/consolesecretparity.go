package main

// Secret Manager console parity (#1850): Google's menu (Secret Manager,
// Regional secrets, Settings), the secrets list's columns and row Actions
// (Add new version, Edit), Create secret laid out as Google's page under its
// section headings (Secret value with Upload file; Replication policy;
// Encryption; Rotation with Set rotation period, a Rotation period list and
// Starting on; Notifications; Expiration; Labels; Annotations; Version
// aliases; Delayed destruction), the secret's Overview, Versions,
// Permissions and Logs tabs, and version aliases, which the backend serves
// since this change (internal/service/secrets/aliases.go).
//
// Sources: https://docs.cloud.google.com/secret-manager/docs/creating-and-accessing-secrets
// (Name, Secret type, Secret value or Upload file, Create secret),
// https://docs.cloud.google.com/secret-manager/docs/secret-rotation (Rotation,
// Set rotation period, Rotation period with Custom, Starting on, Update
// secret, the Actions menu's Edit),
// https://docs.cloud.google.com/secret-manager/docs/assign-alias-to-secret-version
// (Version aliases, Add alias, Update secret),
// https://docs.cloud.google.com/secret-manager/docs/add-secret-version (Add new
// version), https://docs.cloud.google.com/secret-manager/docs/view-secret-details
// (Overview) and
// https://docs.cloud.google.com/secret-manager/regional-secrets/view-regional-secret-details
// (Regional secrets), read 2026-11-19.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
)

// secretsRegionalProvider is the Regional secrets page: the project's
// regional secrets (#1519), and Create regional secret. Every other
// operation is the Secret Manager page's, which addresses a regional secret
// by its full name.
type secretsRegionalProvider struct{ secretsProvider }

func (secretsRegionalProvider) ID() string    { return "secrets-regional" }
func (secretsRegionalProvider) Title() string { return "Regional secrets" }

func (p secretsRegionalProvider) List(ctx context.Context, project string) (console.Listing, error) {
	return p.ListFiltered(ctx, project, "")
}

func (p secretsRegionalProvider) ListFiltered(ctx context.Context, project, filter string) (console.Listing, error) {
	return p.secretsListing(ctx, project, filter, true)
}

func (p secretsRegionalProvider) CreateForm() (string, []console.Field) {
	return "Create regional secret", secretCreateFields(true)
}

func (secretsRegionalProvider) CreateTitle() string { return "Create regional secret" }

// Create makes a regional secret: the location is required here.
func (p secretsRegionalProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if strings.TrimSpace(values["location"]) == "" {
		return "", fmt.Errorf("a regional secret needs a location, such as us-central1")
	}
	return p.secretsProvider.Create(ctx, project, values)
}

// The secrets list's columns, Google's order; the row menu is Actions.
var secretListColumns = []string{"Location", "Encryption", "Labels", "Created", "Expiration"}

// secretsListing lists the project's global secrets, or its regional ones.
func (p secretsProvider) secretsListing(_ context.Context, project, filter string, regional bool) (console.Listing, error) {
	st := p.svc.Store()
	if st == nil {
		return console.Listing{}, fmt.Errorf("Secret Manager has not started")
	}
	if project == "" {
		return console.Listing{Columns: secretListColumns, Noun: "secrets",
			Prompt: "Secret Manager lists secrets per project. Choose one in the toolbar."}, nil
	}
	sf := &console.ServerFilter{Value: filter, Placeholder: "labels.env=prod AND name:app", Help: secretsFilterHelp}
	var all []secrets.Secret
	var err error
	if regional {
		all, err = st.ListRegionalSecretsFiltered(project, filter)
	} else {
		all, err = st.ListSecretsFiltered(project, filter)
	}
	if err != nil {
		if filter == "" {
			return console.Listing{}, err
		}
		sf.Error = err.Error()
		return console.Listing{Columns: secretListColumns, Noun: "secrets", ServerFilter: sf}, nil
	}
	items := make([]console.Resource, 0, len(all))
	for _, s := range all {
		items = append(items, console.Resource{
			Name: s.Name,
			Fields: map[string]string{
				"Location":   secretLocationText(s),
				"Encryption": secretEncryptionText(s),
				"Labels":     secretPairsText(s.Labels),
				"Created":    s.Created.Format(time.RFC3339),
				"Expiration": secretExpiry(s),
			},
			Actions: []console.Action{secretAddVersionAction()},
		})
	}
	return console.Listing{Columns: secretListColumns, Noun: "secrets", Items: items, Total: len(items), ServerFilter: sf}, nil
}

// secretLocationText is the list's Location: a regional secret's region, a
// user-managed secret's replica locations, or automatic replication.
func secretLocationText(s secrets.Secret) string {
	switch {
	case s.Regional() && len(s.ReplicaLocations) == 1:
		return s.ReplicaLocations[0]
	case s.Replication == "user-managed":
		return strings.Join(s.ReplicaLocations, ", ")
	}
	return "Automatically replicated"
}

// secretEncryptionText is the list's Encryption.
func secretEncryptionText(s secrets.Secret) string {
	for _, k := range s.KMSKeys {
		if k != "" {
			return "Customer-managed"
		}
	}
	return "Google-managed"
}

// secretPairsText is labels as the list shows them: "key: value, …".
func secretPairsText(m map[string]string) string {
	if len(m) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+m[k])
	}
	return strings.Join(parts, ", ")
}

// RowEditLabel puts Edit in each secret row's Actions menu, as Google's
// list has it; it opens the secret's Edit secret form.
func (secretsProvider) RowEditLabel() string { return "Edit" }

// CreateLayout draws Create secret as one page under its section headings.
func (secretsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (secretsProvider) CreateTitle() string { return "Create secret" }

// The Add new version action (#1850), on a secret row and the secret's page.
const actSecretAddVersion = "addversion"

func secretAddVersionAction() console.Action {
	return console.Action{
		ID: actSecretAddVersion, Label: "Add new version",
		Fields: []console.Field{
			{Name: "payload", Label: "Secret value", Type: "textarea", Required: true, Upload: "Upload file",
				Help: "Type the value or upload a text file, at most 64 KiB. It becomes the new enabled version; " +
					"earlier versions keep their state unless Disable all past versions is checked."},
			{Name: "disablePast", Label: "Disable all past versions", Type: "checkbox"},
		},
	}
}

// addSecretVersion performs Add new version.
func addSecretVersion(st *secrets.Store, project, id string, values map[string]string) error {
	payload := values["payload"]
	if payload == "" {
		return fmt.Errorf("a version needs a value")
	}
	previous, err := st.ListVersions(project, id)
	if err != nil {
		return err
	}
	if _, err := st.AddVersion(project, id, []byte(payload)); err != nil {
		return err
	}
	if values["disablePast"] != "true" {
		return nil
	}
	for _, v := range previous {
		if v.State != secrets.StateEnabled {
			continue
		}
		if _, err := st.SetVersionState(project, id, strconv.Itoa(v.Number), secrets.StateDisabled); err != nil {
			return fmt.Errorf("the new version was added, but version %d could not be disabled: %w", v.Number, err)
		}
	}
	return nil
}

// The Rotation period list (#1850). Google's page offers "default options
// or Custom"; the docs do not name the defaults, so these are UNVERIFIED
// (re-read 2026-11-19: https://docs.cloud.google.com/secret-manager/docs/secret-rotation).
var secretRotationPresets = []struct {
	label string
	d     time.Duration
}{
	{"30 days", 30 * 24 * time.Hour},
	{"90 days", 90 * 24 * time.Hour},
	{"180 days", 180 * 24 * time.Hour},
	{"365 days", 365 * 24 * time.Hour},
}

const secretRotationCustom = "Custom"

func secretRotationOptions() []string {
	out := make([]string, 0, len(secretRotationPresets)+1)
	for _, p := range secretRotationPresets {
		out = append(out, p.label)
	}
	return append(out, secretRotationCustom)
}

// secretRotationPresetOf is the list's choice for a period.
func secretRotationPresetOf(d time.Duration) string {
	for _, p := range secretRotationPresets {
		if p.d == d {
			return p.label
		}
	}
	if d == 0 {
		return secretRotationPresets[0].label
	}
	return secretRotationCustom
}

// The encryption choices of the secret forms.
const (
	secretEncryptionGoogle = "google"
	secretEncryptionCMEK   = "cmek"
)

// secretCreateFields are Create secret's fields in Google's order; regional
// for Create regional secret, which has a Location and no replication.
func secretCreateFields(regional bool) []console.Field {
	fields := []console.Field{
		{Name: "secretId", Label: "Name", Type: "text", Required: true,
			Help:    "Up to 255 characters: letters, digits, hyphens and underscores.",
			Pattern: `^[A-Za-z0-9_\-]{1,255}$`},
	}
	if regional {
		fields = append(fields, console.Field{Name: "location", Label: "Location", Type: "text", Required: true,
			Pattern: `^[a-z]+-[a-z]+[0-9]+$`,
			Help: "A region, such as us-central1. A regional secret's data stays in that location, as " +
				"secretmanager.{location}.rep.googleapis.com serves it; its customer-managed key must be in that location."})
	}
	fields = append(fields,
		console.Field{Name: "payload", Label: "Secret value", Type: "textarea", Required: true, Upload: "Upload file",
			Help: "Type the value or upload a text file, at most 64 KiB. It becomes version 1."})
	if !regional {
		fields = append(fields,
			console.Field{Name: "replication", Label: "Replication policy", Type: "radio", Section: "Replication policy",
				Options: []string{replicationAutomatic, replicationUserManaged}, Default: replicationAutomatic,
				OptionLabels: map[string]string{replicationAutomatic: "Automatically manage locations for this secret",
					replicationUserManaged: "Manually manage locations for this secret"},
				Help: "Recorded and returned as set; there is one local store either way. It cannot be changed " +
					"after the secret is created."},
			console.Field{Name: "locations", Label: "Locations", Type: "text", Section: "Replication policy",
				ShowWhen: "replication=" + replicationUserManaged, Required: true,
				Pattern: `^[a-z0-9\-]+(\s*,\s*[a-z0-9\-]+)*$`,
				Help:    "The replica locations, comma-separated, for example us-east1, europe-west1."})
	}
	keys := secretKMSKeysField()
	keys.ShowWhen = "encryption=" + secretEncryptionCMEK
	if regional {
		keys.Label, keys.Help = "Cloud KMS key", "A Cloud KMS CryptoKey of this instance in the secret's location."
	}
	fields = append(fields,
		console.Field{Name: "encryption", Label: "Encryption", Type: "radio", Section: "Encryption",
			Options: []string{secretEncryptionGoogle, secretEncryptionCMEK}, Default: secretEncryptionGoogle,
			OptionLabels: map[string]string{secretEncryptionGoogle: "Google-managed encryption key",
				secretEncryptionCMEK: "Customer-managed encryption key (CMEK)"}},
		keys)
	fields = append(fields, secretOptionalFields(nil)...)
	return fields
}

// secretOptionalFields are the sections Create secret and Edit secret
// share, after encryption: Rotation, Notifications, Expiration, Labels,
// Annotations, Version aliases and Delayed destruction. sec gives an edit
// form its current values; nil is Create's.
func secretOptionalFields(sec *secrets.Secret) []console.Field {
	sched := secretScheduleFields(sec)
	next, period, ttl := sched[0], sched[1], sched[2]
	next.Label, next.ShowWhen = "Starting on", "setRotation"
	period.Label, period.ShowWhen = "Custom rotation period", "setRotation&rotationPreset="+secretRotationCustom
	ttl.Label, ttl.ShowWhen = "Duration", "setDestroyDelay"
	setRotation, preset := "false", secretRotationPresets[0].label
	setExpiry, setDelay := "false", "false"
	var topics, labels, annotations, aliases, expire string
	if sec != nil {
		if !sec.NextRotation.IsZero() || sec.RotationPeriod > 0 {
			setRotation = "true"
		}
		preset = secretRotationPresetOf(sec.RotationPeriod)
		if !sec.Expire.IsZero() {
			setExpiry = "true"
		}
		if sec.VersionDestroyTTL > 0 {
			setDelay = "true"
		}
		topics = strings.Join(sec.Topics, "\n")
		labels, annotations = console.FormatMap(sec.Labels), console.FormatMap(sec.Annotations)
		aliases, expire = secretAliasesDefault(sec.VersionAliases), expireDefault(*sec)
	}
	return []console.Field{
		{Name: "setRotation", Label: "Set rotation period", Type: "checkbox", Section: "Rotation", Default: setRotation,
			Help: "Publishes SECRET_ROTATE to the secret's Pub/Sub topics on the schedule; it needs at least one topic. " +
				"Secret Manager does not change the value: your subscriber adds the new version."},
		{Name: "rotationPreset", Label: "Rotation period", Type: "select", Section: "Rotation", ShowWhen: "setRotation",
			Options: secretRotationOptions(), Default: preset},
		period, next,
		{Name: "topics", Label: "Pub/Sub topics", Type: "textarea", Section: "Notifications", Default: topics,
			Help: secretTopicsHelp},
		{Name: "setExpiration", Label: "Set expiration date", Type: "checkbox", Section: "Expiration", Default: setExpiry,
			Help: "At the expiration the secret and every version of it are deleted."},
		{Name: "expireTime", Label: "Expiration time", Type: "text", Section: "Expiration", ShowWhen: "setExpiration",
			Default: expire, Pattern: secretExpireTimePattern, Help: secretExpireTimeHelp},
		{Name: "ttl", Label: "Or a duration (TTL)", Type: "text", Section: "Expiration", ShowWhen: "setExpiration",
			Pattern: secretTTLPattern, Help: secretTTLHelp},
		{Name: "labels", Label: "Labels", Type: "map", Section: "Labels", Default: labels,
			Help: "Optional. One key=value per line."},
		{Name: "annotations", Label: "Annotations", Type: "map", Section: "Annotations", Default: annotations,
			Help: "Optional. One key=value per line."},
		{Name: "versionAliases", Label: "Version aliases", Type: "map", Section: "Version aliases", Default: aliases,
			KeyPattern: `^[A-Za-z][A-Za-z0-9_\-]{0,62}$`, ValuePattern: `^[1-9][0-9]*$`, MaxEntries: secrets.MaxVersionAliases,
			KeyHelp:   "An alias starts with a letter and holds at most 63 letters, numerals, hyphens and underscores; not latest or NEW.",
			ValueHelp: "The version number the alias names.",
			Help: "Optional. One alias=version per line, such as prod=1: the version is then reachable as " +
				".../versions/prod. At most 50."},
		{Name: "setDestroyDelay", Label: "Set duration for delayed destruction", Type: "checkbox",
			Section: "Delayed destruction", Default: setDelay,
			Help: "Destroying a version then disables it and destroys it after the duration; enabling or disabling it " +
				"before then cancels the destruction."},
		ttl,
	}
}

// secretAliasesDefault is an alias map as the form's lines.
func secretAliasesDefault(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	s := make(map[string]string, len(m))
	for a, n := range m {
		s[a] = strconv.Itoa(n)
	}
	return console.FormatMap(s)
}

// secretAliasesValue reads the Version aliases field.
func secretAliasesValue(raw string) (map[string]int, error) {
	m, err := console.ParseMap(raw)
	if err != nil {
		return nil, fmt.Errorf("version aliases: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(m))
	for a, v := range m {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("version alias %q must name a version number, got %q", a, v)
		}
		out[a] = n
	}
	return out, nil
}

// secretFormValues applies the forms' checkboxes and lists to the values
// the older fields carry: an unchecked Set rotation period, Set expiration
// date or Set duration for delayed destruction clears its fields; a Rotation
// period other than Custom is its duration; Google-managed encryption has
// no keys. A request without a checkbox (an older client of the form route)
// is read as before.
func secretFormValues(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	if out["setRotation"] == "false" {
		out["nextRotationTime"], out["rotationPeriod"] = "", ""
	} else if preset := out["rotationPreset"]; preset != "" && preset != secretRotationCustom {
		for _, p := range secretRotationPresets {
			if p.label == preset {
				out["rotationPeriod"] = p.d.String()
			}
		}
	}
	if out["setExpiration"] == "false" {
		out["expireTime"], out["ttl"] = "", ""
	}
	if out["setDestroyDelay"] == "false" {
		out["versionDestroyTtl"] = ""
	}
	if out["encryption"] == secretEncryptionGoogle {
		out["kmsKeys"] = ""
	}
	return out
}

// secretOverview is the secret's Overview tab: its general details and
// metadata (https://docs.cloud.google.com/secret-manager/docs/view-secret-details).
func secretOverview(sec secrets.Secret, versions int) console.Section {
	groups := []console.PropertyGroup{
		{Heading: "General details", Properties: []console.Property{
			{Label: "Resource name", Value: sec.Name, Copy: sec.Name},
			{Label: "Created", Value: sec.Created.Format(time.RFC3339)},
			{Label: "Versions", Value: strconv.Itoa(versions)},
		}},
		{Heading: "Replication", Properties: replicationProperties(sec)},
		{Heading: "Encryption", Properties: secretEncryptionProperties(sec)},
		{Heading: "Expiration", Properties: []console.Property{{Label: "Expiration time", Value: secretExpiry(sec)}}},
		{Heading: "Notifications", Properties: []console.Property{{Label: "Pub/Sub topics", Value: secretTopicsText(sec)}}},
	}
	groups = append(groups, secretScheduleProperties(sec)...)
	aliasProps := []console.Property{}
	for _, a := range secrets.SortedAliases(sec.VersionAliases) {
		aliasProps = append(aliasProps, console.Property{Label: a, Value: "Version " + strconv.Itoa(sec.VersionAliases[a])})
	}
	if len(aliasProps) == 0 {
		aliasProps = append(aliasProps, console.Property{Label: "Aliases", Value: "None"})
	}
	groups = append(groups, console.PropertyGroup{Heading: "Version aliases", Properties: aliasProps})
	if len(sec.Labels) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Labels", Properties: sortedPairs(sec.Labels)})
	}
	if len(sec.Annotations) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Annotations", Properties: sortedPairs(sec.Annotations)})
	}
	return console.Section{ID: "overview", Label: "Overview", Kind: console.KindProperties, Groups: groups,
		Note: "Replication is fixed when the secret is created, except for its customer-managed encryption keys " +
			"(Change encryption keys); Edit secret changes the rest."}
}

// secretVersionAliases is each version's aliases, for the Versions tab.
func secretVersionAliases(sec secrets.Secret) map[int][]string {
	out := map[int][]string{}
	for _, a := range secrets.SortedAliases(sec.VersionAliases) {
		n := sec.VersionAliases[a]
		out[n] = append(out[n], a)
	}
	return out
}

// secretVersionRowActions are a version row's Actions menu: what its state
// allows, and View secret value, which opens the version's page and shows
// the value there (recorded as every reveal is).
func secretVersionRowActions(sec secrets.Secret, v secrets.Version, now time.Time, href string) []console.Action {
	destroy := secretDestroyAction(sec, now)
	var out []console.Action
	switch {
	case v.State == secrets.StateEnabled:
		out = []console.Action{{ID: "disable", Label: "Disable"}, destroy}
	case v.State == secrets.StateDisabled && !v.ScheduledDestroy.IsZero():
		out = []console.Action{{ID: "enable", Label: "Enable (cancels the destruction)"},
			{ID: "disable", Label: "Keep disabled (cancels the destruction)"}}
	case v.State == secrets.StateDisabled:
		out = []console.Action{{ID: "enable", Label: "Enable"}, destroy}
	}
	if v.State == secrets.StateEnabled {
		out = append([]console.Action{{ID: "view-value", Label: "View secret value", Href: href}}, out...)
	}
	return out
}
