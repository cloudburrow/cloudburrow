package main

// Create repository and Edit repository on the Artifact Registry pages,
// through CreateRepository and UpdateRepository on the Run port, laid out as
// Google's Create repository page (#1855,
// https://docs.cloud.google.com/artifact-registry/docs/repositories/create-repos,
// read 2026-11-19): Name, Format, Mode with each mode's settings, Location
// type and Region, Description, Labels, Encryption, Immutable image tags,
// Cleanup policies and Artifact Analysis, ending in Create.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (p artifactRegistryProvider) withClient(ctx context.Context, fn func(*artifactregistry.Client) error) error {
	addr := ""
	if p.runAddr != nil {
		addr = p.runAddr()
	}
	if addr == "" {
		return errors.New("the Cloud Run adapter, which serves Artifact Registry, is not running")
	}
	c, err := artifactregistry.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Artifact Registry: %w", err)
	}
	defer c.Close()
	return fn(c)
}

// The formats Google's Format list offers, in its order, and the words it
// shows them as. Go and Kubeflow Pipelines are drawn disabled: this
// instance serves neither (CreateRepository refuses them).
var (
	arFormats      = []string{"DOCKER", "MAVEN", "NPM", "PYTHON", "APT", "YUM", "KFP", "GO", "GENERIC"}
	arFormatLabels = map[string]string{"DOCKER": "Docker", "MAVEN": "Maven", "NPM": "npm", "PYTHON": "Python",
		"APT": "Apt", "YUM": "Yum", "KFP": "Kubeflow Pipelines", "GO": "Go", "GENERIC": "Generic"}
	arFormatsUnavailable = map[string]string{
		"KFP": "Not available locally: this instance serves no Kubeflow Pipelines template registry.",
		"GO":  "Not available locally: this instance serves no Go module proxy.",
	}
	arModeLabels = map[string]string{"STANDARD_REPOSITORY": "Standard", "REMOTE_REPOSITORY": "Remote", "VIRTUAL_REPOSITORY": "Virtual"}
)

// The create page's choices, as Google words them.
const (
	arLocRegion       = "Region"
	arLocMultiRegion  = "Multi-Region"
	arEncGoogle       = "Google-managed encryption key"
	arEncCMEK         = "Customer-managed key"
	arEnabled         = "Enabled"
	arDisabled        = "Disabled"
	arCleanupDryRun   = "Dry run"
	arCleanupDelete   = "Delete artifacts"
	arRemotePreset    = "Preset"
	arRemoteCustom    = "Custom"
	arRemoteAR        = "Artifact Registry repository"
	arAuthNone        = "Unauthenticated"
	arAuthUser        = "Authenticated"
	arPolicyDelete    = "Conditional delete"
	arPolicyKeep      = "Conditional keep"
	arPolicyMostRecnt = "Keep most recent versions"
	arTagAny          = "Any tag state"
	arTagTagged       = "Tagged"
	arTagUntagged     = "Untagged"

	arNoEgress = "Not available locally: an upstream outside this instance (a preset such as Docker Hub, PyPI, npmjs or " +
		"Maven Central, or a custom URL) needs the internet, and this instance makes no calls off the machine."
	arNoUpstreamAuth = "Not available locally: an Artifact Registry upstream of this instance is read without credentials."
	arNoScanning     = "Not available locally: vulnerability scanning is Artifact Analysis (the Container Scanning API), " +
		"which CloudBurrow does not run."
)

// arMultiRegionLabels name the multi-regions as Google's Multi-region list does.
var arMultiRegionLabels = map[string]string{
	"asia": "asia (multiple regions in Asia)", "europe": "europe (multiple regions in European Union)",
	"us": "us (multiple regions in United States)"}

// arLocations splits the served locations into regions and multi-regions,
// each region named "us-central1 (Iowa)" where its city is known.
func arLocations() (regions, multi []string, labels map[string]string) {
	city := map[string]string{}
	for _, r := range bqRegionNames {
		city[r[0]] = r[1]
	}
	labels = map[string]string{}
	for _, l := range runadapter.ArtifactRegistryLocations {
		if _, ok := arMultiRegionLabels[l]; ok {
			multi = append(multi, l)
			labels[l] = arMultiRegionLabels[l]
			continue
		}
		regions = append(regions, l)
		if c := city[l]; c != "" {
			labels[l] = l + " (" + c + ")"
		}
	}
	return regions, multi, labels
}

// arCleanupGroup is the Cleanup policies rows, one per policy, with
// Google's fields; Add a cleanup policy adds one.
func arCleanupGroup(def string) console.Field {
	return console.Field{Name: "cleanupPolicies", Label: "Cleanup policy", Type: console.GroupFieldType, Default: def,
		Section: "Cleanup policies", AddLabel: "Add a cleanup policy", Group: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `[a-z]([a-z0-9\-]{0,61}[a-z0-9])?`,
				Help: "lowercase letters, digits and hyphens, starting with a letter"},
			{Name: "type", Label: "Policy type", Type: "select", Options: []string{arPolicyDelete, arPolicyKeep, arPolicyMostRecnt}},
			{Name: "tagState", Label: "Tag state", Type: "select", Options: []string{arTagAny, arTagTagged, arTagUntagged}},
			{Name: "tagPrefixes", Label: "Tag prefixes", Type: "text"},
			{Name: "versionPrefixes", Label: "Version prefixes", Type: "text"},
			{Name: "packagePrefixes", Label: "Package prefixes", Type: "text"},
			{Name: "olderThan", Label: "Older than", Type: "text", Pattern: `([0-9]+[smhd])?`, Help: "a duration such as 30d, 12h or 90s"},
			{Name: "newerThan", Label: "Newer than", Type: "text", Pattern: `([0-9]+[smhd])?`, Help: "a duration such as 30d, 12h or 90s"},
			{Name: "keepCount", Label: "Keep count", Type: "text", Pattern: `([0-9]+)?`, Help: "a number of versions"},
		},
		Help: "Conditional delete and Conditional keep take the tag state, prefixes and ages; Keep most recent versions " +
			"takes Keep count and Package prefixes. Tag prefixes need Tagged. Policies are stored and read back; " +
			"this instance does not run them (#1897)."}
}

// arCleanupModeField is Dry run or Delete artifacts.
func arCleanupModeField(dry bool) console.Field {
	def := arCleanupDelete
	if dry {
		def = arCleanupDryRun
	}
	return console.Field{Name: "cleanupMode", Label: "Cleanup policies", Type: "radio", Section: "Cleanup policies",
		Options: []string{arCleanupDryRun, arCleanupDelete}, Default: def,
		Help: "Dry run evaluates the policies and deletes nothing; Delete artifacts applies them."}
}

// arImmutableField is Immutable image tags, on a Docker repository.
func arImmutableField(on bool, showWhen string) console.Field {
	def := arDisabled
	if on {
		def = arEnabled
	}
	return console.Field{Name: "immutableTags", Label: "Immutable image tags", Type: "radio", ShowWhen: showWhen,
		Options: []string{arEnabled, arDisabled}, Default: def,
		Help: "Enabled: a tag always names the same image; it cannot be moved or deleted, and a tagged image cannot be deleted."}
}

// CreateForm implements console.Creator: Google's Create repository page.
func (artifactRegistryProvider) CreateForm() (string, []console.Field) {
	regions, multi, labels := arLocations()
	return "Create Repository", []console.Field{
		{Name: "id", Label: "Name", Type: "text", Required: true, Pattern: "^[a-z]([a-z0-9\\-]{0,61}[a-z0-9])?$",
			Help: "Lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "format", Label: "Format", Type: "radio", Options: arFormats, OptionLabels: arFormatLabels, Default: "DOCKER",
			UnavailableOptions: arFormatsUnavailable},
		{Name: "mode", Label: "Mode", Type: "radio", Options: []string{"STANDARD_REPOSITORY", "REMOTE_REPOSITORY", "VIRTUAL_REPOSITORY"},
			OptionLabels: arModeLabels, Default: "STANDARD_REPOSITORY",
			Help: "Remote: Docker, Maven, npm and Python, caching another repository of this instance. Virtual: Maven, npm and Python."},
		// Maven's version policy (maven_config).
		{Name: "versionPolicy", Label: "Version policy", Type: "radio", ShowWhen: "format=MAVEN&mode=STANDARD_REPOSITORY",
			Options:      []string{"VERSION_POLICY_UNSPECIFIED", "RELEASE", "SNAPSHOT"},
			OptionLabels: map[string]string{"VERSION_POLICY_UNSPECIFIED": "None", "RELEASE": "Release", "SNAPSHOT": "Snapshot"},
			Default:      "VERSION_POLICY_UNSPECIFIED"},
		{Name: "allowSnapshotOverwrites", Label: "Allow snapshot overwrites", Type: "checkbox",
			ShowWhen: "format=MAVEN&mode=STANDARD_REPOSITORY&versionPolicy=SNAPSHOT"},
		// A remote repository's source and upstream.
		{Name: "remoteSource", Label: "Remote repository source", Type: "radio", ShowWhen: "mode=REMOTE_REPOSITORY",
			Options: []string{arRemotePreset, arRemoteCustom, arRemoteAR}, Default: arRemoteAR,
			UnavailableOptions: map[string]string{arRemotePreset: arNoEgress, arRemoteCustom: arNoEgress}},
		{Name: "upstream", Label: "Artifact Registry repository", Type: "text", ShowWhen: "mode=REMOTE_REPOSITORY", Required: true,
			Help: "A standard repository of the same format: its ID in this location, or projects/PROJECT/locations/LOCATION/repositories/REPOSITORY."},
		{Name: "remoteAuth", Label: "Remote repository authentication mode", Type: "radio", ShowWhen: "mode=REMOTE_REPOSITORY",
			Options: []string{arAuthUser, arAuthNone}, Default: arAuthNone,
			UnavailableOptions: map[string]string{arAuthUser: arNoUpstreamAuth}},
		// A virtual repository's upstream policies.
		{Name: "upstreams", Label: "Virtual Repository Upstreams", Type: console.GroupFieldType, ShowWhen: "mode=VIRTUAL_REPOSITORY",
			AddLabel: "Add Upstream", Group: []console.Field{
				{Name: "policy", Label: "Policy name", Type: "text", Required: true, Pattern: `[a-z]([a-z0-9\-]{0,61}[a-z0-9])?`,
					Help: "lowercase letters, digits and hyphens"},
				{Name: "repository", Label: "Repository", Type: "text", Required: true},
				{Name: "priority", Label: "Priority", Type: "text", Required: true, Pattern: `[0-9]+`, Help: "a whole number; the highest is read first"},
			},
			Help: "Each upstream is a standard or remote repository of the same format in this location: its ID, or its full name."},
		{Name: "locationType", Label: "Location type", Type: "radio", Options: []string{arLocRegion, arLocMultiRegion}, Default: arLocRegion},
		{Name: "location", Label: "Region", Type: "select", Options: regions, OptionLabels: labels, Default: "us-central1",
			ShowWhen: "locationType=" + arLocRegion},
		{Name: "multiRegion", Label: "Multi-region", Type: "select", Options: multi, OptionLabels: labels, Default: "us",
			ShowWhen: "locationType=" + arLocMultiRegion},
		{Name: "description", Label: "Description", Type: "text"},
		arLabelsField(""),
		{Name: "encryption", Label: "Encryption", Type: "radio", Section: "Encryption", Options: []string{arEncGoogle, arEncCMEK}, Default: arEncGoogle},
		{Name: "kmsKey", Label: "Select a customer-managed key", Type: "text", Section: "Encryption", Required: true,
			ShowWhen: "encryption=" + arEncCMEK,
			Help: "A Cloud KMS key of this instance in the repository's location, projects/P/locations/L/keyRings/R/cryptoKeys/K. " +
				"It cannot be changed later. The key is stored on the repository; nothing is encrypted with it locally (#1897)."},
		withSection(arImmutableField(false, "format=DOCKER"), "Immutable image tags"),
		arCleanupModeField(true),
		arCleanupGroup(""),
		{Name: "vulnerabilityScanning", Label: "Vulnerability scanning", Type: "radio", Section: "Artifact Analysis",
			Options: []string{arEnabled, arDisabled}, Default: arDisabled,
			UnavailableOptions: map[string]string{arEnabled: arNoScanning}},
	}
}

func withSection(f console.Field, s string) console.Field {
	f.Section = s
	return f
}

// arLabelsField is Labels, key and value rows (Add Label).
func arLabelsField(def string) console.Field {
	return console.Field{Name: "labels", Label: "Labels", Type: "map", Default: def,
		KeyPattern: `^\p{Ll}[\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`, ValuePattern: `^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`,
		KeyHelp:   "A key is 1 to 63 lowercase letters, digits, _ and -, starting with a lowercase letter.",
		ValueHelp: "A value is 0 to 63 lowercase letters, digits, _ and -.", MaxEntries: 64}
}

// CreateOnPage, CreateLayout, CreateTitle and CreateSubmit make Create
// Repository Google's one page ending in Create.
func (artifactRegistryProvider) CreateOnPage() bool   { return true }
func (artifactRegistryProvider) CreateLayout() string { return console.CreateLayoutPage }
func (artifactRegistryProvider) CreateTitle() string  { return "Create repository" }
func (artifactRegistryProvider) CreateSubmit() string { return "Create" }
func (artifactRegistryProvider) createFields() []console.Field {
	_, f := artifactRegistryProvider{}.CreateForm()
	return f
}

// arFormLabels reads the labels field: the map field's JSON object, or
// key=value pairs comma separated, as the form sent them before #1855.
func arFormLabels(v string) (map[string]string, error) {
	if strings.HasPrefix(strings.TrimSpace(v), "{") {
		return console.ParseMap(v)
	}
	return parseAnnotations(v)
}

// Create implements console.Creator: CreateRepository with every setting
// the page holds.
func (p artifactRegistryProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		project = p.defaultProject
	}
	if err := refuseUnavailable(v, p.createFields()); err != nil {
		return "", err
	}
	labels, err := arFormLabels(v["labels"])
	if err != nil {
		return "", err
	}
	id, loc := strings.TrimSpace(v["id"]), strings.TrimSpace(v["location"])
	if v["locationType"] == arLocMultiRegion {
		loc = strings.TrimSpace(v["multiRegion"])
	}
	if id == "" || loc == "" {
		return "", errors.New("a name and a region are required")
	}
	format := arpb.Repository_DOCKER
	if f := strings.TrimSpace(v["format"]); f != "" {
		n, ok := arpb.Repository_Format_value[f]
		if !ok {
			return "", fmt.Errorf("unknown format %q", f)
		}
		format = arpb.Repository_Format(n)
	}
	parent := "projects/" + project + "/locations/" + loc
	repo := &arpb.Repository{Format: format, Description: strings.TrimSpace(v["description"]), Labels: labels}
	upstream := v["upstream"]
	if strings.TrimSpace(v["mode"]) == "VIRTUAL_REPOSITORY" && strings.TrimSpace(v["upstreams"]) != "" {
		if err := setVirtualUpstreams(repo, parent, v["upstreams"]); err != nil {
			return "", err
		}
	} else if err := setRepositoryMode(repo, parent, v["mode"], upstream); err != nil {
		return "", err
	}
	if format == arpb.Repository_MAVEN && repo.GetMode() != arpb.Repository_REMOTE_REPOSITORY && repo.GetMode() != arpb.Repository_VIRTUAL_REPOSITORY {
		if vp := strings.TrimSpace(v["versionPolicy"]); vp != "" && vp != "VERSION_POLICY_UNSPECIFIED" || v["allowSnapshotOverwrites"] == "true" {
			mc := &arpb.Repository_MavenRepositoryConfig{
				VersionPolicy:           arpb.Repository_MavenRepositoryConfig_VersionPolicy(arpb.Repository_MavenRepositoryConfig_VersionPolicy_value[vp]),
				AllowSnapshotOverwrites: v["allowSnapshotOverwrites"] == "true"}
			repo.FormatConfig = &arpb.Repository_MavenConfig{MavenConfig: mc}
		}
	}
	if format == arpb.Repository_DOCKER && v["immutableTags"] == arEnabled {
		repo.FormatConfig = &arpb.Repository_DockerConfig{DockerConfig: &arpb.Repository_DockerRepositoryConfig{ImmutableTags: true}}
	}
	if v["encryption"] == arEncCMEK {
		repo.KmsKeyName = strings.TrimSpace(v["kmsKey"])
		if repo.KmsKeyName == "" {
			return "", errors.New("Customer-managed key: select a key")
		}
	}
	if _, ok := v["cleanupPolicies"]; ok {
		pols, err := arCleanupFromGroup(v["cleanupPolicies"])
		if err != nil {
			return "", err
		}
		repo.CleanupPolicies = pols
		repo.CleanupPolicyDryRun = v["cleanupMode"] != arCleanupDelete
	}
	if _, ok := v["vulnerabilityScanning"]; ok {
		repo.VulnerabilityScanningConfig = &arpb.Repository_VulnerabilityScanningConfig{
			EnablementConfig: arpb.Repository_VulnerabilityScanningConfig_DISABLED}
	}
	err = p.withClient(ctx, func(c *artifactregistry.Client) error {
		op, err := c.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: repo})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return id, err
}

// setRepositoryMode sets a new repository's mode and its upstreams, from
// the create form: a remote repository's common_repository, the one
// upstream, or a virtual one's upstream policies, the first highest.
func setRepositoryMode(r *arpb.Repository, parent, mode, upstream string) error {
	var ups []string
	for _, u := range strings.Split(upstream, ",") {
		if u = strings.TrimSpace(u); u != "" {
			if !strings.HasPrefix(u, "projects/") {
				u = parent + "/repositories/" + u
			}
			ups = append(ups, u)
		}
	}
	switch strings.TrimSpace(mode) {
	case "", "STANDARD_REPOSITORY":
		// The page sends the hidden remote fields too; a standard
		// repository has no upstream, so they are not read.
	case "REMOTE_REPOSITORY":
		if len(ups) != 1 {
			return errors.New("a remote repository needs exactly one upstream repository")
		}
		r.Mode = arpb.Repository_REMOTE_REPOSITORY
		r.ModeConfig = &arpb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &arpb.RemoteRepositoryConfig{
			RemoteSource: &arpb.RemoteRepositoryConfig_CommonRepository{CommonRepository: &arpb.RemoteRepositoryConfig_CommonRemoteRepository{Uri: ups[0]}}}}
	case "VIRTUAL_REPOSITORY":
		if len(ups) == 0 {
			return errors.New("a virtual repository needs at least one upstream repository: Add Upstream")
		}
		vc := &arpb.VirtualRepositoryConfig{}
		for i, u := range ups {
			vc.UpstreamPolicies = append(vc.UpstreamPolicies, &arpb.UpstreamPolicy{Id: u[strings.LastIndex(u, "/")+1:], Repository: u, Priority: int32(100 - i)})
		}
		r.Mode = arpb.Repository_VIRTUAL_REPOSITORY
		r.ModeConfig = &arpb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: vc}
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	return nil
}

// setVirtualUpstreams sets a virtual repository's upstream policies from
// the page's Virtual Repository Upstreams rows: Policy name, Repository and
// Priority each.
func setVirtualUpstreams(r *arpb.Repository, parent, group string) error {
	rows, err := console.ParseGroup(group)
	if err != nil {
		return fmt.Errorf("Virtual Repository Upstreams: %w", err)
	}
	if len(rows) == 0 {
		return errors.New("a virtual repository needs at least one upstream repository: Add Upstream")
	}
	vc := &arpb.VirtualRepositoryConfig{}
	seen := map[string]bool{}
	for i, row := range rows {
		id, u := strings.TrimSpace(row["policy"]), strings.TrimSpace(row["repository"])
		if id == "" || u == "" {
			return fmt.Errorf("upstream %d: a policy name and a repository are required", i+1)
		}
		if seen[id] {
			return fmt.Errorf("upstream policy %q is named twice", id)
		}
		seen[id] = true
		if !strings.HasPrefix(u, "projects/") {
			u = parent + "/repositories/" + u
		}
		pr, err := strconv.Atoi(strings.TrimSpace(row["priority"]))
		if err != nil {
			return fmt.Errorf("upstream %q: priority %q is not a whole number", id, row["priority"])
		}
		vc.UpstreamPolicies = append(vc.UpstreamPolicies, &arpb.UpstreamPolicy{Id: id, Repository: u, Priority: int32(pr)})
	}
	r.Mode = arpb.Repository_VIRTUAL_REPOSITORY
	r.ModeConfig = &arpb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: vc}
	return nil
}

// editRepositoryAction is Edit repository on a repository's page, with the
// create page's sections a repository can change: Description, Labels,
// Immutable image tags (Docker), and Cleanup policies, prefilled.
func editRepositoryAction(r *arpb.Repository) console.Action {
	fields := []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: r.GetDescription()},
		arLabelsField(console.FormatMap(r.GetLabels())),
	}
	if r.GetFormat() == arpb.Repository_DOCKER {
		fields = append(fields, withSection(arImmutableField(r.GetDockerConfig().GetImmutableTags(), ""), "Immutable image tags"))
	}
	if r.GetMode() == arpb.Repository_STANDARD_REPOSITORY || r.GetMode() == arpb.Repository_REMOTE_REPOSITORY {
		fields = append(fields, arCleanupModeField(r.GetCleanupPolicyDryRun()), arCleanupGroup(arCleanupToGroup(r)))
	}
	return console.Action{ID: "edit-repository", Label: "Edit Repository", Fields: fields}
}

func repoLabelsText(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// editRepository is UpdateRepository of what Edit Repository sent.
func (p artifactRegistryProvider) editRepository(ctx context.Context, r *arpb.Repository, v map[string]string) error {
	labels, err := arFormLabels(v["labels"])
	if err != nil {
		return err
	}
	in := &arpb.Repository{Name: r.GetName(), Description: strings.TrimSpace(v["description"]), Labels: labels}
	mask := []string{"description", "labels"}
	if t, ok := v["immutableTags"]; ok && r.GetFormat() == arpb.Repository_DOCKER && (t == arEnabled) != r.GetDockerConfig().GetImmutableTags() {
		in.FormatConfig = &arpb.Repository_DockerConfig{DockerConfig: &arpb.Repository_DockerRepositoryConfig{ImmutableTags: t == arEnabled}}
		mask = append(mask, "docker_config.immutable_tags")
	}
	if g, ok := v["cleanupPolicies"]; ok {
		pols, err := arCleanupFromGroup(g)
		if err != nil {
			return err
		}
		in.CleanupPolicies, in.CleanupPolicyDryRun = pols, v["cleanupMode"] != arCleanupDelete
		mask = append(mask, "cleanup_policies", "cleanup_policy_dry_run")
	}
	return p.withClient(ctx, func(c *artifactregistry.Client) error {
		_, err := c.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
		return err
	})
}

var (
	_ console.Creator         = artifactRegistryProvider{}
	_ console.PageCreator     = artifactRegistryProvider{}
	_ console.CreateLayouter  = artifactRegistryProvider{}
	_ console.CreateTitler    = artifactRegistryProvider{}
	_ console.CreateSubmitter = artifactRegistryProvider{}
)
