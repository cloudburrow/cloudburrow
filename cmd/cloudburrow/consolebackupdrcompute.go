package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// Backup and DR's Compute Engine VMs and disks in the console (#2075):
// Schedule backup's resources, a VM's or disk's page in Vaulted backups,
// and Restore, as Google lays them out
// (https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/compute/compute-instance-restore,
// https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/compute/disk-restore).

// bdrComputeType is whether t is one of Compute Engine's types.
func bdrComputeType(t string) bool {
	return t == backupdr.ComputeInstanceResourceType || t == backupdr.ComputeDiskResourceType
}

// computeAPI is the Compute Engine API, or an error when it is not served.
func (p backupDRProvider) computeAPI() (*compute.Server, error) {
	if p.compute != nil {
		if api := p.compute(); api != nil {
			return api, nil
		}
	}
	return nil, errors.New("Compute Engine is not served by this instance: start it with compute in --services")
}

// computeResources are the project's VMs (collection "instances") or disks
// ("disks"), by name, each with its zone.
func (p backupDRProvider) computeResources(project, collection string) map[string]string {
	out := map[string]string{}
	api, err := p.computeAPI()
	if err != nil || project == "" {
		return out
	}
	res, err := api.Call(http.MethodGet, project, "aggregated/"+collection, nil, nil)
	if err != nil {
		return out
	}
	items, _ := res["items"].(map[string]any)
	for _, scoped := range items {
		m, _ := scoped.(map[string]any)
		l, _ := m[collection].([]any)
		for _, e := range l {
			o, _ := e.(map[string]any)
			out[str(o, "name")] = lastSegment(str(o, "zone"))
		}
	}
	return out
}

// bdrResourceOptions are a select's options, "name (zone)".
func bdrResourceOptions(m map[string]string) ([]string, map[string]string) {
	labels := map[string]string{}
	keys := sortedKeysOf(m)
	for _, n := range keys {
		labels[n] = n + " (" + m[n] + ")"
	}
	return keys, labels
}

// scheduleCompute applies the plan to a VM or disk.
func (p bdrVaultedProvider) scheduleCompute(ctx context.Context, project string, v map[string]string) (string, error) {
	typ := v["resourceType"]
	coll, field := "instances", "vm"
	if typ == backupdr.ComputeDiskResourceType {
		coll, field = "disks", "disk"
	}
	name := v[field]
	if name == "" {
		return "", fmt.Errorf("choose the %s to back up", bdrTypeLabel(typ))
	}
	zone, ok := p.api.computeResources(project, coll)[name]
	if !ok {
		return "", fmt.Errorf("%s %s does not exist in project %s", bdrTypeLabel(typ), name, project)
	}
	region := zone[:strings.LastIndex(zone, "-")]
	if v["region"] != "" && region != v["region"] {
		return "", fmt.Errorf("%s is in %s, not %s", name, region, v["region"])
	}
	if locationOf(v["plan"]) != region {
		return "", fmt.Errorf("backup plan %s is in %s; choose a plan in %s, the resource's region", lastSegment(v["plan"]), locationOf(v["plan"]), region)
	}
	if plan, err := p.api.get(ctx, v["plan"]); err == nil && str(plan, "resourceType") != typ {
		return "", fmt.Errorf("backup plan %s protects %s, not %s", lastSegment(v["plan"]), bdrTypeLabel(str(plan, "resourceType")), bdrTypeLabel(typ))
	}
	loc := "projects/" + project + "/locations/" + region
	_, err := p.api.call(ctx, p.api.bdr, http.MethodPost, "/v1/"+loc+"/backupPlanAssociations?backupPlanAssociationId="+bdrAssocID(name),
		map[string]any{"backupPlan": v["plan"], "resourceType": typ, "resource": "projects/" + project + "/zones/" + zone + "/" + coll + "/" + name})
	return name, err
}

// bdrComputeSummary are a VM's or disk's rows on its Vaulted backups page.
func bdrComputeSummary(s bdrSource) []console.Property {
	g := nested(s.ds, "dataSourceGcpResource")
	if str(g, "type") == backupdr.ComputeDiskResourceType {
		d := nested(g, "diskDatasourceProperties")
		return []console.Property{{Label: "Disk type", Value: lastSegment(str(d, "type"))}, {Label: "Size", Value: str(d, "sizeGb") + " GB"}}
	}
	i := nested(g, "computeInstanceDatasourceProperties")
	return []console.Property{{Label: "Machine type", Value: lastSegment(str(i, "machineType"))},
		{Label: "Disks", Value: str(i, "totalDiskCount") + " (" + str(i, "totalDiskSizeGb") + " GB)"}}
}

// bdrRestoreName is the restored resource's default name: the source's
// "followed by a time and date stamp" (disk-restore), "-restore-" and the
// time, within 63 characters.
func bdrRestoreName(source string, now time.Time) string {
	sfx := "-restore-" + now.UTC().Format("20060102150405")
	if len(source)+len(sfx) > 63 {
		source = strings.TrimRight(source[:63-len(sfx)], "-")
	}
	return source + sfx
}

// computeRestoreAction is Restore for a VM's or disk's backup: Restore
// scope (the whole VM, or one of its disks), the restore project, then the
// new resource's properties, pre-filled from the backup.
func (p bdrVaultedProvider) computeRestoreAction(ctx context.Context, project string, s bdrSource, backups []string) console.Action {
	typ := str(nested(s.ds, "dataSourceGcpResource"), "type")
	zones := computeZonesOf(s.location())
	fields := []console.Field{}
	if backups != nil {
		fields = append(fields, console.Field{Name: "backup", Label: "Backup creation time", Type: "select", Required: true, Options: backups,
			Section: "Choose the backup"})
	}
	name := bdrRestoreName(s.instanceName(), time.Now())
	if typ == backupdr.ComputeInstanceResourceType {
		var devices []string
		if out, err := p.api.get(ctx, str(s.ds, "name")+"/backups"); err == nil {
			seen := map[string]bool{}
			for _, b := range bdrItems(out, "backups") {
				l, _ := nested(b, "computeInstanceBackupProperties")["disk"].([]any)
				for _, e := range l {
					if dev := str(e.(map[string]any), "deviceName"); dev != "" && !seen[dev] {
						seen[dev] = true
						devices = append(devices, dev)
					}
				}
			}
		}
		sort.Strings(devices)
		fields = append(fields,
			console.Field{Name: "scope", Label: "Restore scope", Type: "radio", Required: true, Section: "Restore scope",
				Options: []string{"instance", "disk"}, Default: "instance",
				OptionLabels: map[string]string{"instance": "Entire instance", "disk": "Individual disk"}},
			console.Field{Name: "device", Label: "Disk", Type: "select", Section: "Restore scope", Options: devices, ShowWhen: "scope=disk",
				Help: "The disk of the backup to restore, by its device name, to a new disk."})
	}
	fields = append(fields,
		console.Field{Name: "project", Label: "Restore project name", Type: "text", Required: true, Section: "Choose the restore destination",
			Default: project, Help: "The project the new resource is made in."},
		console.Field{Name: "zone", Label: "Zone", Type: "select", Required: true, Section: "Choose the restore destination", Options: zones,
			Default: s.location(), Help: "The zone of the new resource."},
		console.Field{Name: "name", Label: "Name", Type: "text", Required: true, Section: "Properties", Default: name,
			Pattern: `[a-z]([-a-z0-9]{0,61}[a-z0-9])?`, Help: "The new resource's name: lowercase letters, digits and hyphens, starting with a letter."})
	if typ == backupdr.ComputeInstanceResourceType {
		mts, _ := computeMachineTypeOptions()
		fields = append(fields, console.Field{Name: "machineType", Label: "Machine type", Type: "select", Section: "Properties",
			Options: mts, Default: lastSegment(str(nested(s.ds, "dataSourceGcpResource", "computeInstanceDatasourceProperties"), "machineType")),
			ShowWhen: "scope=instance", Help: "Pre-filled with the backed up instance's."})
	}
	return console.Action{ID: "restore", Label: "Restore", Submit: "Restore", Steps: "continue", Fields: fields,
		Confirm: "A new " + strings.ToLower(bdrTypeLabel(typ)) + " is made from the backup; the source is not changed."}
}

// computeZonesOf are every served zone, zone's own first: a restore may
// target any zone.
func computeZonesOf(zone string) []string {
	out := []string{}
	if zone != "" {
		out = append(out, zone)
	}
	for _, r := range compute.Regions() {
		for _, z := range compute.Zones(r) {
			if z != zone {
				out = append(out, z)
			}
		}
	}
	return out
}

// computeMachineTypeOptions are the served machine types' names.
func computeMachineTypeOptions() ([]string, map[string]string) {
	var out []string
	for _, m := range compute.MachineTypes {
		out = append(out, m.Name)
	}
	return out, nil
}

// restoreCompute sends backups.restore for a VM's or disk's backup.
func (p bdrVaultedProvider) restoreCompute(ctx context.Context, s bdrSource, v map[string]string) error {
	if v["backup"] == "" {
		return errors.New("choose the backup to restore")
	}
	if v["name"] == "" || v["zone"] == "" || v["project"] == "" {
		return errors.New("give the restore project, the zone and the new resource's name")
	}
	typ := str(nested(s.ds, "dataSourceGcpResource"), "type")
	var body map[string]any
	switch {
	case typ == backupdr.ComputeInstanceResourceType && v["scope"] != "disk":
		props := map[string]any{"name": v["name"]}
		if v["machineType"] != "" {
			props["machineType"] = "zones/" + v["zone"] + "/machineTypes/" + v["machineType"]
		}
		body = map[string]any{"computeInstanceTargetEnvironment": map[string]any{"project": v["project"], "zone": v["zone"]},
			"computeInstanceRestoreProperties": props}
	case typ == backupdr.ComputeInstanceResourceType:
		if v["device"] == "" {
			return errors.New("choose the disk to restore")
		}
		body = map[string]any{"diskTargetEnvironment": map[string]any{"project": v["project"], "zone": v["zone"]},
			"diskRestoreProperties": map[string]any{"name": v["name"], "instanceBackupSource": map[string]any{"sourceDeviceName": v["device"]}}}
	default:
		body = map[string]any{"diskTargetEnvironment": map[string]any{"project": v["project"], "zone": v["zone"]},
			"diskRestoreProperties": map[string]any{"name": v["name"]}}
	}
	_, err := p.api.call(ctx, p.api.bdr, http.MethodPost, "/v1/"+str(s.ds, "name")+"/backups/"+v["backup"]+":restore", body)
	return err
}
