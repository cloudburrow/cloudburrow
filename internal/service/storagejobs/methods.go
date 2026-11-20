package storagejobs

// MethodStatuses is every method of the three APIs' discovery documents
// (Storage Insights: its REST mapping), true where it is served.
func MethodStatuses() map[string]bool {
	out := map[string]bool{}
	for _, m := range []string{
		"storagebatchoperations.projects.locations.get", "storagebatchoperations.projects.locations.list",
		"storagebatchoperations.projects.locations.jobs.create", "storagebatchoperations.projects.locations.jobs.get",
		"storagebatchoperations.projects.locations.jobs.list", "storagebatchoperations.projects.locations.jobs.cancel",
		"storagebatchoperations.projects.locations.jobs.delete",
		"storagebatchoperations.projects.locations.jobs.bucketOperations.get",
		"storagebatchoperations.projects.locations.jobs.bucketOperations.list",
		"storagebatchoperations.projects.locations.operations.get", "storagebatchoperations.projects.locations.operations.list",
		"storagebatchoperations.projects.locations.operations.cancel", "storagebatchoperations.projects.locations.operations.delete",
		"storageinsights.projects.locations.get", "storageinsights.projects.locations.list",
		"storageinsights.projects.locations.reportConfigs.create", "storageinsights.projects.locations.reportConfigs.get",
		"storageinsights.projects.locations.reportConfigs.list", "storageinsights.projects.locations.reportConfigs.patch",
		"storageinsights.projects.locations.reportConfigs.delete",
		"storageinsights.projects.locations.reportConfigs.reportDetails.get",
		"storageinsights.projects.locations.reportConfigs.reportDetails.list",
		"storageinsights.projects.locations.datasetConfigs.create", "storageinsights.projects.locations.datasetConfigs.get",
		"storageinsights.projects.locations.datasetConfigs.list", "storageinsights.projects.locations.datasetConfigs.patch",
		"storageinsights.projects.locations.datasetConfigs.delete", "storageinsights.projects.locations.datasetConfigs.linkDataset",
		"storageinsights.projects.locations.datasetConfigs.unlinkDataset",
		"storageinsights.projects.locations.operations.get", "storageinsights.projects.locations.operations.list",
		"storageinsights.projects.locations.operations.cancel", "storageinsights.projects.locations.operations.delete",
		"storagetransfer.googleServiceAccounts.get",
		"storagetransfer.transferJobs.create", "storagetransfer.transferJobs.get", "storagetransfer.transferJobs.list",
		"storagetransfer.transferJobs.patch", "storagetransfer.transferJobs.run", "storagetransfer.transferJobs.delete",
		"storagetransfer.transferOperations.get", "storagetransfer.transferOperations.list",
		"storagetransfer.transferOperations.pause", "storagetransfer.transferOperations.resume",
		"storagetransfer.transferOperations.cancel",
		"storagetransfer.projects.agentPools.create", "storagetransfer.projects.agentPools.get",
		"storagetransfer.projects.agentPools.list", "storagetransfer.projects.agentPools.patch",
		"storagetransfer.projects.agentPools.delete",
	} {
		out[m] = true
	}
	return out
}
