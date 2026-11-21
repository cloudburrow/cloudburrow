package iameval

import "fmt"

// AppHub is App Hub's table (#2110): the permission and resource each
// method's REST reference names
// (https://docs.cloud.google.com/app-hub/docs/reference/rest/v1, read
// 2026-11-21). Four need a second permission on another resource:
// serviceProjectAttachments.create apphub.serviceProjectAttachments.attach
// on the service project, updateBoundary apphub.boundaries.attach on the
// CRM node, and services.create and workloads.create
// apphub.discovered{Services,Workloads}.register on the discovered
// resource. The discovered resources' lookups are checked as their lists.
// getIamPolicy and setIamPolicy name no permission there; the roles hold
// apphub.applications.getIamPolicy and setIamPolicy, which they take
// (testIamPermissions is not checked, as elsewhere). Applications keep
// their own policies. Google's wording of a denial is UNVERIFIED: its
// newer APIs' "Permission 'P' denied on resource 'R' (or it may not
// exist).", with ErrorInfo IAM_PERMISSION_DENIED.
var AppHub = &Service{
	Name:              "apphub.googleapis.com",
	PolicyCollections: []string{"applications"},
	Types: map[string]string{"applications": "apphub.googleapis.com/Application",
		"services": "apphub.googleapis.com/Service", "workloads": "apphub.googleapis.com/Workload",
		"serviceProjectAttachments": "apphub.googleapis.com/ServiceProjectAttachment",
		"discoveredServices":        "apphub.googleapis.com/DiscoveredService",
		"discoveredWorkloads":       "apphub.googleapis.com/DiscoveredWorkload"},
	Methods: func() map[string]Method {
		const p = "/google.cloud.apphub.v1.AppHub/"
		m := sm(map[string][2]string{
			"GetBoundary":                    {"apphub.boundaries.get", "name"},
			"UpdateBoundary":                 {"apphub.boundaries.update", "boundary.name"},
			"LookupServiceProjectAttachment": {"apphub.serviceProjectAttachments.lookup", "name"},
			"DetachServiceProjectAttachment": {"apphub.serviceProjectAttachments.detach", "name"},
			"CreateServiceProjectAttachment": {"apphub.serviceProjectAttachments.create", "parent"},
			"GetServiceProjectAttachment":    {"apphub.serviceProjectAttachments.get", "name"},
			"ListServiceProjectAttachments":  {"apphub.serviceProjectAttachments.list", "parent"},
			"DeleteServiceProjectAttachment": {"apphub.serviceProjectAttachments.delete", "name"},
			"CreateApplication":              {"apphub.applications.create", "parent"},
			"GetApplication":                 {"apphub.applications.get", "name"},
			"ListApplications":               {"apphub.applications.list", "parent"},
			"UpdateApplication":              {"apphub.applications.update", "application.name"},
			"DeleteApplication":              {"apphub.applications.delete", "name"},
			"CreateService":                  {"apphub.services.create", "parent"},
			"GetService":                     {"apphub.services.get", "name"},
			"ListServices":                   {"apphub.services.list", "parent"},
			"UpdateService":                  {"apphub.services.update", "service.name"},
			"DeleteService":                  {"apphub.services.delete", "name"},
			"CreateWorkload":                 {"apphub.workloads.create", "parent"},
			"GetWorkload":                    {"apphub.workloads.get", "name"},
			"ListWorkloads":                  {"apphub.workloads.list", "parent"},
			"UpdateWorkload":                 {"apphub.workloads.update", "workload.name"},
			"DeleteWorkload":                 {"apphub.workloads.delete", "name"},
			"GetDiscoveredService":           {"apphub.discoveredServices.get", "name"},
			"ListDiscoveredServices":         {"apphub.discoveredServices.list", "parent"},
			"LookupDiscoveredService":        {"apphub.discoveredServices.list", "parent"},
			"GetDiscoveredWorkload":          {"apphub.discoveredWorkloads.get", "name"},
			"ListDiscoveredWorkloads":        {"apphub.discoveredWorkloads.list", "parent"},
			"LookupDiscoveredWorkload":       {"apphub.discoveredWorkloads.list", "parent"},
			"GetExtendedMetadataSchema":      {"apphub.extendedMetadataSchemas.get", "name"},
			"ListExtendedMetadataSchemas":    {"apphub.extendedMetadataSchemas.list", "parent"},
		}, p)
		also := map[string]Also{
			"CreateServiceProjectAttachment": {Field: "service_project_attachment.service_project", Permission: "apphub.serviceProjectAttachments.attach"},
			"UpdateBoundary":                 {Field: "boundary.crm_node", Permission: "apphub.boundaries.attach"},
			"CreateService":                  {Field: "service.discovered_service", Permission: "apphub.discoveredServices.register"},
			"CreateWorkload":                 {Field: "workload.discovered_workload", Permission: "apphub.discoveredWorkloads.register"},
		}
		for k, a := range also {
			row := m[p+k]
			row.Also = []Also{a}
			m[p+k] = row
		}
		for prefix, rows := range map[string]map[string][2]string{
			"/google.cloud.location.Locations/": {
				"GetLocation":   {"apphub.locations.get", "name"},
				"ListLocations": {"apphub.locations.list", "name"},
			},
			"/google.longrunning.Operations/": {
				"GetOperation":    {"apphub.operations.get", "name"},
				"ListOperations":  {"apphub.operations.list", "name"},
				"DeleteOperation": {"apphub.operations.delete", "name"},
				"CancelOperation": {"apphub.operations.cancel", "name"},
			},
			"/google.iam.v1.IAMPolicy/": {
				"GetIamPolicy": {"apphub.applications.getIamPolicy", "resource"},
				"SetIamPolicy": {"apphub.applications.setIamPolicy", "resource"},
			},
		} {
			for k, v := range sm(rows, prefix) {
				m[k] = v
			}
		}
		return m
	}(),
	Deny: func(permission, resource string) error {
		return grpcDenied(fmt.Sprintf("Permission '%s' denied on resource '%s' (or it may not exist).", permission, resource),
			permission, resource, true)
	},
}
