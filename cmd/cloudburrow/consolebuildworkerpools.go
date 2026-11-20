package main

// A Cloud Build worker pool's page, Google's Edit private pool (#1854), and the default
// build service account on the Triggers page (#1491): the console parity
// audit found GetWorkerPool, UpdateWorkerPool and GetDefaultServiceAccount
// never sent by the console.

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// poolName is the full name of the project's pool of that ID; an ID in two
// regions is refused, since the page names pools by ID alone.
func (p buildWorkerPoolsProvider) poolName(project, id string) (string, error) {
	var match []string
	for _, w := range runadapter.WorkerPools(p.project(project)) {
		if strings.HasSuffix(w.GetName(), "/workerPools/"+id) {
			match = append(match, w.GetName())
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("worker pool %q not found", id)
	case 1:
		return match[0], nil
	}
	return "", fmt.Errorf("worker pool %q is in %d regions; use gcloud builds worker-pools --region", id, len(match))
}

// get reads a pool through GetWorkerPool.
func (p buildWorkerPoolsProvider) get(ctx context.Context, project, id string) (*cbpb.WorkerPool, error) {
	name, err := p.poolName(project, id)
	if err != nil {
		return nil, err
	}
	var w *cbpb.WorkerPool
	err = p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		var e error
		w, e = c.GetWorkerPool(ctx, &cbpb.GetWorkerPoolRequest{Name: name})
		return e
	})
	return w, err
}

// Detail implements console.Driller: a pool's page, as GetWorkerPool reads it.
func (p buildWorkerPoolsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	w, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	cfg := w.GetPrivatePoolV1Config()
	wc := cfg.GetWorkerConfig()
	props := []console.Property{{Label: "Name", Value: w.GetName()}, {Label: "State", Value: w.GetState().String()},
		{Label: "Display name", Value: w.GetDisplayName()}, {Label: "Machine type", Value: wc.GetMachineType()}}
	if wc.GetDiskSizeGb() != 0 {
		props = append(props, console.Property{Label: "Disk size (GB)", Value: strconv.FormatInt(wc.GetDiskSizeGb(), 10)})
	}
	props = append(props, console.Property{Label: "Network", Value: cfg.GetNetworkConfig().GetPeeredNetwork()},
		console.Property{Label: "Etag", Value: w.GetEtag()}, console.Property{Label: "Created", Value: stampAge(w.GetCreateTime())},
		console.Property{Label: "Updated", Value: stampAge(w.GetUpdateTime())})
	var shown []console.Property
	for _, pr := range props {
		if pr.Value != "" && pr.Value != "-" {
			shown = append(shown, pr)
		}
	}
	return console.Detail{Summary: shown, Edit: editPoolForm(w)}, nil
}

// editPoolForm is Google's Edit private pool page (EditForm.Page, #1854):
// "Click the private pool name. The Edit private pool page is displayed.
// ... update the machine type, disk size, and other settings as needed.
// Click Save." (create-manage-private-pools). It holds Create private
// pool's Machine configuration, Worker release and Assign external IPs,
// prefilled; the name, region and network are fixed once it exists.
func editPoolForm(w *cbpb.WorkerPool) *console.EditForm {
	cfg := w.GetPrivatePoolV1Config()
	wc := cfg.GetWorkerConfig()
	series, _, _ := strings.Cut(wc.GetMachineType(), "-")
	if poolMachineTypes[series] == nil {
		series = "e2"
	}
	disk := ""
	if wc.GetDiskSizeGb() != 0 {
		disk = strconv.FormatInt(wc.GetDiskSizeGb(), 10)
	}
	release, channel, version := "channel", "regular", ""
	switch r := wc.GetWorkerRelease(); r {
	case "", "regular", "stable", "rapid":
		channel = orText(r, "regular")
	default:
		release, version = "version", r
	}
	external := "true"
	if cfg.GetNetworkConfig().GetEgressOption() == cbpb.PrivatePoolV1Config_NetworkConfig_NO_PUBLIC_EGRESS {
		external = "false"
	}
	_, all := buildWorkerPoolsProvider{}.CreateForm()
	var fields []console.Field
	for _, f := range all {
		switch {
		case f.Name == "name" || f.Name == "region" || f.Section == "Network type" && f.Name != "externalIPs":
			continue
		case f.Name == "series":
			f.Default = series
		case strings.HasPrefix(f.Name, "machineType-"):
			if f.Name == "machineType-"+series && wc.GetMachineType() != "" {
				f.Default = wc.GetMachineType()
				if !slices.Contains(f.Options, f.Default) {
					f.Options = append(f.Options, f.Default)
				}
			}
		case f.Name == "diskSizeGb":
			f.Default = disk
		case f.Name == "nestedVirtualization":
			f.Default = fmt.Sprint(wc.GetEnableNestedVirtualization())
		case f.Name == "release":
			f.Default = release
		case f.Name == "channel":
			f.Default = channel
		case f.Name == "version":
			f.Default = version
		case f.Name == "externalIPs":
			f.Default = external
		}
		fields = append(fields, f)
	}
	fields = append(fields, console.Field{Name: "etag", Type: "hidden", Default: w.GetEtag()})
	return &console.EditForm{Label: "Edit", Title: "Edit private pool", Submit: "Save", Page: true, Fields: fields,
		Note: "Name, Region and Network type are fixed once the pool exists."}
}

// Edit implements console.Editor: Edit private pool's Save, an
// UpdateWorkerPool of what the page holds, with the etag it was drawn from.
func (p buildWorkerPoolsProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("only a private pool is edited here")
	}
	name, err := p.poolName(project, path[0])
	if err != nil {
		return err
	}
	if _, set := v["externalIPs"]; !set {
		// An unchecked box is not sent.
		v["externalIPs"] = "false"
	}
	pool, err := poolFromValues(v)
	if err != nil {
		return err
	}
	if pool.GetPrivatePoolV1Config().GetWorkerConfig().GetMachineType() == "" {
		return fmt.Errorf("a machine type is required")
	}
	pool.Name, pool.Etag = name, v["etag"]
	paths := []string{"private_pool_v1_config.worker_config.machine_type", "private_pool_v1_config.worker_config.disk_size_gb",
		"private_pool_v1_config.worker_config.enable_nested_virtualization", "private_pool_v1_config.network_config.egress_option"}
	if v["release"] != "" {
		paths = append(paths, "private_pool_v1_config.worker_config.worker_release")
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.UpdateWorkerPool(ctx, &cbpb.UpdateWorkerPoolRequest{WorkerPool: pool, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// RowDeleteIcon draws each row's Delete as a trash icon: "In the row with
// your private pool, click the trash icon."
func (buildWorkerPoolsProvider) RowDeleteIcon() bool { return true }

// defaultServiceAccount is the Triggers page's summary: the account builds
// run as, as GetDefaultServiceAccount answers it.
func (p buildTriggersProvider) defaultServiceAccount(ctx context.Context, project string) []console.PropertyGroup {
	var sa *cbpb.DefaultServiceAccount
	if err := p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		var e error
		sa, e = c.GetDefaultServiceAccount(ctx, &cbpb.GetDefaultServiceAccountRequest{
			Name: "projects/" + p.project(project) + "/locations/global/defaultServiceAccount"})
		return e
	}); err != nil || sa.GetServiceAccountEmail() == "" {
		return nil
	}
	email := sa.GetServiceAccountEmail()
	return []console.PropertyGroup{{Heading: "Settings", Properties: []console.Property{
		{Label: "Default build service account", Value: email[strings.LastIndex(email, "/")+1:]}}}}
}

var (
	_ console.Driller         = buildWorkerPoolsProvider{}
	_ console.Editor          = buildWorkerPoolsProvider{}
	_ console.RowDeleteIconer = buildWorkerPoolsProvider{}
)
