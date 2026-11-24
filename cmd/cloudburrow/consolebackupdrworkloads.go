package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// Backup and DR's AlloyDB clusters and Filestore instances in the console
// (#1524): Schedule backup's resources, a cluster's or instance's page in
// Vaulted backups, and Restore, as Google lays them out
// (https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/alloydb/alloydb-restore,
// https://docs.cloud.google.com/backup-disaster-recovery/docs/cloud-console/filestore/filestore-instance-restore):
// an AlloyDB backup is restored into a new cluster, or a new cluster at a
// point in time, through AlloyDB's clusters.restore; a Filestore backup into
// a new instance, through Filestore's instances.create.

// bdrWorkloadType is whether t is AlloyDB's or Filestore's.
func bdrWorkloadType(t string) bool {
	return t == backupdr.AlloyDBResourceType || t == backupdr.FilestoreResourceType
}

// bdrServedType is whether the console offers t: Cloud SQL, Compute
// Engine's, and AlloyDB's and Filestore's while they are served.
func (p backupDRProvider) bdrServedType(t string) bool {
	if t == backupdr.CloudSQLResourceType || bdrComputeType(t) {
		return true
	}
	_, unserved := p.unservedTypes()[t]
	return bdrWorkloadType(t) && !unserved
}

// unservedTypes are the Resource Type choices drawn disabled, each with
// why: AlloyDB or Filestore when this instance does not serve it.
func (p backupDRProvider) unservedTypes() map[string]string {
	out := map[string]string{}
	for t, svc := range map[string]string{backupdr.AlloyDBResourceType: "alloydb", backupdr.FilestoreResourceType: "filestore"} {
		if p.workloadAPI(t) == nil {
			out[t] = "Not available: " + bdrResourceTypeLabels[t] + " is not served by this instance; start it with " + svc + " in --services (docs/backupdr.md)."
		}
	}
	return out
}

// workloadAPI is AlloyDB's or Filestore's REST API, nil when not served.
func (p backupDRProvider) workloadAPI(t string) http.Handler {
	f := p.alloydb
	if t == backupdr.FilestoreResourceType {
		f = p.filestore
	}
	if f == nil {
		return nil
	}
	return f()
}

// bdrWorkload is one cluster or instance the project has.
type bdrWorkload struct {
	name, location string
	item           map[string]any
}

// workloadResources are the project's AlloyDB clusters or Filestore
// instances, by ID.
func (p backupDRProvider) workloadResources(ctx context.Context, project, typ string) map[string]bdrWorkload {
	out := map[string]bdrWorkload{}
	h := p.workloadAPI(typ)
	if h == nil || project == "" {
		return out
	}
	coll := "clusters"
	if typ == backupdr.FilestoreResourceType {
		coll = "instances"
	}
	res, err := p.call(ctx, h, http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/locations/-/"+coll, nil)
	if err != nil {
		return out
	}
	l, _ := res[coll].([]any)
	for _, e := range l {
		o, _ := e.(map[string]any)
		n := str(o, "name")
		out[lastSegment(n)] = bdrWorkload{name: n, location: locationOf(n), item: o}
	}
	return out
}

// bdrWorkloadOptions are a select's options, "name (location)".
func bdrWorkloadOptions(m map[string]bdrWorkload) ([]string, map[string]string) {
	labels := map[string]string{}
	keys := sortedKeysOf(m)
	for _, n := range keys {
		labels[n] = n + " (" + m[n].location + ")"
	}
	return keys, labels
}

// bdrRegionOf is a location's region: a Filestore zonal instance's zone is
// in its region.
func bdrRegionOf(loc string) string {
	if strings.Count(loc, "-") >= 2 {
		return loc[:strings.LastIndex(loc, "-")]
	}
	return loc
}

// workloadScheduleFields are Schedule backup's AlloyDB and Filestore
// choices, after the region.
func workloadScheduleFields(clusters []string, clusterLabels map[string]string, fs []string, fsLabels map[string]string) []console.Field {
	return []console.Field{
		{Name: "cluster", Label: "Clusters", Type: "select", Section: "Choose resources", Options: clusters, OptionLabels: clusterLabels,
			ShowWhen: "resourceType=" + backupdr.AlloyDBResourceType,
			Help:     "The AlloyDB cluster to back up, in the region chosen. Its own automated and continuous backups are turned off while the plan protects it."},
		{Name: "fsinstance", Label: "Instances", Type: "select", Section: "Choose resources", Options: fs, OptionLabels: fsLabels,
			ShowWhen: "resourceType=" + backupdr.FilestoreResourceType,
			Help:     "The Filestore instance to back up, in the region chosen: Zonal, Regional and Enterprise instances only."},
	}
}

// scheduleWorkload applies the plan to a cluster or an instance.
func (p bdrVaultedProvider) scheduleWorkload(ctx context.Context, project string, v map[string]string) (string, error) {
	typ := v["resourceType"]
	field := "cluster"
	if typ == backupdr.FilestoreResourceType {
		field = "fsinstance"
	}
	id := v[field]
	if id == "" {
		return "", fmt.Errorf("choose the %s to back up", map[bool]string{true: "cluster", false: "instance"}[field == "cluster"])
	}
	w, ok := p.api.workloadResources(ctx, project, typ)[id]
	if !ok {
		return "", fmt.Errorf("%s %s does not exist in project %s", bdrTypeLabel(typ), id, project)
	}
	region := bdrRegionOf(w.location)
	if v["region"] != "" && region != v["region"] {
		return "", fmt.Errorf("%s is in %s, not %s", id, region, v["region"])
	}
	if locationOf(v["plan"]) != region {
		return "", fmt.Errorf("backup plan %s is in %s; choose a plan in %s, the resource's region", lastSegment(v["plan"]), locationOf(v["plan"]), region)
	}
	if plan, err := p.api.get(ctx, v["plan"]); err == nil && str(plan, "resourceType") != typ {
		return "", fmt.Errorf("backup plan %s protects %s, not %s", lastSegment(v["plan"]), bdrTypeLabel(str(plan, "resourceType")), bdrTypeLabel(typ))
	}
	loc := "projects/" + project + "/locations/" + region
	_, err := p.api.call(ctx, p.api.bdr, http.MethodPost, "/v1/"+loc+"/backupPlanAssociations?backupPlanAssociationId="+url.QueryEscape(bdrAssocID(id)),
		map[string]any{"backupPlan": v["plan"], "resourceType": typ, "resource": w.name})
	return id, err
}

// bdrWorkloadSummary are a cluster's or instance's rows on its Vaulted
// backups page.
func bdrWorkloadSummary(s bdrSource) []console.Property {
	g := nested(s.ds, "dataSourceGcpResource")
	if str(g, "type") == backupdr.FilestoreResourceType {
		return []console.Property{{Label: "Instance created", Value: str(nested(g, "filestoreInstanceDatasourceProperties"), "instanceCreateTime")}}
	}
	return []console.Property{{Label: "Cluster UID", Value: str(nested(g, "alloyDbClusterDatasourceProperties"), "clusterUid")},
		{Label: "Point-in-time recovery", Value: bdrPITRText(s)}}
}

// bdrPITRText is a cluster's recovery windows, for its page.
func bdrPITRText(s bdrSource) string {
	l, _ := nested(s.ds, "dataSourceGcpResource", "alloyDbClusterDatasourceProperties")["pitrWindows"].([]any)
	var parts []string
	for _, e := range l {
		w, _ := e.(map[string]any)
		end := str(w, "endTime")
		if end == "" {
			end = "now"
		}
		parts = append(parts, str(w, "startTime")+" to "+end)
	}
	if len(parts) == 0 {
		return "Off: the backup plan keeps no logs"
	}
	return strings.Join(parts, "; ")
}

// workloadRestoreAction is Restore for a cluster's or instance's backup:
// always into a new resource, its properties pre-filled from the source.
func (p bdrVaultedProvider) workloadRestoreAction(ctx context.Context, project string, s bdrSource, backups []string) console.Action {
	typ := s.typ()
	fields := []console.Field{}
	if backups != nil {
		fields = append(fields, console.Field{Name: "backup", Label: "Backup creation time", Type: "select", Required: true, Options: backups,
			Section: "Choose the backup"})
	}
	name := bdrRestoreName(s.instanceName(), time.Now())
	if typ == backupdr.AlloyDBResourceType {
		fields = append(fields,
			console.Field{Name: "project", Label: "Project", Type: "text", Required: true, Section: "Choose the restore destination", Default: project,
				Help: "The project the new cluster is made in."},
			console.Field{Name: "region", Label: "Region", Type: "select", Required: true, Section: "Choose the restore destination",
				Options: backupdr.Regions, OptionLabels: bdrRegionLabels(), Default: s.location(), Help: "The new cluster's region."},
			console.Field{Name: "name", Label: "Cluster ID", Type: "text", Required: true, Section: "Configure your cluster", Default: name,
				Pattern: `[a-z]([a-z0-9\-]{0,61}[a-z0-9])?`, Help: "The new cluster's ID: lowercase letters, digits and hyphens, starting with a letter."},
			console.Field{Name: "network", Label: "Network", Type: "text", Required: true, Section: "Configure your cluster", Default: "default",
				Help: "The VPC network the new cluster's instances are reached on."})
		return console.Action{ID: "restore", Label: "Restore", Submit: "Restore", Steps: "continue", Fields: fields,
			Confirm: "A new cluster is made from the backup; the source is not changed. Create its primary instance on the AlloyDB page to connect to it."}
	}
	tier, gb, share := "ZONAL", "1024", "vol1"
	if in, err := p.api.call(ctx, p.api.workloadAPI(typ), http.MethodGet, "/v1/"+s.instance(), nil); err == nil {
		tier = str(in, "tier")
		if fsl, _ := in["fileShares"].([]any); len(fsl) > 0 {
			f, _ := fsl[0].(map[string]any)
			share = str(f, "name")
			switch c := f["capacityGb"].(type) {
			case string:
				gb = c
			case float64:
				gb = strconv.FormatFloat(c, 'f', 0, 64)
			}
		}
	}
	fields = append(fields,
		console.Field{Name: "project", Label: "Project", Type: "text", Required: true, Section: "Choose the restore destination", Default: project,
			Help: "The project the new instance is made in."},
		console.Field{Name: "location", Label: "Location", Type: "text", Required: true, Section: "Choose the restore destination", Default: s.location(),
			Help: "The new instance's zone (Zonal) or region (Regional, Enterprise)."},
		console.Field{Name: "name", Label: "Instance ID", Type: "text", Required: true, Section: "Configure your instance", Default: name,
			Pattern: `[a-z]([a-z0-9\-]{0,61}[a-z0-9])?`, Help: "The new instance's ID: lowercase letters, digits and hyphens, starting with a letter."},
		console.Field{Name: "tier", Label: "Service tier", Type: "select", Required: true, Section: "Configure your instance",
			Options: []string{"ZONAL", "REGIONAL", "ENTERPRISE"}, Default: tier,
			OptionLabels: map[string]string{"ZONAL": "Zonal", "REGIONAL": "Regional", "ENTERPRISE": "Enterprise"},
			Help:         "A Zonal, Regional or Enterprise instance: a basic tier cannot hold an enhanced backup."},
		console.Field{Name: "capacity", Label: "Capacity (GiB)", Type: "text", Required: true, Section: "Configure your instance", Default: gb,
			Pattern: `[1-9][0-9]*`, Help: "At least the source instance's capacity."},
		console.Field{Name: "share", Label: "File share name", Type: "text", Required: true, Section: "Configure your instance", Default: share},
		console.Field{Name: "network", Label: "VPC network", Type: "text", Required: true, Section: "Configure your instance", Default: "default"})
	return console.Action{ID: "restore", Label: "Restore", Submit: "Restore", Steps: "continue", Fields: fields,
		Confirm: "A new instance is made from the backup; the source is not changed."}
}

// workloadPITRAction is an AlloyDB cluster's point-in-time restore, into a
// new cluster, from its data source's logs.
func workloadPITRAction(project string, s bdrSource) console.Action {
	return console.Action{ID: "pitr", Label: "Point-in-time restore", Submit: "Restore", Fields: []console.Field{
		{Name: "time", Label: "Point in time", Type: "text", Required: true,
			Help: "An RFC 3339 time inside a recovery window (" + bdrPITRText(s) + "), such as 2026-10-16T12:00:00Z."},
		{Name: "project", Label: "Project", Type: "text", Required: true, Default: project},
		{Name: "region", Label: "Region", Type: "select", Required: true, Options: backupdr.Regions, OptionLabels: bdrRegionLabels(), Default: s.location()},
		{Name: "name", Label: "Cluster ID", Type: "text", Required: true, Default: bdrRestoreName(s.instanceName(), time.Now()),
			Pattern: `[a-z]([a-z0-9\-]{0,61}[a-z0-9])?`},
		{Name: "network", Label: "Network", Type: "text", Required: true, Default: "default"}},
		Confirm: "A new cluster is made at the point in time; the source is not changed."}
}

// restoreWorkload restores a cluster's or instance's backup, or a cluster
// at a point in time (pitr), into a new resource.
func (p bdrVaultedProvider) restoreWorkload(ctx context.Context, s bdrSource, v map[string]string, pitr bool) error {
	if !pitr && v["backup"] == "" {
		return errors.New("choose the backup to restore")
	}
	if v["name"] == "" || v["project"] == "" {
		return errors.New("give the restore project and the new resource's ID")
	}
	h := p.api.workloadAPI(s.typ())
	if h == nil {
		return fmt.Errorf("%s is not served by this instance", bdrTypeLabel(s.typ()))
	}
	backup := str(s.ds, "name") + "/backups/" + v["backup"]
	if s.typ() == backupdr.AlloyDBResourceType {
		if v["region"] == "" || v["network"] == "" {
			return errors.New("give the new cluster's region and network")
		}
		network := v["network"]
		if !strings.Contains(network, "/") {
			network = "projects/" + v["project"] + "/global/networks/" + network
		}
		body := map[string]any{"clusterId": v["name"], "cluster": map[string]any{"networkConfig": map[string]any{"network": network}}}
		if pitr {
			if _, err := time.Parse(time.RFC3339Nano, v["time"]); err != nil {
				return errors.New("the point in time is an RFC 3339 time, such as 2026-10-16T12:00:00Z")
			}
			body["backupdrPitrSource"] = map[string]any{"dataSource": str(s.ds, "name"), "pointInTime": v["time"]}
		} else {
			body["backupdrBackupSource"] = map[string]any{"backup": backup}
		}
		_, err := p.api.call(ctx, h, http.MethodPost, "/v1/projects/"+url.PathEscape(v["project"])+"/locations/"+url.PathEscape(v["region"])+"/clusters:restore", body)
		return err
	}
	if v["location"] == "" {
		return errors.New("give the new instance's location")
	}
	gb, err := strconv.ParseInt(v["capacity"], 10, 64)
	if err != nil || gb < 1 {
		return errors.New("the capacity is a number of GiB")
	}
	share := v["share"]
	if share == "" {
		share = "vol1"
	}
	network := v["network"]
	if network == "" {
		network = "default"
	}
	body := map[string]any{"tier": v["tier"],
		"fileShares": []any{map[string]any{"name": share, "capacityGb": strconv.FormatInt(gb, 10), "sourceBackupdrBackup": backup}},
		"networks":   []any{map[string]any{"network": network}}}
	_, err = p.api.call(ctx, h, http.MethodPost, "/v1/projects/"+url.PathEscape(v["project"])+"/locations/"+url.PathEscape(v["location"])+
		"/instances?instanceId="+url.QueryEscape(v["name"]), body)
	return err
}
