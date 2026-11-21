package firebaserules

// Methods is every method of the firebaserules v1 discovery document
// (google.golang.org/api/firebaserules/v1), by discovery ID; all served.
var Methods = []string{
	"firebaserules.projects.test",
	"firebaserules.projects.rulesets.create",
	"firebaserules.projects.rulesets.get",
	"firebaserules.projects.rulesets.list",
	"firebaserules.projects.rulesets.delete",
	"firebaserules.projects.releases.create",
	"firebaserules.projects.releases.get",
	"firebaserules.projects.releases.getExecutable",
	"firebaserules.projects.releases.list",
	"firebaserules.projects.releases.patch",
	"firebaserules.projects.releases.delete",
}

// MethodStatuses maps each method to whether it is served, for
// tools/coverage.
func MethodStatuses() map[string]bool {
	out := make(map[string]bool, len(Methods))
	for _, m := range Methods {
		out[m] = true
	}
	return out
}
