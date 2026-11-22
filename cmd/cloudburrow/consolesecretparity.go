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
	loc := secretRegionalLocation(values)
	if loc == "" {
		return "", fmt.Errorf("a regional secret needs a location: a region such as us-central1, or a multi-region")
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	out["location"] = loc
	return p.secretsProvider.Create(ctx, project, out)
}

// The secrets list's columns, Google's order; the row menu is Actions.
var secretListColumns = []string{"Location", "Encryption", "Labels", "Tags", "Created", "Expiration"}

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
	tags := p.secretTags(project)
	items := make([]console.Resource, 0, len(all))
	for _, s := range all {
		items = append(items, console.Resource{
			Name: s.Name,
			Fields: map[string]string{
				"Location":   secretLocationText(s),
				"Encryption": secretEncryptionText(s),
				"Labels":     secretPairsText(s.Labels),
				"Tags":       secretPairsText(tags[secrets.FullResourcePrefix+s.Name]),
				"Created":    s.Created.Format(time.RFC3339),
				"Expiration": secretExpiry(s),
			},
			Actions: []console.Action{secretAddVersionAction()},
			// Google's info panel (https://docs.cloud.google.com/secret-manager/docs/manage-access-to-secrets,
			// https://docs.cloud.google.com/secret-manager/docs/add-labels-to-secrets,
			// read 2026-11-22): Permissions and Labels (#2142).
			Info: pubsubInfoTabs(s.Labels, "secret", true),
		})
	}
	return console.Listing{Columns: secretListColumns, Noun: "secrets", Items: items, Total: len(items), ServerFilter: sf,
		InfoOnSelect: true}, nil
}

// secretTags is the project's secrets' tags, as Resource Manager binds them
// (Secret.tags is input only, so GetSecret never returns them): by the
// secret's full resource name, each tag key's short name to its value's
// (https://docs.cloud.google.com/secret-manager/docs/create-and-manage-tags,
// read 2026-11-22: the Secrets list's Tags column, #2142).
func (p secretsProvider) secretTags(project string) map[string]map[string]string {
	if p.svc == nil || p.svc.registry == nil {
		return nil
	}
	reg := p.svc.registry()
	if reg == nil {
		return nil
	}
	bindings, err := reg.TagBindingsUnder(secrets.FullResourcePrefix + "projects/" + project + "/")
	if err != nil {
		return nil
	}
	out := map[string]map[string]string{}
	for _, b := range bindings {
		// tagValueNamespacedName is PARENT/KEY_SHORT_NAME/VALUE_SHORT_NAME.
		parts := strings.Split(b.GetTagValueNamespacedName(), "/")
		if len(parts) < 3 {
			continue
		}
		if out[b.GetParent()] == nil {
			out[b.GetParent()] = map[string]string{}
		}
		out[b.GetParent()][parts[len(parts)-2]] = parts[len(parts)-1]
	}
	return out
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
		// Google's Add new version dialog, ending in Add new version
		// (https://docs.cloud.google.com/secret-manager/docs/add-secret-version,
		// read 2026-11-22).
		ID: actSecretAddVersion, Label: "Add new version", Title: "Add new version", Submit: "Add new version",
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
	fields = append(fields, secretTypeField(regional),
		console.Field{Name: "payload", Label: "Secret value", Type: "textarea", Required: true, Upload: "Upload file",
			Help: "Type the value or upload a text file, at most 64 KiB. It becomes version 1."})
	if regional {
		// "Choose the location where you want to store your regional
		// secret": Region or Multi-region
		// (https://docs.cloud.google.com/secret-manager/regional-secrets/create-regional-secret,
		// read 2026-11-20); the multi-regions are us, eu, ca and in
		// (https://docs.cloud.google.com/secret-manager/docs/locations).
		fields = append(fields,
			console.Field{Name: "locationType", Label: "Location type", Type: "radio", Section: "Location",
				Options: []string{secretLocationRegion, secretLocationMulti}, Default: secretLocationRegion,
				OptionLabels: map[string]string{
					secretLocationRegion: "Region: stores the secret in a single region for minimal latency",
					secretLocationMulti:  "Multi-region: replicates the secret across multiple locations for high availability"}},
			console.Field{Name: "location", Label: "Region", Type: "text", Required: true, Section: "Location",
				ShowWhen: "locationType=" + secretLocationRegion, Pattern: `^[a-z]+-[a-z]+[0-9]+$`,
				Help: "A region, such as us-central1. A regional secret's data stays in that location, as " +
					"secretmanager.{location}.rep.googleapis.com serves it; its customer-managed key must be in that location."},
			console.Field{Name: "multiRegion", Label: "Multi-region", Type: "select", Section: "Location",
				ShowWhen: "locationType=" + secretLocationMulti, Options: secretMultiRegions, Default: secretMultiRegions[0],
				Help: "Its customer-managed key must be in the same multi-region."})
	}
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
	// Tags are given at creation only ("Input only. Immutable.", #1961).
	fields = append(fields, console.Field{Name: "tags", Label: "Tags", Type: "map", Section: "Tags",
		Help: "Optional. One tag key=value per line, each by its namespaced name or ID (tagKeys/…, tagValues/…), such as " +
			"100000000001/env=prod; bound through Resource Manager to the secret, and fixed once it is created."})
	return fields
}

// The Location choices of Create regional secret.
const (
	secretLocationRegion = "region"
	secretLocationMulti  = "multi-region"
)

// secretMultiRegions are the multi-regions of regional secrets.
var secretMultiRegions = []string{"us", "eu", "ca", "in"}

// secretRegionalLocation is Create regional secret's location: the Region
// typed, or the Multi-region chosen.
func secretRegionalLocation(values map[string]string) string {
	if values["locationType"] == secretLocationMulti {
		return strings.TrimSpace(values["multiRegion"])
	}
	return strings.TrimSpace(values["location"])
}

// The Secret type choices, in the docs' order: "Other DB credentials,
// Access key, Certificate, Other", and on Create regional secret Cloud SQL
// DB credentials first (https://docs.cloud.google.com/secret-manager/docs/creating-and-accessing-secrets,
// https://docs.cloud.google.com/secret-manager/regional-secrets/create-regional-secret,
// read 2026-11-20). The value is resources.proto's enum name.
var secretTypeLabels = map[string]string{
	"":                         "None",
	"CLOUD_SQL_DB_CREDENTIALS": "Cloud SQL DB credentials",
	"OTHER_DB_CREDENTIALS":     "Other DB credentials",
	"ACCESS_KEY":               "Access key",
	"CERTIFICATE":              "Certificate",
	"OTHER":                    "Other",
}

// secretTypeField is Create secret's Secret type (Preview).
func secretTypeField(regional bool) console.Field {
	opts := []string{"", "OTHER_DB_CREDENTIALS", "ACCESS_KEY", "CERTIFICATE", "OTHER"}
	if regional {
		opts = append([]string{"", "CLOUD_SQL_DB_CREDENTIALS"}, opts[1:]...)
	}
	return console.Field{Name: "secretType", Label: "Secret type (Preview)", Type: "select",
		Options: opts, OptionLabels: secretTypeLabels,
		Help: "Optional. Categorizes the secret. It is set only when the secret is created and cannot be changed."}
}

// secretTypeText is a secret's type as the Overview shows it.
func secretTypeText(t string) string {
	if l, ok := secretTypeLabels[t]; ok {
		return l
	}
	return t
}

// secretOptionalFields are the sections Create secret and Edit secret
// share, after encryption: Rotation, Notifications, Expiration, Labels,
// Annotations, Version aliases and Delayed destruction. sec gives an edit
// form its current values; nil is Create's.
func secretOptionalFields(sec *secrets.Secret) []console.Field {
	return secretOptionalFieldsWith(sec, nil)
}

// secretDelaySection is Google's name of the delayed destruction section.
const secretDelaySection = "Delay secret version destroy"

// secretRowsField is a section of key and value rows with an Add button,
// its value a group the forms turn back into key=value lines.
func secretRowsField(name, section, add, key, value, lines string) console.Field {
	m, _ := console.ParseMap(lines)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, map[string]string{"key": k, "value": m[k]})
	}
	def := ""
	if len(rows) > 0 {
		def = console.FormatGroup(rows)
	}
	return console.Field{Name: name, Label: section, Type: console.GroupFieldType, Section: section, AddLabel: add, Default: def,
		Help: "Optional.", Group: []console.Field{
			{Name: "key", Label: key, Type: "text", Required: true},
			{Name: "value", Label: value, Type: "text"}}}
}

// secretAliasesField is Version aliases' Add alias rows: the alias, and
// the version chosen from the secret's (versions), or typed on Create.
func secretAliasesField(lines string, versions []string) console.Field {
	f := secretRowsField("versionAliases", "Version aliases", "Add alias", "Alias", "Version", lines)
	f.Group[0].Pattern = `^[A-Za-z][A-Za-z0-9_\-]{0,62}$`
	f.Group[0].Help = "Starts with a letter; at most 63 letters, numerals, hyphens and underscores; not latest or new."
	if len(versions) > 0 {
		f.Group[1] = console.Field{Name: "value", Label: "Version", Type: "select", Options: versions}
	} else {
		f.Group[1].Pattern, f.Group[1].Help = `^[1-9][0-9]*$`, "The version number, 1 for the first."
	}
	f.Help = "Optional. A version is then reachable as .../versions/ALIAS. At most 50."
	return f
}

// secretVersionNumbers are the version rows' numbers, oldest first, for
// Version aliases' choices.
func secretVersionNumbers(l console.Listing) []string {
	var out []string
	for i := len(l.Items) - 1; i >= 0; i-- {
		out = append(out, lastSegment(l.Items[i].Name))
	}
	return out
}

// secretOptionalFieldsWith is secretOptionalFields with the secret's
// version numbers for Version aliases' choices.
func secretOptionalFieldsWith(sec *secrets.Secret, versions []string) []console.Field {
	sched := secretScheduleFields(sec)
	next, period, ttl := sched[0], sched[1], sched[2]
	next.Label, next.ShowWhen = "Starting on", "setRotation"
	// A date and time picker, as Google's Starting on is (#1850): the
	// browser's local time, sent as the RFC 3339 instant it names.
	next.Type, next.Pattern = console.DateTimeFieldType, ""
	next.Help = "When a SECRET_ROTATE message is first published to the secret's Pub/Sub topics, in your " +
		"browser's time zone; at least 5 minutes from now. Secret Manager does not change the value: your subscriber adds the new version."
	period.Label, period.ShowWhen = "Custom rotation period", "setRotation&rotationPreset="+secretRotationCustom
	// Google's Destruction delay duration, in days from 1 to 1000
	// (https://docs.cloud.google.com/secret-manager/docs/delay-destruction-of-secret-versions,
	// read 2026-11-22).
	delayDays := ""
	if sec != nil && sec.VersionDestroyTTL > 0 {
		delayDays = strconv.Itoa(int((sec.VersionDestroyTTL + 12*time.Hour) / (24 * time.Hour)))
	}
	ttl = console.Field{Name: "destroyDelayDays", Label: "Destruction delay duration", Type: "text", Section: secretDelaySection,
		ShowWhen: "setDestroyDelay", Default: delayDays, Pattern: `^([1-9][0-9]{0,2}|1000)$`,
		Help: "Days, 1 to 1000: a version destroyed is disabled, then destroyed this long after."}
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
		// Google's console offers a date and time only; a TTL is gcloud's
		// and REST's (https://docs.cloud.google.com/secret-manager/docs/creating-and-managing-expiring-secrets,
		// read 2026-11-22).
		{Name: "setExpiration", Label: "Set expiration date", Type: "checkbox", Section: "Expiration", Default: setExpiry,
			Help: "At the expiration the secret and every version of it are deleted."},
		{Name: "expireTime", Label: "Expiration date", Type: console.DateTimeFieldType, Section: "Expiration", ShowWhen: "setExpiration",
			Default: expire, Help: "In your browser's time zone; at least 60 seconds from now."},
		// Add label, Add annotation and Add alias rows, as Google's
		// sections have them (add-labels-to-secrets,
		// creating-and-managing-annotations, assign-alias-to-secret-version,
		// read 2026-11-22).
		secretRowsField("labels", "Labels", "Add label", "Key", "Value", labels),
		secretRowsField("annotations", "Annotations", "Add annotation", "Key", "Value", annotations),
		secretAliasesField(aliases, versions),
		{Name: "setDestroyDelay", Label: "Set duration for delayed destruction", Type: "checkbox",
			Section: secretDelaySection, Default: setDelay,
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
	// The rows of Labels, Annotations and Version aliases, as the
	// key=value lines the older fields carry.
	for _, k := range []string{"labels", "annotations", "versionAliases"} {
		if v := strings.TrimSpace(out[k]); strings.HasPrefix(v, "[") {
			if rows, err := console.ParseGroup(v); err == nil {
				m := map[string]string{}
				for _, r := range rows {
					if key := strings.TrimSpace(r["key"]); key != "" {
						m[key] = strings.TrimSpace(r["value"])
					}
				}
				out[k] = console.FormatMap(m)
			}
		}
	}
	if d := strings.TrimSpace(out["destroyDelayDays"]); d != "" && out["setDestroyDelay"] != "false" {
		if n, err := strconv.Atoi(d); err == nil && n >= 1 && n <= 1000 {
			out["versionDestroyTtl"] = (time.Duration(n) * 24 * time.Hour).String()
		} else {
			// Refused by secretScheduleValue, naming what was typed.
			out["versionDestroyTtl"] = d + " days (1 to 1000)"
		}
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
			{Label: "Secret type", Value: secretTypeText(sec.SecretType)},
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

// The selection field of the Versions tab's actions over checked versions.
const secretVersionsField = "versions"

// secretVersionAction is Disable, Enable or Destroy of secret versions with
// Google's confirmation: "In the confirmation dialog that appears, click
// Disable selected versions" (and Enable selected versions); Destroy's asks
// to "enter the secret ID to confirm, and then click Destroy selected
// versions" (https://docs.cloud.google.com/secret-manager/docs/disable-secret-version,
// .../enable-secret-version, .../destroy-secret-version, read 2026-11-20).
// The dialogs' titles and text are not in the docs
// (UNVERIFIED, re-read 2026-11-20: the destroy-secret-version page). selection
// makes it the Versions tab's action over the checked rows; otherwise it is
// one version's, from its row's Actions menu or its own page.
func secretVersionAction(verb string, sec secrets.Secret, now time.Time, selection bool) console.Action {
	var a console.Action
	switch verb {
	case "disable":
		a = console.Action{ID: "disable", Label: "Disable", Submit: "Disable selected versions",
			ConfirmTitle: "Disable selected versions?", ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "A disabled version cannot be accessed until it is enabled again."}
	case "enable":
		a = console.Action{ID: "enable", Label: "Enable", Submit: "Enable selected versions",
			ConfirmTitle: "Enable selected versions?", ConfirmStyle: console.DeleteConfirmClick,
			Confirm: "An enabled version can be accessed again."}
	case "destroy":
		a = secretDestroyAction(sec, now)
		a.Submit, a.ConfirmTitle = "Destroy selected versions", "Destroy selected versions?"
		if sec.VersionDestroyTTL > 0 {
			// With a delay, Google's button schedules them
			// (https://docs.cloud.google.com/secret-manager/docs/delay-destruction-of-secret-versions,
			// read 2026-11-22).
			a.Submit, a.ConfirmTitle = "Schedule selected versions for destruction", "Schedule selected versions for destruction?"
		}
		a.ConfirmWord = lastSegment(sec.Name)
		if a.Confirm == "" {
			a.Confirm = "A destroyed version's value is deleted permanently; the version stays listed as destroyed."
		}
	}
	if selection {
		a.ID += "versions"
		a.SelectionField = secretVersionsField
		a.Fields = []console.Field{{Name: secretVersionsField, Type: "hidden"}}
	}
	return a
}

// secretVersionSelectActions are the Versions tab's Actions over the
// checked versions: Disable, Enable and Destroy.
func secretVersionSelectActions(sec secrets.Secret, now time.Time) []console.Action {
	return []console.Action{
		secretVersionAction("disable", sec, now, true),
		secretVersionAction("enable", sec, now, true),
		secretVersionAction("destroy", sec, now, true),
	}
}

// secretVersionLifecycle is what one version's state allows, each with
// Google's confirmation. Enabling an enabled version and destroying a
// destroyed one are buttons that exist only to fail, so neither is drawn;
// a version scheduled for destruction is enabled or kept disabled, either
// of which cancels the destruction (#1278).
func secretVersionLifecycle(sec secrets.Secret, v secrets.Version, now time.Time) []console.Action {
	disable := secretVersionAction("disable", sec, now, false)
	enable := secretVersionAction("enable", sec, now, false)
	destroy := secretVersionAction("destroy", sec, now, false)
	switch {
	case v.State == secrets.StateEnabled:
		return []console.Action{disable, destroy}
	case v.State == secrets.StateDisabled && !v.ScheduledDestroy.IsZero():
		enable.Label = "Enable (cancels the destruction)"
		disable.Label, disable.Submit = "Keep disabled (cancels the destruction)", "Keep selected versions disabled"
		return []console.Action{enable, disable}
	case v.State == secrets.StateDisabled:
		return []console.Action{enable, destroy}
	}
	return nil
}

// secretVersionRowActions are a version row's Actions menu: View secret
// value, which shows the value in a dialog ending in Done ("A dialog appears
// displaying the value of the secret version. Click Done to exit the
// dialog", https://docs.cloud.google.com/secret-manager/docs/access-secret-version,
// read 2026-11-20), recorded as every reveal is, then what its state allows.
func secretVersionRowActions(sec secrets.Secret, v secrets.Version, now time.Time) []console.Action {
	out := secretVersionLifecycle(sec, v, now)
	if v.State == secrets.StateEnabled {
		out = append([]console.Action{{ID: "view-value", Label: "View secret value", Reveal: true}}, out...)
	}
	return out
}

// actSecretVersions performs Disable, Enable or Destroy on the versions
// checked on the Versions tab, one after another; the first refusal stops
// it and names the version.
func actSecretVersions(st *secrets.Store, project, id, verb, list string) error {
	var numbers []string
	for _, n := range strings.FieldsFunc(list, func(r rune) bool { return r == '\n' || r == ',' || r == '\r' }) {
		if n = strings.TrimSpace(n); n != "" {
			numbers = append(numbers, n)
		}
	}
	if len(numbers) == 0 {
		return fmt.Errorf("select at least one version")
	}
	for _, n := range numbers {
		var err error
		switch verb {
		case "disable":
			_, err = st.SetVersionState(project, id, n, secrets.StateDisabled)
		case "enable":
			_, err = st.SetVersionState(project, id, n, secrets.StateEnabled)
		case "destroy":
			_, err = st.DestroyVersion(project, id, n)
		default:
			return fmt.Errorf("unknown action %q", verb)
		}
		if err != nil {
			return fmt.Errorf("version %s: %w", n, err)
		}
	}
	return nil
}

// DeleteWords: Google's Delete secret dialog asks for the secret's name
// and ends in Delete secret (https://docs.cloud.google.com/secret-manager/docs/delete-secrets,
// read 2026-11-22).
func (secretsProvider) DeleteWords() console.DeleteWords {
	return console.DeleteWords{Submit: "Delete secret"}
}

// DeleteWords: a regional secret's dialog is the same.
func (secretsRegionalProvider) DeleteWords() console.DeleteWords {
	return console.DeleteWords{Submit: "Delete secret"}
}
