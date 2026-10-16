package storage

// status says whether a discovery method is served.
type status int

const (
	unimplemented status = iota
	built
)

// methodStatus lists every method in the discovery document, and only those
// (TestStorageDiscoveryDrift). A method moves to built in the issue that
// implements it; until then it answers 501 notImplemented naming it, never a
// plausible stub. Buckets are built (#490), objects.insert, get and delete (#491), objects.patch and update (#492), objects.list (#494), and compose, copy, rewrite and move (#495); managed folders, all but update (#828); hierarchical-namespace folders and their operations' get and list (#1203); the three ACL resources, stored and never enforced (#1233); channels.stop, with objects.watchAll routed outside the discovery document (#1281); Anywhere Caches, as records with no cache tier behind them, buckets.relocate, advanceRelocateBucket and operations.cancel (#1444).
var methodStatus = map[string]status{
	"storage.anywhereCaches.disable":                   built,
	"storage.anywhereCaches.get":                       built,
	"storage.anywhereCaches.insert":                    built,
	"storage.anywhereCaches.list":                      built,
	"storage.anywhereCaches.pause":                     built,
	"storage.anywhereCaches.resume":                    built,
	"storage.anywhereCaches.update":                    built,
	"storage.bucketAccessControls.delete":              built,
	"storage.bucketAccessControls.get":                 built,
	"storage.bucketAccessControls.insert":              built,
	"storage.bucketAccessControls.list":                built,
	"storage.bucketAccessControls.patch":               built,
	"storage.bucketAccessControls.update":              built,
	"storage.buckets.delete":                           built,
	"storage.buckets.get":                              built,
	"storage.buckets.getIamPolicy":                     built,
	"storage.buckets.getStorageLayout":                 built,
	"storage.buckets.insert":                           built,
	"storage.buckets.list":                             built,
	"storage.buckets.lockRetentionPolicy":              built,
	"storage.buckets.operations.advanceRelocateBucket": built,
	"storage.buckets.operations.cancel":                built,
	"storage.buckets.operations.get":                   built,
	"storage.buckets.operations.list":                  built,
	"storage.buckets.patch":                            built,
	"storage.buckets.relocate":                         built,
	"storage.buckets.restore":                          built,
	"storage.buckets.setIamPolicy":                     built,
	"storage.buckets.testIamPermissions":               built,
	"storage.buckets.update":                           built,
	"storage.channels.stop":                            built,
	"storage.defaultObjectAccessControls.delete":       built,
	"storage.defaultObjectAccessControls.get":          built,
	"storage.defaultObjectAccessControls.insert":       built,
	"storage.defaultObjectAccessControls.list":         built,
	"storage.defaultObjectAccessControls.patch":        built,
	"storage.defaultObjectAccessControls.update":       built,
	"storage.folders.delete":                           built,
	"storage.folders.deleteRecursive":                  built,
	"storage.folders.get":                              built,
	"storage.folders.insert":                           built,
	"storage.folders.list":                             built,
	"storage.folders.rename":                           built,
	"storage.managedFolders.delete":                    built,
	"storage.managedFolders.get":                       built,
	"storage.managedFolders.getIamPolicy":              built,
	"storage.managedFolders.insert":                    built,
	"storage.managedFolders.list":                      built,
	"storage.managedFolders.setIamPolicy":              built,
	"storage.managedFolders.testIamPermissions":        built,
	"storage.managedFolders.update":                    unimplemented,
	"storage.notifications.delete":                     built,
	"storage.notifications.get":                        built,
	"storage.notifications.insert":                     built,
	"storage.notifications.list":                       built,
	"storage.objectAccessControls.delete":              built,
	"storage.objectAccessControls.get":                 built,
	"storage.objectAccessControls.insert":              built,
	"storage.objectAccessControls.list":                built,
	"storage.objectAccessControls.patch":               built,
	"storage.objectAccessControls.update":              built,
	"storage.objects.bulkRestore":                      built,
	"storage.objects.compose":                          built,
	"storage.objects.copy":                             built,
	"storage.objects.delete":                           built,
	"storage.objects.get":                              built,
	"storage.objects.getIamPolicy":                     unimplemented,
	"storage.objects.insert":                           built,
	"storage.objects.list":                             built,
	"storage.objects.move":                             built,
	"storage.objects.patch":                            built,
	"storage.objects.restore":                          built,
	"storage.objects.rewrite":                          built,
	"storage.objects.setIamPolicy":                     unimplemented,
	"storage.objects.testIamPermissions":               unimplemented,
	"storage.objects.update":                           built,
	"storage.projects.hmacKeys.create":                 built,
	"storage.projects.hmacKeys.delete":                 built,
	"storage.projects.hmacKeys.get":                    built,
	"storage.projects.hmacKeys.list":                   built,
	"storage.projects.hmacKeys.update":                 built,
	"storage.projects.serviceAccount.get":              built,
	"storage.rapidCaches.disable":                      unimplemented,
	"storage.rapidCaches.get":                          unimplemented,
	"storage.rapidCaches.insert":                       unimplemented,
	"storage.rapidCaches.list":                         unimplemented,
	"storage.rapidCaches.update":                       unimplemented,
}

// MethodStatuses reports every discovery method and whether it is built,
// for the coverage report (tools/coverage, #520): a built method is served,
// and any other answers 501 notImplemented naming it.
func MethodStatuses() map[string]bool {
	out := make(map[string]bool, len(methodStatus))
	for id, st := range methodStatus {
		out[id] = st == built
	}
	return out
}
