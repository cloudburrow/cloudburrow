package run

// Resetting a project (#1543): what /admin/reset?project= and a project's
// purge (#1537) delete. Google deletes a purged project's Artifact
// Registry repositories, with their packages, versions, tags, files and
// rules, and its project settings and configs; a project created again
// under its ID has none of them.

import "strings"

// ResetArtifactRegistry deletes what Artifact Registry keeps for project,
// or for every project when project is "": each repository, as
// DeleteRepository does, with its images, packages, files, rules,
// attachments, annotations and IAM policy, and the project's settings,
// VPC-SC configs and locations' projectConfigs, in memory and in the
// registry's state directory. Images built under a repository's URL stay
// loaded in the cluster, as DeleteRepository leaves them: they are this
// instance's build cache.
func ResetArtifactRegistry(project string) error {
	under := func(name string) bool { return project == "" || strings.HasPrefix(name, "projects/"+project+"/") }
	repositories.mu.Lock()
	var names []string
	for n := range repositories.repos {
		if under(n) {
			names = append(names, n)
		}
	}
	repositories.mu.Unlock()
	for _, n := range names {
		RemoveRepository(n)
	}

	// What outlives a repository's record: its project's settings and
	// configs, and anything left under a name no repository holds.
	repositories.mu.Lock()
	policies := false
	for k := range repositories.policies {
		if under(k) {
			delete(repositories.policies, k)
			policies = true
		}
	}
	if policies {
		saveRepositoryPoliciesLocked()
	}
	repositories.mu.Unlock()

	arConfig.mu.Lock()
	n := dropUnder(arConfig.rules, under) + dropUnder(arConfig.attachments, under) +
		dropUnder(arConfig.settings, under) + dropUnder(arConfig.vpcsc, under) +
		dropUnder(arConfig.annotations, under) + dropUnder(arConfig.displayNames, under) +
		dropUnder(arConfig.projectConfigs, under)
	if n > 0 {
		saveARConfigLocked()
	}
	arConfig.mu.Unlock()

	var orphans []string
	fmtState.mu.Lock()
	for n := range fmtState.repos {
		if under(n) {
			orphans = append(orphans, n)
		}
	}
	fmtState.mu.Unlock()
	for _, n := range orphans {
		dropFormatRepo(n)
	}
	return nil
}

// dropUnder deletes m's keys under, and says how many it deleted.
func dropUnder[V any](m map[string]V, under func(string) bool) int {
	n := 0
	for k := range m {
		if under(k) {
			delete(m, k)
			n++
		}
	}
	return n
}
