package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// memorystoreProvider is Memorystore's Instances page (#1302), read and
// changed through the admin API's own methods, so the screen shows what
// gcloud and the SDK see: Create instance (CreateInstance), Edit
// (UpdateInstance: display name, labels, capacity, configs) and Delete
// (DeleteInstance), and Flush, which empties the instance `up` runs as
// FLUSHALL does — the Memorystore console's own data-plane flush; the
// admin API has no method for it.
type memorystoreProvider struct {
	// api is the admin API `up` serves; a stand-in describing the server
	// at the RESP address alone before it has started.
	api     func() *redisadmin.Server
	backend redisadmin.Valkey
}

// newMemorystoreProvider builds the screen for the server at addr, over
// svc's API when it runs.
func newMemorystoreProvider(cfg config.Config, addr string, svc *memorystoreAdminService) memorystoreProvider {
	be := redisadmin.Valkey{Addr: func() string { return addr }}
	fallback := redisadmin.NewServer(memorystoreInstance(cfg, time.Time{}, be.Addr), be)
	return memorystoreProvider{backend: be, api: func() *redisadmin.Server {
		if svc != nil && svc.api != nil {
			return svc.api
		}
		return fallback
	}}
}

func (memorystoreProvider) ID() string    { return "memorystore" }
func (memorystoreProvider) Title() string { return "Memorystore" }

const memorystoreNote = "The instance memorystore is the Valkey server --services memorystore runs; it cannot be edited or deleted. " +
	"Each instance created here is a server of its own, BASIC tier (REDIS_6_X Redis 6.2, REDIS_7_0 Redis 7.0, REDIS_7_2 Valkey), without in-transit encryption."

// List implements console.Provider: the project's instances.
func (p memorystoreProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{
		Columns:    []string{"Location", "Version", "Endpoint", "Tier", "Capacity", "AUTH"},
		NameColumn: "Instance ID",
		Noun:       "instances",
		Note:       memorystoreNote,
	}
	resp, err := p.api().ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		l.Unavailable = "cannot reach Memorystore: " + err.Error()
		return l, nil
	}
	for _, inst := range resp.GetInstances() {
		r := console.Resource{
			Name:   memorystoreID(inst.GetName()),
			Status: inst.GetState().String(),
			Fields: map[string]string{
				"Location": inst.GetLocationId(),
				"Version":  inst.GetRedisVersion(),
				"Endpoint": memorystoreEndpoint(inst),
				"Tier":     inst.GetTier().String(),
				"Capacity": fmt.Sprintf("%d GB", inst.GetMemorySizeGb()),
				"AUTH":     map[bool]string{true: "Enabled", false: "Disabled"}[inst.GetAuthEnabled()],
			},
		}
		r.Actions = p.Actions(r)
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return l, nil
}

func memorystoreEndpoint(inst *redispb.Instance) string {
	if inst.GetHost() == "" {
		return "—"
	}
	return net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort())))
}

// memorystoreID is the console's name for an instance: its ID, with its
// region when it is not the default one, region/ID.
func memorystoreID(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) != 6 {
		return name
	}
	if parts[3] == redisadmin.Region {
		return parts[5]
	}
	return parts[3] + "/" + parts[5]
}

// memorystoreName is the resource name of the console's name in project.
func memorystoreName(project, id string) (string, error) {
	region := redisadmin.Region
	if r, i, ok := strings.Cut(id, "/"); ok {
		region, id = r, i
	}
	if id == "" || region == "" || strings.Contains(id, "/") {
		return "", errors.New("no such instance: " + id)
	}
	return "projects/" + project + "/locations/" + region + "/instances/" + id, nil
}

// Actions implements console.Actor: Flush, on the instance `up` runs.
func (memorystoreProvider) Actions(r console.Resource) []console.Action {
	if r.Name != redisadmin.InstanceID {
		return nil
	}
	return []console.Action{{ID: "flush", Label: "Flush", Destructive: true,
		Confirm: "Every key in every database of the instance is deleted (FLUSHALL)."}}
}

// Act implements console.Actor.
func (p memorystoreProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "flush" {
		return fmt.Errorf("unknown action %q", action)
	}
	if name != redisadmin.InstanceID {
		return errors.New("Flush empties the instance memorystore only: connect to " + name + " with a Redis client")
	}
	// The instance must be this project's, as the list shows it.
	if _, err := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{
		Name: "projects/" + project + "/locations/" + redisadmin.Region + "/instances/" + redisadmin.InstanceID}); err != nil {
		return errors.New("project " + project + " has no Memorystore instance memorystore")
	}
	return p.backend.Flush(ctx)
}

// CreateForm implements console.Creator: CreateInstance's fields that work
// here. Standard tier and other versions are refused by the API, so they
// are not offered.
func (memorystoreProvider) CreateForm() (string, []console.Field) {
	return "Create instance", []console.Field{
		{Name: "id", Label: "Instance ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`,
			Help: "1 to 40 lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "displayName", Label: "Display name", Type: "text"},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: redisadmin.Region,
			Help: "CloudBurrow does not place instances geographically."},
		{Name: "memorySizeGb", Label: "Capacity (GB)", Type: "number", Required: true, Default: "1", Pattern: `^[0-9]{1,3}$`,
			Help: "1 to 300: the server's maxmemory."},
		{Name: "redisVersion", Label: "Version", Type: "select", Default: redisadmin.DefaultRedisVersion, Options: redisadmin.Versions,
			Help: "REDIS_6_X is Redis 6.2, REDIS_7_0 Redis 7.0, REDIS_7_2 Valkey 8.1."},
		{Name: "authEnabled", Label: "Enable AUTH", Type: "checkbox",
			Help: "Clients must AUTH with the instance's AUTH string, which GetInstanceAuthString returns."},
		{Name: "labels", Label: "Labels", Type: "map", Help: "Optional. One key=value per line."},
		{Name: "redisConfigs", Label: "Redis configs", Type: "map",
			Help: "Optional. One key=value per line: maxmemory-policy, notify-keyspace-events, activedefrag, " +
				"lfu-decay-time, lfu-log-factor, stream-node-max-bytes, stream-node-max-entries, maxmemory-gb."},
	}
}

// CreateOnPage implements console.PageCreator.
func (memorystoreProvider) CreateOnPage() bool { return true }

// Create implements console.Creator, waiting for the operation, as the
// Memorystore console shows an instance once it is ready.
func (p memorystoreProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	size, err := strconv.Atoi(strings.TrimSpace(values["memorySizeGb"]))
	if err != nil {
		return "", fmt.Errorf("capacity %q is not a number of GB", values["memorySizeGb"])
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	configs, err := console.ParseMap(values["redisConfigs"])
	if err != nil {
		return "", err
	}
	region := strings.TrimSpace(values["region"])
	op, err := p.api().CreateInstance(ctx, &redispb.CreateInstanceRequest{
		Parent: "projects/" + project + "/locations/" + region, InstanceId: strings.TrimSpace(values["id"]),
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: int32(size), DisplayName: values["displayName"],
			AuthEnabled: values["authEnabled"] == "true", Labels: labels, RedisConfigs: configs, RedisVersion: values["redisVersion"]}})
	if err != nil {
		return "", err
	}
	if err := p.wait(ctx, op.GetName()); err != nil {
		return "", err
	}
	return memorystoreID("projects/" + project + "/locations/" + region + "/instances/" + strings.TrimSpace(values["id"])), nil
}

// wait polls an operation until it is done, and returns its error.
func (p memorystoreProvider) wait(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	for {
		op, err := p.api().Operation(name)
		if err != nil {
			return err
		}
		if op.GetDone() {
			if e := op.GetError(); e != nil {
				return errors.New(e.GetMessage())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation %s is still running", name)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Delete implements console.Deleter: DeleteInstance, waited for.
func (p memorystoreProvider) Delete(ctx context.Context, project, id string) error {
	name, err := memorystoreName(project, id)
	if err != nil {
		return err
	}
	op, err := p.api().DeleteInstance(ctx, &redispb.DeleteInstanceRequest{Name: name})
	if err != nil {
		return err
	}
	return p.wait(ctx, op.GetName())
}

// Detail implements console.Detailer: an instance's properties, and its
// Edit form for an instance CreateInstance made.
func (p memorystoreProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 || (len(path) == 2 && path[0] == redisadmin.Region) {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	inst, err := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance: " + consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "State", Value: inst.GetState().String()},
		{Label: "Endpoint", Value: memorystoreEndpoint(inst)},
		{Label: "Version", Value: inst.GetRedisVersion()},
		{Label: "Tier", Value: inst.GetTier().String()},
		{Label: "Capacity", Value: fmt.Sprintf("%d GB", inst.GetMemorySizeGb())},
		{Label: "AUTH", Value: map[bool]string{true: "Enabled", false: "Disabled"}[inst.GetAuthEnabled()]},
		{Label: "Display name", Value: orDash(inst.GetDisplayName())},
		{Label: "Labels", Value: orDash(memorystorePairs(inst.GetLabels()))},
		{Label: "Redis configs", Value: orDash(memorystorePairs(inst.GetRedisConfigs()))},
		{Label: "Persistence", Value: memorystorePersistence(inst.GetPersistenceConfig())},
	}
	if inst.GetAuthEnabled() {
		if a, err := p.api().GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name}); err == nil {
			props = append(props, console.Property{Label: "AUTH string", Value: orDash(a.GetAuthString())})
		}
	}
	if m := inst.GetStatusMessage(); m != "" {
		props = append(props, console.Property{Label: "Status", Value: m})
	}
	d := console.Detail{Sections: []console.Section{{ID: "config", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Properties: props}}}}}
	if memorystoreID(name) == redisadmin.InstanceID {
		return d, nil
	}
	d.Edit = &console.EditForm{
		Label: "Edit instance",
		Fields: []console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Default: inst.GetDisplayName()},
			{Name: "memorySizeGb", Label: "Capacity (GB)", Type: "number", Required: true, Pattern: `^[0-9]{1,3}$`,
				Default: strconv.Itoa(int(inst.GetMemorySizeGb())), Help: "1 to 300: set on the running server, which keeps its keys."},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(inst.GetLabels())},
			{Name: "redisConfigs", Label: "Redis configs", Type: "map", Default: console.FormatMap(inst.GetRedisConfigs()),
				Help: "A config removed here goes back to its default."},
			{Name: "persistence", Label: "Persistence", Type: "select", Default: memorystorePersistenceChoice(inst.GetPersistenceConfig()),
				Options: memorystorePersistenceChoices,
				Help:    "RDB snapshots every period (the server's save point), or none; set on the running server."},
		},
		Note: "Saved through UpdateInstance. The ID, region, tier, version and AUTH cannot be changed here.",
	}
	return d, nil
}

// Edit implements console.Editor: UpdateInstance of the form's fields.
func (p memorystoreProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return err
	}
	size, err := strconv.Atoi(strings.TrimSpace(values["memorySizeGb"]))
	if err != nil {
		return fmt.Errorf("capacity %q is not a number of GB", values["memorySizeGb"])
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return err
	}
	configs, err := console.ParseMap(values["redisConfigs"])
	if err != nil {
		return err
	}
	paths := []string{"display_name", "memory_size_gb", "labels", "redis_configs"}
	in := &redispb.Instance{Name: name, DisplayName: values["displayName"], MemorySizeGb: int32(size),
		Labels: labels, RedisConfigs: configs}
	if v, ok := values["persistence"]; ok {
		cur, err := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err != nil {
			return err
		}
		if v != memorystorePersistenceChoice(cur.GetPersistenceConfig()) {
			pc := &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_DISABLED}
			if period, ok := redispb.PersistenceConfig_SnapshotPeriod_value[v]; ok {
				pc = &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB,
					RdbSnapshotPeriod: redispb.PersistenceConfig_SnapshotPeriod(period)}
			} else if v != "DISABLED" {
				return fmt.Errorf("persistence %q is not DISABLED or a snapshot period", v)
			}
			paths, in.PersistenceConfig = append(paths, "persistence_config"), pc
		}
	}
	op, err := p.api().UpdateInstance(ctx, &redispb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, Instance: in})
	if err != nil {
		return err
	}
	return p.wait(ctx, op.GetName())
}

// memorystorePairs is a map as key=value pairs, in key order.
func memorystorePairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + m[k]
	}
	return strings.Join(keys, ", ")
}

// DetailActions implements console.PathActor: Export and Import (#1384)
// on an instance CreateInstance made, as the Memorystore console's
// instance page offers them, to and from a gs:// RDB file in this
// instance's Cloud Storage.
func (p memorystoreProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil || memorystoreID(name) == redisadmin.InstanceID {
		return nil
	}
	var upgrade []console.Action
	if inst, err := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err == nil {
		var later []string
		seen := false
		for _, v := range redisadmin.Versions {
			if seen {
				later = append(later, v)
			}
			seen = seen || v == inst.GetRedisVersion()
		}
		if len(later) > 0 {
			upgrade = []console.Action{{ID: "upgrade", Label: "Upgrade", Confirm: "The instance's server is started again on the new version, with its data; an instance cannot be downgraded.",
				Fields: []console.Field{{Name: "redisVersion", Label: "Version", Type: "select", Required: true, Options: later, Default: later[0]}}}}
		}
	}
	return append(upgrade, []console.Action{
		{ID: "export", Label: "Export", Fields: []console.Field{{Name: "uri", Label: "Destination", Type: "text", Required: true,
			Pattern: "^gs://[^/]+/.+$", Help: "gs://bucket/object: the RDB file ExportInstance writes, in this instance's Cloud Storage."}}},
		{ID: "import", Label: "Import", Destructive: true,
			Confirm: "Every key of the instance is replaced by the RDB file's (ImportInstance overwrites the data).",
			Fields: []console.Field{{Name: "uri", Label: "Source", Type: "text", Required: true,
				Pattern: "^gs://[^/]+/.+$", Help: "gs://bucket/object: an RDB file in this instance's Cloud Storage."}}},
	}...)
}

// ActAt implements console.PathActor: ExportInstance or ImportInstance,
// waited for.
func (p memorystoreProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return err
	}
	uri := strings.TrimSpace(values["uri"])
	var opName string
	switch action {
	case "export":
		op, err := p.api().ExportInstance(ctx, &redispb.ExportInstanceRequest{Name: name,
			OutputConfig: &redispb.OutputConfig{Destination: &redispb.OutputConfig_GcsDestination{GcsDestination: &redispb.GcsDestination{Uri: uri}}}})
		if err != nil {
			return err
		}
		opName = op.GetName()
	case "import":
		op, err := p.api().ImportInstance(ctx, &redispb.ImportInstanceRequest{Name: name,
			InputConfig: &redispb.InputConfig{Source: &redispb.InputConfig_GcsSource{GcsSource: &redispb.GcsSource{Uri: uri}}}})
		if err != nil {
			return err
		}
		opName = op.GetName()
	case "upgrade":
		op, err := p.api().UpgradeInstance(ctx, &redispb.UpgradeInstanceRequest{Name: name, RedisVersion: values["redisVersion"]})
		if err != nil {
			return err
		}
		opName = op.GetName()
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return p.wait(ctx, opName)
}

// memorystorePersistenceChoices are the Edit form's persistence options:
// none, or RDB with one of Memorystore's snapshot periods.
var memorystorePersistenceChoices = []string{"DISABLED", "ONE_HOUR", "SIX_HOURS", "TWELVE_HOURS", "TWENTY_FOUR_HOURS"}

func memorystorePersistenceChoice(pc *redispb.PersistenceConfig) string {
	if pc.GetPersistenceMode() == redispb.PersistenceConfig_RDB {
		return pc.GetRdbSnapshotPeriod().String()
	}
	return "DISABLED"
}

// memorystorePersistence is persistence_config as the page shows it.
func memorystorePersistence(pc *redispb.PersistenceConfig) string {
	if pc.GetPersistenceMode() != redispb.PersistenceConfig_RDB {
		return "Disabled"
	}
	return fmt.Sprintf("RDB every %s, next %s", pc.GetRdbSnapshotPeriod(), pc.GetRdbNextSnapshotTime().AsTime().UTC().Format(time.RFC3339))
}
