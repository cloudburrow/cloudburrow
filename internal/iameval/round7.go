package iameval

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The seventh round's tables (#1879): Certificate Authority Service,
// Kubernetes Engine, Compute Engine (the methods served), API Keys and
// Service Usage, each method with the permission Google's reference names
// for it (read 2026-11-20), and the Cloud Run port's UploadSource and
// Operations and Locations mixins. Every permission is in Google's
// permission index (TestRoundSevenTables).

func init() {
	for name, svc := range map[string]*Service{
		"google.cloud.security.privateca.v1.CertificateAuthorityService": PrivateCA,
		"google.container.v1.ClusterManager":                             GKE,
		"google.cloud.run.v2.SourceUpload":                               Run,
	} {
		Services[name] = svc
	}
	Run.Methods["/google.cloud.run.v2.SourceUpload/UploadSource"] = Method{Permission: RunUploadSource, Field: "parent"}
}

// RunUploadSource is UploadSource's permission, "on the specified resource
// parent" (https://docs.cloud.google.com/run/docs/reference/rest/v2/projects.locations.sourceUploads/upload,
// read 2026-11-20), which its media path checks too.
const RunUploadSource = "run.locations.uploadSource"

// PrivateCA is Certificate Authority Service's table: the permission and
// request field its RPC reference names for each method ("Authorization
// requires the following IAM permission on the specified resource F",
// https://docs.cloud.google.com/certificate-authority-service/docs/reference/rpc/google.cloud.security.privateca.v1,
// and "Certificate Authority Service audit logging",
// https://docs.cloud.google.com/certificate-authority-service/docs/audit-logging,
// read 2026-11-20); the IAM, Operations and Locations mixins' as "IAM
// permissions and roles" lists them
// (https://docs.cloud.google.com/certificate-authority-service/docs/reference/permissions-and-roles).
// CreateCertificate needs "one or more of" privateca.certificates.create
// and privateca.certificates.createForSelf: privateca.certificates.create
// is checked (createForSelf, which issues only for the caller's own
// identity, is not accepted in its place: UNVERIFIED). "Permission
// privateca.certificateTemplates.use is only needed when ...
// CreateCertificate is called with a certificate template being specified
// in the request": it is checked on that template (which resource Google
// checks it on is UNVERIFIED). CA pools, certificate templates and
// certificate revocation lists keep their own policies; a pool's applies
// to its CAs, certificates and CRLs. TestIamPermissions needs no
// permission and answers what the caller holds. Google's wording of a
// denial is UNVERIFIED (re-read 2026-11-20: the troubleshooting page gives
// only "Read access to project PROJECT_NAME was denied"): the newer APIs'.
var PrivateCA = &Service{
	Name:              "privateca.googleapis.com",
	PolicyCollections: []string{"caPools", "certificateTemplates", "certificateRevocationLists"},
	Types: map[string]string{"caPools": "privateca.googleapis.com/CaPool", "certificateAuthorities": "privateca.googleapis.com/CertificateAuthority",
		"certificates": "privateca.googleapis.com/Certificate", "certificateRevocationLists": "privateca.googleapis.com/CertificateRevocationList",
		"certificateTemplates": "privateca.googleapis.com/CertificateTemplate"},
	Methods: func() map[string]Method {
		m := rows(map[string]map[string][2]string{
			"/google.cloud.security.privateca.v1.CertificateAuthorityService/": {
				"CreateCertificate":               {"privateca.certificates.create", "parent"},
				"GetCertificate":                  {"privateca.certificates.get", "name"},
				"ListCertificates":                {"privateca.certificates.list", "parent"},
				"RevokeCertificate":               {"privateca.certificates.update", "name"},
				"UpdateCertificate":               {"privateca.certificates.update", "certificate.name"},
				"ActivateCertificateAuthority":    {"privateca.certificateAuthorities.update", "name"},
				"CreateCertificateAuthority":      {"privateca.certificateAuthorities.create", "parent"},
				"DisableCertificateAuthority":     {"privateca.certificateAuthorities.update", "name"},
				"EnableCertificateAuthority":      {"privateca.certificateAuthorities.update", "name"},
				"FetchCertificateAuthorityCsr":    {"privateca.certificateAuthorities.get", "name"},
				"GetCertificateAuthority":         {"privateca.certificateAuthorities.get", "name"},
				"ListCertificateAuthorities":      {"privateca.certificateAuthorities.list", "parent"},
				"UndeleteCertificateAuthority":    {"privateca.certificateAuthorities.update", "name"},
				"DeleteCertificateAuthority":      {"privateca.certificateAuthorities.delete", "name"},
				"UpdateCertificateAuthority":      {"privateca.certificateAuthorities.update", "certificate_authority.name"},
				"CreateCaPool":                    {"privateca.caPools.create", "parent"},
				"UpdateCaPool":                    {"privateca.caPools.update", "ca_pool.name"},
				"GetCaPool":                       {"privateca.caPools.get", "name"},
				"ListCaPools":                     {"privateca.caPools.list", "parent"},
				"DeleteCaPool":                    {"privateca.caPools.delete", "name"},
				"FetchCaCerts":                    {"privateca.caPools.get", "ca_pool"},
				"GetCertificateRevocationList":    {"privateca.certificateRevocationLists.get", "name"},
				"ListCertificateRevocationLists":  {"privateca.certificateRevocationLists.list", "parent"},
				"UpdateCertificateRevocationList": {"privateca.certificateRevocationLists.update", "certificate_revocation_list.name"},
				"CreateCertificateTemplate":       {"privateca.certificateTemplates.create", "parent"},
				"DeleteCertificateTemplate":       {"privateca.certificateTemplates.delete", "name"},
				"GetCertificateTemplate":          {"privateca.certificateTemplates.get", "name"},
				"ListCertificateTemplates":        {"privateca.certificateTemplates.list", "parent"},
				"UpdateCertificateTemplate":       {"privateca.certificateTemplates.update", "certificate_template.name"},
			},
			"/google.longrunning.Operations/": {
				"GetOperation":    {"privateca.operations.get", "name"},
				"ListOperations":  {"privateca.operations.list", "name"},
				"CancelOperation": {"privateca.operations.cancel", "name"},
				"DeleteOperation": {"privateca.operations.delete", "name"},
			},
			"/google.cloud.location.Locations/": {
				"GetLocation":   {"privateca.locations.get", "name"},
				"ListLocations": {"privateca.locations.list", "name"},
			},
		})
		row := m["/google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate"]
		row.Also = []Also{{Field: "certificate.certificate_template", Permission: "privateca.certificateTemplates.use"}}
		m["/google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate"] = row
		for _, verb := range []string{"GetIamPolicy", "SetIamPolicy"} {
			suffix := "." + strings.ToLower(verb[:1]) + verb[1:]
			m["/google.iam.v1.IAMPolicy/"+verb] = Method{Field: "resource", PermissionFor: func(resource string) string {
				switch {
				case strings.Contains(resource, "/certificateRevocationLists/"):
					return "privateca.certificateRevocationLists" + suffix
				case strings.Contains(resource, "/certificateTemplates/"):
					return "privateca.certificateTemplates" + suffix
				}
				return "privateca.caPools" + suffix
			}}
		}
		return m
	}(),
	Deny: newerDenyOf,
}

// computeDeny is Compute Engine's denial, "Required 'P' permission for
// 'R'", 403 with reason forbidden (UNVERIFIED, re-read 2026-11-20: no
// reference page quotes it; it is the wording Compute's own
// troubleshooting pages show).
func computeDeny(permission, resource string) error {
	return status.Error(codes.PermissionDenied, fmt.Sprintf("Required '%s' permission for '%s'", permission, resource))
}

// Compute is Compute Engine's table, for the methods served: each method's
// REST reference (https://docs.cloud.google.com/compute/docs/reference/rest/v1/RESOURCE/METHOD,
// "authorization requires one or more of the following IAM permissions",
// read 2026-11-20), the permission named for the method itself checked on
// the project for an insert, list or aggregatedList and on the resource
// otherwise. Where a page lists further permissions (a patch's get, a
// firewall's compute.networks.updatePolicy, an insert's field-level
// permissions such as compute.subnetworks.use, compute.images.useReadOnly
// and iam.serviceAccounts.actAs on serviceAccounts[].email), only the
// method's own is checked. Compute keeps no resource-level policies on
// what is served, so the project's and its ancestors' apply. Public images
// (debian-cloud, ubuntu-os-cloud, rocky-linux-cloud) are readable by
// every caller, as Google's are (the caller's wrapper skips them).
var Compute = &Service{
	Name: "compute.googleapis.com",
	Deny: computeDeny,
}

// ComputeJSON are Compute's served methods by path, under /compute/v1/:
// the group each pattern captures is the resource checked, the project
// ("projects/P") for an insert, list or aggregatedList.
var ComputeJSON = func() []JSONMethod {
	const p = `projects/[^/]+`
	const global, region, zone = `/global`, `/regions/[^/]+`, `/zones/[^/]+`
	var out []JSONMethod
	add := func(method, pattern, permission string) {
		out = append(out, JSONMethod{Method: method, Pattern: regexp.MustCompile(`^/compute/v1/` + pattern + `$`), Permission: permission})
	}
	// coll is a collection's list, insert, get and delete, its
	// aggregatedList when aggregated, and the verbs on one of its
	// resources (PATCH and PUT are methods on the resource itself).
	coll := func(scope, path, kind string, aggregated bool, verbs map[string]string) {
		one := `(` + p + scope + `/` + path + `/[^/]+)`
		add("GET", `(`+p+`)`+scope+`/`+path, "compute."+kind+".list")
		add("POST", `(`+p+`)`+scope+`/`+path, "compute."+kind+".create")
		add("GET", one, "compute."+kind+".get")
		add("DELETE", one, "compute."+kind+".delete")
		for verb, perm := range verbs {
			if verb == "PATCH" || verb == "PUT" {
				add(verb, one, perm)
				continue
			}
			add("POST", one+`/`+verb, perm)
		}
		if aggregated {
			add("GET", `(`+p+`)/aggregated/`+path, "compute."+kind+".list")
		}
	}
	coll(global, "networks", "networks", false, map[string]string{"PATCH": "compute.networks.update", "switchToCustomMode": "compute.networks.switchToCustomMode"})
	coll(region, "subnetworks", "subnetworks", true, map[string]string{"PATCH": "compute.subnetworks.update",
		"expandIpCidrRange": "compute.subnetworks.expandIpCidrRange", "setPrivateIpGoogleAccess": "compute.subnetworks.setPrivateIpGoogleAccess"})
	coll(global, "firewalls", "firewalls", false, map[string]string{"PATCH": "compute.firewalls.update", "PUT": "compute.firewalls.update"})
	coll(global, "routes", "routes", false, nil)
	coll(region, "addresses", "addresses", true, map[string]string{"setLabels": "compute.addresses.setLabels"})
	coll(global, "addresses", "globalAddresses", false, map[string]string{"setLabels": "compute.globalAddresses.setLabels"})
	coll(zone, "disks", "disks", true, map[string]string{"createSnapshot": "compute.disks.createSnapshot",
		"resize": "compute.disks.resize", "setLabels": "compute.disks.setLabels"})
	coll(global, "snapshots", "snapshots", false, map[string]string{"setLabels": "compute.snapshots.setLabels"})
	instanceVerbs := map[string]string{}
	for _, v := range []string{"attachDisk", "detachDisk", "reset", "resume", "setDeletionProtection", "setLabels", "setMachineType",
		"setMetadata", "setTags", "start", "stop", "suspend"} {
		instanceVerbs[v] = "compute.instances." + v
	}
	coll(zone, "instances", "instances", true, instanceVerbs)
	add("GET", `(`+p+zone+`/instances/[^/]+)/serialPort`, "compute.instances.getSerialPortOutput")
	// Operations: get, wait and delete on the operation, list on the
	// project (aggregatedList is globalOperations.list).
	for scope, kind := range map[string]string{global: "globalOperations", region: "regionOperations", zone: "zoneOperations"} {
		one := `(` + p + scope + `/operations/[^/]+)`
		add("GET", one, "compute."+kind+".get")
		add("POST", one+`/wait`, "compute."+kind+".get")
		add("DELETE", one, "compute."+kind+".delete")
		add("GET", `(`+p+`)`+scope+`/operations`, "compute."+kind+".list")
	}
	add("GET", `(`+p+`)/aggregated/operations`, "compute.globalOperations.list")
	// Read-only catalogs, and the project.
	add("GET", `(`+p+`)/regions`, "compute.regions.list")
	add("GET", `(`+p+region+`)`, "compute.regions.get")
	add("GET", `(`+p+`)/zones`, "compute.zones.list")
	add("GET", `(`+p+zone+`)`, "compute.zones.get")
	add("GET", `(`+p+`/global/images/family/[^/]+)`, "compute.images.getFromFamily")
	add("GET", `(`+p+`)/global/images`, "compute.images.list")
	add("GET", `(`+p+`/global/images/[^/]+)`, "compute.images.get")
	add("GET", `(`+p+`)`+zone+`/machineTypes`, "compute.machineTypes.list")
	add("GET", `(`+p+zone+`/machineTypes/[^/]+)`, "compute.machineTypes.get")
	add("GET", `(`+p+`)`+zone+`/diskTypes`, "compute.diskTypes.list")
	add("GET", `(`+p+zone+`/diskTypes/[^/]+)`, "compute.diskTypes.get")
	add("GET", `(`+p+`)`, "compute.projects.get")
	add("POST", `(`+p+`)/setCommonInstanceMetadata`, "compute.projects.setCommonInstanceMetadata")
	return out
}()

// gkeDeny is Kubernetes Engine's denial, `Required "P" permission(s) for
// "R".` (UNVERIFIED, re-read 2026-11-20: no reference page quotes one).
func gkeDeny(permission, resource string) error {
	return grpcDenied(fmt.Sprintf("Required %q permission(s) for %q.", permission, resource), permission, resource, false)
}

// GKE is Kubernetes Engine's table: the permission each method's REST
// reference names ("Authorization requires the following IAM permission
// on the specified resource", https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1,
// and "GKE API permissions", https://docs.cloud.google.com/kubernetes-engine/docs/reference/api-permissions,
// read 2026-11-20): a node pool's methods are its cluster's
// container.clusters.get or container.clusters.update. GetServerConfig and
// CancelOperation, whose pages name none, are as "GKE audit logging"
// names them (https://docs.cloud.google.com/kubernetes-engine/docs/how-to/audit-logging-container):
// container.clusters.list and container.clusters.update. A request in the
// deprecated form (project_id, zone, cluster_id) is checked on its
// project. CreateCluster also needs "iam.serviceAccounts.actAs on the
// specified service account": its node config's and each node pool's
// (which fields Google reads is UNVERIFIED). Get's and list's
// container.clusters.getCredentials, which only reveals credentials, is
// not checked. Kubernetes Engine keeps no resource-level policies.
var GKE = &Service{
	Name:  "container.googleapis.com",
	Types: map[string]string{"clusters": "container.googleapis.com/Cluster"},
	Methods: func() map[string]Method {
		m := rows(map[string]map[string][2]string{
			"/google.container.v1.ClusterManager/": {
				"CreateCluster":   {"container.clusters.create", "parent|project_id"},
				"GetCluster":      {"container.clusters.get", "name|project_id"},
				"ListClusters":    {"container.clusters.list", "parent|project_id"},
				"DeleteCluster":   {"container.clusters.delete", "name|project_id"},
				"UpdateCluster":   {"container.clusters.update", "name|project_id"},
				"UpdateMaster":    {"container.clusters.update", "name|project_id"},
				"SetLabels":       {"container.clusters.update", "name|project_id"},
				"SetMasterAuth":   {"container.clusters.update", "name|project_id"},
				"GetServerConfig": {"container.clusters.list", "name|project_id"},
				"ListNodePools":   {"container.clusters.get", "parent|project_id"},
				"GetNodePool":     {"container.clusters.get", "name|project_id"},
				"CreateNodePool":  {"container.clusters.update", "parent|project_id"},
				"DeleteNodePool":  {"container.clusters.update", "name|project_id"},
				"SetNodePoolSize": {"container.clusters.update", "name|project_id"},
				"UpdateNodePool":  {"container.clusters.update", "name|project_id"},
				"GetOperation":    {"container.operations.get", "name|project_id"},
				"ListOperations":  {"container.operations.list", "parent|project_id"},
				"CancelOperation": {"container.clusters.update", "name|project_id"},
			},
		})
		row := m["/google.container.v1.ClusterManager/CreateCluster"]
		row.ActAs = []string{"cluster.node_config.service_account", "cluster.node_pools.config.service_account"}
		m["/google.container.v1.ClusterManager/CreateCluster"] = row
		return m
	}(),
	Deny: gkeDeny,
}

// APIKeys is the API Keys API's table, over JSON (the paths its server
// routes): each method's REST reference
// (https://docs.cloud.google.com/api-keys/docs/reference/rest/v2/projects.locations.keys,
// and "Access control", https://docs.cloud.google.com/api-keys/docs/access-control,
// read 2026-11-20). Not checked: keys.lookupKey, whose apikeys.keys.lookup
// is "on the parent project" of a key the request names only by its
// string, and operations.get, whose serviceusage.operations.get names no
// resource an operation's name carries. API keys keep no resource-level
// policies. Google's wording of a denial is UNVERIFIED (re-read
// 2026-11-20: the pages give none): the newer APIs'.
var APIKeys = &Service{Name: "apikeys.googleapis.com", Deny: newerDenyOf}

// APIKeysJSON are API Keys' methods by path.
var APIKeysJSON = func() []JSONMethod {
	const keys = `^/v2/(projects/[^/]+/locations/[^/]+)/keys$`
	const key = `^/v2/(projects/[^/]+/locations/[^/]+/keys/[^/:]+)`
	return []JSONMethod{
		{Method: "POST", Pattern: regexp.MustCompile(keys), Permission: "apikeys.keys.create"},
		{Method: "GET", Pattern: regexp.MustCompile(keys), Permission: "apikeys.keys.list"},
		{Method: "GET", Pattern: regexp.MustCompile(key + `/keyString$`), Permission: "apikeys.keys.getKeyString"},
		{Method: "GET", Pattern: regexp.MustCompile(key + `$`), Permission: "apikeys.keys.get"},
		{Method: "PATCH", Pattern: regexp.MustCompile(key + `$`), Permission: "apikeys.keys.update"},
		{Method: "DELETE", Pattern: regexp.MustCompile(key + `$`), Permission: "apikeys.keys.delete"},
		{Method: "POST", Pattern: regexp.MustCompile(key + `:undelete$`), Permission: "apikeys.keys.undelete"},
	}
}()

// ServiceUsage is the Service Usage API's table, over JSON: each method's
// REST reference (https://docs.cloud.google.com/service-usage/docs/reference/rest/v1/services,
// read 2026-11-20); batchEnable is serviceusage.services.enable and
// batchGet serviceusage.services.get, on the parent. Not checked: the
// operations' methods (only operations.get has a permission,
// serviceusage.operations.get, on no resource an operation's name
// carries), v1beta1's generateServiceIdentity (no page names one), and
// the servicemanagement.services.bind "Service Usage audit logging" lists
// beside an enable, which names the service producer's project. Services
// keep no resource-level policies. Google's wording of a denial is
// UNVERIFIED (re-read 2026-11-20: the pages give none): the newer APIs'.
var ServiceUsage = &Service{Name: "serviceusage.googleapis.com", Deny: newerDenyOf}

// ServiceUsageJSON are Service Usage's v1 methods by path; a consumer is a
// project, folder or organization.
var ServiceUsageJSON = func() []JSONMethod {
	const parent = `((?:projects|folders|organizations)/[^/]+)`
	const svc = `^/v1/(` + `(?:projects|folders|organizations)/[^/]+/services/[^/:]+)`
	return []JSONMethod{
		{Method: "GET", Pattern: regexp.MustCompile(`^/v1/` + parent + `/services$`), Permission: "serviceusage.services.list"},
		{Method: "GET", Pattern: regexp.MustCompile(`^/v1/` + parent + `/services:batchGet$`), Permission: "serviceusage.services.get"},
		{Method: "POST", Pattern: regexp.MustCompile(`^/v1/` + parent + `/services:batchEnable$`), Permission: "serviceusage.services.enable"},
		{Method: "GET", Pattern: regexp.MustCompile(svc + `$`), Permission: "serviceusage.services.get"},
		{Method: "POST", Pattern: regexp.MustCompile(svc + `:enable$`), Permission: "serviceusage.services.enable"},
		{Method: "POST", Pattern: regexp.MustCompile(svc + `:disable$`), Permission: "serviceusage.services.disable"},
	}
}()

// RunPortOperation is the API, and its permissions to get, list and
// delete operations, of an operation the Cloud Run port serves, by its
// metadata's type URL: the port's one Operations service serves Cloud
// Run's, Cloud Functions' and Cloud Build's operations (and those of the
// other APIs on it). Cloud Run's are "Cloud Run audit logging"'s
// run.operations.get, .list and .delete (WaitOperation is .get,
// https://docs.cloud.google.com/run/docs/audit-logging); Cloud Functions'
// and Cloud Build's are the permission index's cloudfunctions.operations.get
// and .list and cloudbuild.operations.get and .list (UNVERIFIED which
// method each guards, re-read 2026-11-20: their reference pages name
// none; neither has a delete permission). Any other API's operation
// (Artifact Registry's, whose audit page lists its operations among the
// methods that produce no audit logs, and names no permission; Eventarc's;
// Secure Source Manager's) is not checked: svc nil.
func RunPortOperation(typeURL string) (svc *Service, get, list, del string) {
	name := typeURL[strings.LastIndex(typeURL, "/")+1:]
	switch {
	case strings.HasPrefix(name, "google.cloud.run.v2."):
		return Run, "run.operations.get", "run.operations.list", "run.operations.delete"
	case strings.HasPrefix(name, "google.cloud.functions."):
		return Functions, "cloudfunctions.operations.get", "cloudfunctions.operations.list", ""
	case strings.HasPrefix(name, "google.devtools.cloudbuild."):
		return CloudBuild, "cloudbuild.operations.get", "cloudbuild.operations.list", ""
	}
	return nil, "", "", ""
}

// RunPortListOperations are the permissions, any one of which lets a
// caller list a location's operations on the Cloud Run port; each listed
// operation is then kept only if the caller holds its own API's list
// permission (RunPortOperation).
var RunPortListOperations = []string{"run.operations.list", "cloudfunctions.operations.list", "cloudbuild.operations.list"}

// RunPortListLocations are the permissions, any one of which lets a caller
// list locations on the Cloud Run port, whose one Locations service
// answers both Cloud Run's ListLocations, run.locations.list ("Cloud Run
// audit logging"), and Cloud Functions', cloudfunctions.locations.list
// (the permission index; UNVERIFIED, re-read 2026-11-20: no page names
// it). GetLocation is not checked: neither API names a permission for it
// (the index has no run.locations.get or cloudfunctions.locations.get).
var RunPortListLocations = []string{"run.locations.list", "cloudfunctions.locations.list"}

// ArtifactRegistryLocationsJSON are Artifact Registry's Locations mixin
// under its own prefix on the Cloud Run port: artifactregistry.locations.list
// and .get, as the permission index names them (UNVERIFIED, re-read
// 2026-11-20: the audit page lists both methods among those that produce
// no audit logs and names no permission).
var ArtifactRegistryLocationsJSON = []JSONMethod{
	{Method: "GET", Pattern: regexp.MustCompile(`^/artifactregistry/v1/(projects/[^/]+)/locations$`), Permission: "artifactregistry.locations.list"},
	{Method: "GET", Pattern: regexp.MustCompile(`^/artifactregistry/v1/(projects/[^/]+/locations/[^/]+)$`), Permission: "artifactregistry.locations.get"},
}

// RunUploadSourceJSON is UploadSource's media path, which the JSON
// transcoder does not route.
var RunUploadSourceJSON = []JSONMethod{
	{Method: "POST", Pattern: regexp.MustCompile(`^/upload/v2/(projects/[^/]+/locations/[^/:]+):uploadSource$`), Permission: RunUploadSource},
}

// HoldsAny is whether the caller of bearer holds any of permissions on
// resource in svc (enforcement off: true); err is UNAUTHENTICATED for a
// token this instance did not mint.
func (e *Enforcer) HoldsAny(ctx context.Context, svc *Service, bearer string, permissions []string, resource string) (bool, error) {
	if e == nil || e.On == nil || !e.On() {
		return true, nil
	}
	c := Caller{Owner: true}
	if bearer != "" && e.CallerOf != nil {
		c = e.CallerOf(bearer)
	}
	if c.Invalid {
		return false, Unauthenticated()
	}
	for _, p := range permissions {
		if e.holds(ctx, svc, c, p, resource) {
			return true, nil
		}
	}
	return false, nil
}
