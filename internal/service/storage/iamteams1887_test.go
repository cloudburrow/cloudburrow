package storage

import (
	"strconv"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestACLProjectTeams (#1887): a bucket ACL's project-editors-NUMBER entity
// grants its role to a caller the host names on that project's editors
// team, and not to one on no team; project-viewers- reads.
func TestACLProjectTeams(t *testing.T) {
	num := strconv.FormatInt(resource.ProjectNumber("teams-p"), 10)
	b := bucketRecord{Project: "teams-p", ACL: []aclEntry{{Entity: "project-editors-" + num, Role: "WRITER"}, {Entity: "project-viewers-" + num, Role: "READER"}}}
	tg := target{bucket: "b", object: "o"}
	editor := iamcheck.Decision{Members: []string{"user:e@example.com"}, Teams: []string{"editors"}}
	viewer := iamcheck.Decision{Members: []string{"user:v@example.com"}, Teams: []string{"viewers"}}
	nobody := iamcheck.Decision{Members: []string{"user:n@example.com"}}
	if !aclGrants("storage.objects.create", target{bucket: "b"}, b, nil, aclMembers(editor, b)) {
		t.Error("project-editors WRITER did not let an editor create")
	}
	if aclGrants("storage.objects.create", target{bucket: "b"}, b, nil, aclMembers(viewer, b)) {
		t.Error("project-viewers READER let a viewer create")
	}
	if !aclGrants("storage.objects.list", target{bucket: "b"}, b, nil, aclMembers(viewer, b)) {
		t.Error("project-viewers READER did not let a viewer list")
	}
	if aclGrants("storage.objects.list", target{bucket: "b"}, b, nil, aclMembers(nobody, b)) {
		t.Error("a caller on no team was granted through a team entity")
	}
	objACL := []aclEntry{{Entity: "project-viewers-" + num, Role: "READER"}}
	if !aclGrants("storage.objects.get", tg, bucketRecord{Project: "teams-p"}, objACL, aclMembers(viewer, b)) {
		t.Error("an object's project-viewers READER did not let a viewer read")
	}
}
