package storage

// Organization policy constraints on Cloud Storage (#1890), asked of the
// host through internal/orgpolicy (the storage server's --resourcemanager),
// whatever IAM enforcement, as on Google Cloud. Each refusal is Cloud
// Storage's 412 conditionNotMet, the code "Restricting Resource Locations"
// documents for gcp.resourceLocations ("the error has code 412 and reason
// conditionNotMet"), and the public access prevention page for
// allUsers/allAuthenticatedUsers grants ("fail with 412 Precondition
// Failed"):
//
//   - gcp.resourceLocations: a bucket created in a location (a region, a
//     dual-region, a multi-region; a custom dual-region's data locations)
//     the constraint does not allow, with the documented message;
//   - storage.uniformBucketLevelAccess: a bucket created, or updated,
//     without uniform bucket-level access;
//   - storage.publicAccessPrevention: on a bucket whose
//     iamConfiguration.publicAccessPrevention is enforced, or inherited
//     (or unset) under an enforcing policy, an IAM policy or ACL that adds
//     allUsers or allAuthenticatedUsers. Grants already made stay, as
//     Google keeps them (inactive);
//   - iam.allowedPolicyMemberDomains: a bucket or managed folder IAM policy
//     that adds a member outside the permitted customers.
//
// The messages of the uniform bucket-level access and public access
// prevention refusals are UNVERIFIED: Google's pages give the code, not
// the text (re-read 2026-11-19:
// https://docs.cloud.google.com/storage/docs/public-access-prevention,
// https://docs.cloud.google.com/storage/docs/uniform-bucket-level-access).

import (
	"context"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

func orgPolicyRefusal(err error) error {
	return errorf(http.StatusPreconditionFailed, "conditionNotMet", "%s", err.Error())
}

func bucketProject(b bucketRecord) string {
	if b.Project == "" {
		return ""
	}
	return "projects/" + b.Project
}

// checkBucketOrgPolicy checks a bucket being created (creating) or
// updated against gcp.resourceLocations and
// storage.uniformBucketLevelAccess.
func checkBucketOrgPolicy(b bucketRecord, creating bool) error {
	project := bucketProject(b)
	if project == "" || !orgpolicy.SourceSet() {
		return nil
	}
	ctx := context.Background()
	if creating {
		locs := []string{}
		if loc, _ := b.Fields["location"].(string); loc != "" {
			locs = append(locs, loc)
		}
		locs = append(locs, dataLocations(b.Fields["customPlacementConfig"])...)
		if err := orgpolicy.CheckLocations(ctx, project, project, locs...); err != nil {
			return orgPolicyRefusal(err)
		}
	}
	if !uniformAccess(b) && orgpolicy.Enforced(ctx, project, orgpolicy.UniformBucketLevelAccess) {
		return errorf(http.StatusPreconditionFailed, "conditionNotMet", "Request violates constraint 'constraints/storage.uniformBucketLevelAccess'")
	}
	return nil
}

// publicAccessPrevented reports whether public access prevention applies
// to b: its own setting is enforced, or it inherits an enforcing policy.
func publicAccessPrevented(b bucketRecord) bool {
	cfg, _ := b.Fields["iamConfiguration"].(map[string]any)
	switch pap, _ := cfg["publicAccessPrevention"].(string); pap {
	case "enforced":
		return true
	case "", "inherited", "unspecified":
		project := bucketProject(b)
		return project != "" && orgpolicy.Enforced(context.Background(), project, orgpolicy.PublicAccessPrevention)
	}
	return false
}

func isPublic(member string) bool {
	return member == "allUsers" || member == "allAuthenticatedUsers"
}

// papRefusal is the 412 a public grant on a prevented bucket gets.
func papRefusal() error {
	return errorf(http.StatusPreconditionFailed, "conditionNotMet",
		"The member bindings allUsers and allAuthenticatedUsers are not allowed since public access prevention is enforced.")
}

// checkPublicACL refuses ACL entries for allUsers or allAuthenticatedUsers
// that before does not already hold, on a bucket public access
// prevention applies to.
func checkPublicACL(b bucketRecord, before, after []aclEntry) error {
	had := map[string]bool{}
	for _, e := range before {
		had[e.Entity] = true
	}
	added := false
	for _, e := range after {
		if isPublic(e.Entity) && !had[e.Entity] {
			added = true
		}
	}
	if added && publicAccessPrevented(b) {
		return papRefusal()
	}
	return nil
}

func policyMembers(p *bucketPolicy) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, b := range p.Bindings {
		out = append(out, b.Members...)
	}
	return out
}

// checkPolicyOrg checks an IAM policy set on bucket b (or a managed
// folder of it) against public access prevention and domain restricted
// sharing.
func checkPolicyOrg(b bucketRecord, current *bucketPolicy, next bucketPolicy) error {
	before, after := policyMembers(current), policyMembers(&next)
	had := map[string]bool{}
	for _, m := range before {
		had[m] = true
	}
	for _, m := range after {
		if isPublic(m) && !had[m] && publicAccessPrevented(b) {
			return papRefusal()
		}
	}
	if err := orgpolicy.CheckMembers(context.Background(), bucketProject(b), before, after); err != nil {
		return orgPolicyRefusal(err)
	}
	return nil
}
