package iameval

// Workbench is Vertex AI Workbench's table (#2173): notebooks.googleapis.com
// v2's instances methods, each with the permission its REST page names
// ("Requires the following IAM permission on the specified resource",
// https://docs.cloud.google.com/vertex-ai/docs/workbench/reference/rest/v2/projects.locations.instances/...,
// read 2026-11-22): notebooks.instances.create and .list on the parent;
// .get, .update, .delete, .start, .stop, .reset, .checkUpgradability and
// .upgrade on the instance. Creating an instance with a service account
// also needs iam.serviceAccounts.actAs on it, which the server checks
// (workbench.go). Its instances' own IAM policies are not served.
//
// UNVERIFIED (re-read 2026-11-22): the denial's wording, Google's newer
// APIs'.
var Workbench = &Service{
	Name: "notebooks.googleapis.com",
	Deny: newerDenyOf,
	Methods: rows(map[string]map[string][2]string{
		"/google.cloud.notebooks.v2.NotebookService/": {
			"CreateInstance":             {"notebooks.instances.create", "parent"},
			"ListInstances":              {"notebooks.instances.list", "parent"},
			"GetInstance":                {"notebooks.instances.get", "name"},
			"UpdateInstance":             {"notebooks.instances.update", "instance.name"},
			"DeleteInstance":             {"notebooks.instances.delete", "name"},
			"StartInstance":              {"notebooks.instances.start", "name"},
			"StopInstance":               {"notebooks.instances.stop", "name"},
			"ResetInstance":              {"notebooks.instances.reset", "name"},
			"CheckInstanceUpgradability": {"notebooks.instances.checkUpgradability", "notebook_instance"},
			"UpgradeInstance":            {"notebooks.instances.upgrade", "name"},
		},
	}),
}

func init() {
	Services["google.cloud.notebooks.v2.NotebookService"] = Workbench
}
