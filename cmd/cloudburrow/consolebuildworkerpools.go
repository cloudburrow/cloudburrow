package main

// A Cloud Build worker pool's page, with Edit worker pool, and the default
// build service account on the Triggers page (#1491): the console parity
// audit found GetWorkerPool, UpdateWorkerPool and GetDefaultServiceAccount
// never sent by the console.

import (
	"context"
	"fmt"
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
	return console.Detail{Summary: shown}, nil
}

// DetailActions implements console.PathActor: Edit worker pool, prefilled.
func (p buildWorkerPoolsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	w, err := p.get(ctx, project, path[0])
	if err != nil {
		return nil
	}
	wc := w.GetPrivatePoolV1Config().GetWorkerConfig()
	disk := ""
	if wc.GetDiskSizeGb() != 0 {
		disk = strconv.FormatInt(wc.GetDiskSizeGb(), 10)
	}
	return []console.Action{{ID: "edit-pool", Label: "Edit worker pool", Fields: []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Default: w.GetDisplayName()},
		{Name: "machineType", Label: "Machine type", Type: "text", Required: true, Default: wc.GetMachineType(),
			Help: "Such as e2-standard-2; a build that names the pool runs on the host's Docker whatever its machine type"},
		{Name: "diskSizeGb", Label: "Disk size (GB)", Type: "number", Default: disk, Help: "100 to 4000; empty or 0 is the default"},
		{Name: "etag", Type: "hidden", Default: w.GetEtag()},
	}}}
}

// ActAt implements console.PathActor: UpdateWorkerPool of the display name,
// machine type and disk size, with the etag the form was drawn from.
func (p buildWorkerPoolsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 || action != "edit-pool" {
		return fmt.Errorf("unknown action %q", action)
	}
	name, err := p.poolName(project, path[0])
	if err != nil {
		return err
	}
	var disk int64
	if d := strings.TrimSpace(v["diskSizeGb"]); d != "" {
		if disk, err = strconv.ParseInt(d, 10, 64); err != nil || disk < 0 {
			return fmt.Errorf("disk size %q: expected a whole number of GB", d)
		}
	}
	mt := strings.TrimSpace(v["machineType"])
	if mt == "" {
		return fmt.Errorf("a machine type is required")
	}
	w := &cbpb.WorkerPool{Name: name, DisplayName: strings.TrimSpace(v["displayName"]), Etag: v["etag"],
		Config: &cbpb.WorkerPool_PrivatePoolV1Config{PrivatePoolV1Config: &cbpb.PrivatePoolV1Config{
			WorkerConfig: &cbpb.PrivatePoolV1Config_WorkerConfig{MachineType: mt, DiskSizeGb: disk}}}}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.UpdateWorkerPool(ctx, &cbpb.UpdateWorkerPoolRequest{WorkerPool: w, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{
			"display_name", "private_pool_v1_config.worker_config.machine_type", "private_pool_v1_config.worker_config.disk_size_gb"}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

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
	_ console.Driller   = buildWorkerPoolsProvider{}
	_ console.PathActor = buildWorkerPoolsProvider{}
)
