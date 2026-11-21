package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/apphub"
)

// App Hub's discovery of the other supported resource types this instance
// serves (#2110, round 3), each read through its product's own JSON API on
// this instance, as an SDK lists it, with no bearer token: an unauthenticated
// in-process call is the instance's own account, so discovery sees every
// resource under --iam-enforcement too, as Google's App Hub service agent
// does. Each type's URI is the one supported-resources writes, with its
// project ID or number as it writes it.
// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/supported-resources.
//
// Supported types this instance does not serve are not discovered: Compute
// Engine forwarding rules, backend services, service attachments and
// instance groups (Compute Engine serves VPC networking, VMs and disks
// only; VM instances are not a supported type), AlloyDB, Memorystore for
// Memcached, Cloud Build connections and installations, Vertex AI's
// datasets, featurestores, metadata stores, notebooks, tensorboards and
// jobs, and the rest of the list. Cloud Scheduler jobs are supported but
// supported-resources gives them no URI, so they are not discovered either.

// appHubDiscoverTimeout bounds one product's answer.
const appHubDiscoverTimeout = 15 * time.Second

// hubItem is one listed resource, as its JSON API returns it.
type hubItem map[string]any

func (it hubItem) str(k string) string {
	s, _ := it[k].(string)
	return s
}

// hubLocation is a resource's location as App Hub places it: a region or
// a multi-region (us, eu) App Hub serves, lowercase; anything else (a
// Spanner or Firestore multi-region such as nam5, a dual-region bucket) is
// global, where a global application can hold it (UNVERIFIED:
// supported-resources does not say where each type is discovered).
// Docs re-read 2026-11-21: https://docs.cloud.google.com/app-hub/docs/locations.
func hubLocation(loc string) string {
	loc = strings.ToLower(loc)
	if loc == "us" || loc == "eu" {
		return loc
	}
	for _, r := range apphub.Regions {
		if r == loc {
			return loc
		}
	}
	return "global"
}

// hubRegionOfZone is a zone's region (us-central1-a is us-central1).
func hubRegionOfZone(zone string) string {
	if strings.Count(zone, "-") == 2 {
		return zone[:strings.LastIndex(zone, "-")]
	}
	return zone
}

// nameSegment is the value after key in a resource name
// (projects/P/locations/L/...: "locations" is L), "" when absent.
func nameSegment(name, key string) string {
	p := strings.Split(name, "/")
	for i := 0; i+1 < len(p); i++ {
		if p[i] == key {
			return p[i+1]
		}
	}
	return ""
}

// withProject is name with its project segment replaced by ref (the
// project ID or number supported-resources writes in the URI).
func withProject(name, ref string) string {
	p := strings.Split(name, "/")
	if len(p) > 1 && p[0] == "projects" {
		p[1] = ref
	}
	return strings.Join(p, "/")
}

// hubLister lists a product's resources through its JSON API.
type hubLister struct {
	client *http.Client
}

// list GETs base+path, following nextPageToken, and returns field's items.
// A product that is not served or does not answer gives none: discovery
// shows what each product reports, and one product's absence never hides
// another's resources.
func (l hubLister) list(ctx context.Context, base, path, field string) []hubItem {
	out, _ := l.listOK(ctx, base, path, field)
	return out
}

// listOK is list, and whether the product answered every page.
func (l hubLister) listOK(ctx context.Context, base, path, field string) ([]hubItem, bool) {
	if base == "" {
		return nil, false
	}
	var out []hubItem
	token := ""
	for range 100 {
		u := base + path
		if token != "" {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + "pageToken=" + url.QueryEscape(token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return out, false
		}
		resp, err := l.client.Do(req)
		if err != nil {
			return out, false
		}
		var page map[string]json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			return out, false
		}
		var items []hubItem
		if raw, ok := page[field]; ok {
			_ = json.Unmarshal(raw, &items)
		}
		out = append(out, items...)
		var next string
		if raw, ok := page["nextPageToken"]; ok {
			_ = json.Unmarshal(raw, &next)
		}
		if next == "" || next == token {
			return out, true
		}
		token = next
	}
	return out, true
}

// appHubOtherResources are project's discovered resources of the types
// beyond Cloud Run and Kubernetes Engine.
func (s *appHubService) appHubOtherResources(ctx context.Context, project string) []apphub.Resource {
	ctx, cancel := context.WithTimeout(ctx, appHubDiscoverTimeout)
	defer cancel()
	number := project
	if s.registry != nil {
		if _, n, ok := (appHubProjects{registry: s.registry}).Project(project); ok && n != 0 {
			number = strconv.FormatInt(n, 10)
		}
	}
	base := func(string) string { return "" }
	if s.live != nil {
		base = localAPIs(s.cfg, s.live)
	}
	l := hubLister{client: &http.Client{Timeout: appHubDiscoverTimeout}}
	p := url.PathEscape(project)
	var out []apphub.Resource
	add := func(uri, loc, zone string, workload bool) {
		out = append(out, apphub.Resource{URI: uri, Project: project, Location: loc, Zone: zone, Workload: workload})
	}

	// Pub/Sub topics and subscriptions: global, by project ID.
	ps := base("pubsub")
	for _, it := range l.list(ctx, ps, "/v1/projects/"+p+"/topics", "topics") {
		add("//pubsub.googleapis.com/"+withProject(it.str("name"), project), "global", "", false)
	}
	for _, it := range l.list(ctx, ps, "/v1/projects/"+p+"/subscriptions", "subscriptions") {
		add("//pubsub.googleapis.com/"+withProject(it.str("name"), project), "global", "", false)
	}

	// Cloud Storage buckets: //storage.googleapis.com/BUCKET, in the
	// bucket's location.
	for _, it := range l.list(ctx, base("storage"), "/storage/v1/b?project="+url.QueryEscape(project), "items") {
		add("//storage.googleapis.com/"+it.str("name"), hubLocation(it.str("location")), "", false)
	}

	// BigQuery datasets, through the forwarded API.
	if bq := fwdAddr(s.bigquery); bq != "" {
		for _, it := range l.list(ctx, "http://"+bq, "/bigquery/v2/projects/"+p+"/datasets?all=true", "datasets") {
			ref, _ := it["datasetReference"].(map[string]any)
			id, _ := ref["datasetId"].(string)
			if id == "" {
				continue
			}
			add("//bigquery.googleapis.com/projects/"+project+"/datasets/"+id, hubLocation(it.str("location")), "", false)
		}
	}

	// Bigtable instances, their clusters, logical views, tables and
	// tables' authorized views, by project ID, on bigtable.googleapis.com.
	bt := base("bigtableadmin")
	for _, inst := range l.list(ctx, bt, "/v2/projects/"+p+"/instances", "instances") {
		iname := withProject(inst.str("name"), project)
		add("//bigtable.googleapis.com/"+iname, "global", "", false)
		for _, c := range l.list(ctx, bt, "/v2/"+inst.str("name")+"/clusters", "clusters") {
			zone := nameSegment(c.str("location"), "locations")
			add("//bigtable.googleapis.com/"+withProject(c.str("name"), project), hubLocation(hubRegionOfZone(zone)), zone, false)
		}
		for _, v := range l.list(ctx, bt, "/v2/"+inst.str("name")+"/logicalViews", "logicalViews") {
			add("//bigtable.googleapis.com/"+withProject(v.str("name"), project), "global", "", false)
		}
		for _, t := range l.list(ctx, bt, "/v2/"+inst.str("name")+"/tables?view=NAME_ONLY", "tables") {
			add("//bigtable.googleapis.com/"+withProject(t.str("name"), project), "global", "", false)
			for _, v := range l.list(ctx, bt, "/v2/"+t.str("name")+"/authorizedViews?view=NAME_ONLY", "authorizedViews") {
				add("//bigtable.googleapis.com/"+withProject(v.str("name"), project), "global", "", false)
			}
		}
	}

	// Spanner instances and databases, by project ID; a regional
	// configuration's instance is in its region.
	sp := base("spanner")
	for _, inst := range l.list(ctx, sp, "/v1/projects/"+p+"/instances", "instances") {
		loc := "global"
		if cfgName := nameSegment(inst.str("config"), "instanceConfigs"); strings.HasPrefix(cfgName, "regional-") {
			loc = hubLocation(strings.TrimPrefix(cfgName, "regional-"))
		}
		add("//spanner.googleapis.com/"+withProject(inst.str("name"), project), loc, "", false)
		for _, d := range l.list(ctx, sp, "/v1/"+inst.str("name")+"/databases", "databases") {
			add("//spanner.googleapis.com/"+withProject(d.str("name"), project), loc, "", false)
		}
	}

	// Firestore databases, by project number.
	for _, it := range l.list(ctx, base("firestore"), "/v1/projects/"+p+"/databases", "databases") {
		add("//firestore.googleapis.com/"+withProject(it.str("name"), number), hubLocation(it.str("locationId")), "", false)
	}

	// Cloud SQL instances: //cloudsql.googleapis.com/projects/P/instances/I.
	for _, it := range l.list(ctx, base("sql"), "/v1/projects/"+p+"/instances", "items") {
		add("//cloudsql.googleapis.com/projects/"+project+"/instances/"+it.str("name"), hubLocation(it.str("region")), "", false)
	}

	// Memorystore: Redis instances and clusters, Valkey instances.
	located := func(host, b, path, field, ref string) {
		for _, it := range l.list(ctx, b, path, field) {
			name := it.str("name")
			add("//"+host+"/"+withProject(name, ref), hubLocation(nameSegment(name, "locations")), "", false)
		}
	}
	rd := base("redis")
	located("redis.googleapis.com", rd, "/v1/projects/"+p+"/locations/-/instances", "instances", project)
	located("redis.googleapis.com", rd, "/v1/projects/"+p+"/locations/-/clusters", "clusters", project)
	located("memorystore.googleapis.com", base("memorystore"), "/memorystore/v1/projects/"+p+"/locations/-/instances", "instances", project)

	// Secret Manager secrets, by project number: global ones, and the
	// regional ones of each location it lists.
	sm := base("secretmanager")
	for _, it := range l.list(ctx, sm, "/v1/projects/"+p+"/secrets", "secrets") {
		add("//secretmanager.googleapis.com/"+withProject(it.str("name"), number), "global", "", false)
	}
	for _, loc := range l.list(ctx, sm, "/v1/projects/"+p+"/locations", "locations") {
		id := loc.str("locationId")
		if id == "" || id == "global" {
			continue
		}
		located("secretmanager.googleapis.com", sm, "/v1/projects/"+p+"/locations/"+url.PathEscape(id)+"/secrets", "secrets", number)
	}

	// Cloud Tasks queues (project number), Workflows, Cloud Run functions
	// (both generations), Eventarc triggers and channels, log buckets
	// (project number), and Vertex AI endpoints and models (project
	// number), in each location.
	// Every location at once ("-") where the product takes it, else
	// each location it lists.
	perLocation := func(host, b, ver, collection, field, ref string, workload bool) {
		if b == "" {
			return
		}
		items, ok := l.listOK(ctx, b, ver+"projects/"+p+"/locations/-/"+collection, field)
		if !ok {
			// Each location the product lists, or, for one that lists
			// none (Cloud Tasks), each App Hub region.
			var locs []string
			for _, x := range l.list(ctx, b, ver+"projects/"+p+"/locations", "locations") {
				if id := x.str("locationId"); id != "" {
					locs = append(locs, id)
				}
			}
			if len(locs) == 0 {
				locs = apphub.Regions
			}
			items = nil
			for _, id := range locs {
				items = append(items, l.list(ctx, b, ver+"projects/"+p+"/locations/"+url.PathEscape(id)+"/"+collection, field)...)
			}
		}
		seen := map[string]bool{}
		for _, it := range items {
			name := it.str("name")
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			add("//"+host+"/"+withProject(name, ref), hubLocation(nameSegment(name, "locations")), "", workload)
		}
	}
	perLocation("cloudtasks.googleapis.com", base("cloudtasks"), "/v2/", "queues", "queues", number, false)
	perLocation("workflows.googleapis.com", base("workflows"), "/v1/", "workflows", "workflows", project, false)
	cf := base("cloudfunctions")
	perLocation("cloudfunctions.googleapis.com", cf, "/v2/", "functions", "functions", project, false)
	perLocation("cloudfunctions.googleapis.com", cf, "/v1/", "functions", "functions", project, false)
	ea := base("eventarc")
	if ea != "" {
		ea += "/eventarc"
	}
	perLocation("eventarc.googleapis.com", ea, "/v1/", "triggers", "triggers", project, false)
	perLocation("eventarc.googleapis.com", ea, "/v1/", "channels", "channels", project, false)
	located("logging.googleapis.com", base("logging"), "/v2/projects/"+p+"/locations/-/buckets", "buckets", number)
	ai := base("aiplatform")
	perLocation("aiplatform.googleapis.com", ai, "/v1/", "endpoints", "endpoints", number, false)
	perLocation("aiplatform.googleapis.com", ai, "/v1/", "models", "models", number, false)

	// Cloud Build private pools: workloads, by project number.
	perLocation("cloudbuild.googleapis.com", base("cloudbuild"), "/v1/", "workerPools", "workerPools", number, true)

	// Service Usage: the project's enabled services, by project number.
	for _, it := range l.list(ctx, base("serviceusage"), "/v1/projects/"+p+"/services?filter=state:ENABLED", "services") {
		svc := nameSegment(it.str("name"), "services")
		if svc != "" {
			add(fmt.Sprintf("//serviceusage.googleapis.com/projects/%s/services/%s", number, svc), "global", "", false)
		}
	}
	return out
}
