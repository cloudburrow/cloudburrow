package redisadmin

import (
	"reflect"
	"testing"
	"time"
)

// TestIAMUsers (#1614): an IAM auth record's nodes get the default user
// with its own password and the hashes of the live tokens of the
// principals granted the product's connect role, and a user per principal;
// a token of an ungranted or unknown account is not among them.
func TestIAMUsers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var asked []string
	c := &clusters{now: func() time.Time { return now }, iam: &iamState{src: IAMSource{
		Tokens: func(time.Time) []IssuedToken {
			return []IssuedToken{
				{Hash: "aa", Email: "ok@p.iam.gserviceaccount.com"},
				{Hash: "bb", Email: "OK@p.iam.gserviceaccount.com"},
				{Hash: "cc", Email: "other@p.iam.gserviceaccount.com"},
				{Hash: "dd"},
				{Hash: "ee", Email: "dev@example.com"},
			}
		},
		Members: func(project, role string) []string {
			asked = append(asked, project+" "+role)
			return []string{"serviceAccount:ok@p.iam.gserviceaccount.com", "user:dev@example.com", "group:g@example.com"}
		},
	}}}
	r := &clusterRecord{Name: "projects/p/locations/us-central1/clusters/c", AuthMode: "AUTH_MODE_IAM_AUTH", AdminPassword: "pw"}
	got := c.iamUsers(r)
	want := map[string][]any{
		"default":                      {"on", "resetpass", ">pw", "#aa", "#bb", "#ee"},
		"ok@p.iam.gserviceaccount.com": {"reset", "on", "#aa", "#bb", "~*", "&*", "+@all"},
		"dev@example.com":              {"reset", "on", "#ee", "~*", "&*", "+@all"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("iamUsers = %v, want %v", got, want)
	}
	if len(asked) != 1 || asked[0] != "p roles/redis.dbConnectionUser" {
		t.Errorf("asked for %v, want the Redis cluster role on p", asked)
	}
	v := &clusterRecord{Name: "projects/p/locations/us-central1/instances/v", AdminPassword: "pw",
		Valkey: &valkeyFields{AuthMode: "IAM_AUTH"}}
	asked = nil
	c.iamUsers(v)
	if len(asked) != 1 || asked[0] != "p roles/memorystore.dbConnectionUser" {
		t.Errorf("asked for %v, want the Memorystore role on p", asked)
	}
	// Not IAM auth, or kept from before #1614 with no password: nothing.
	for _, r := range []*clusterRecord{
		{Name: r.Name, AuthMode: "AUTH_MODE_DISABLED"},
		{Name: r.Name, AuthMode: "AUTH_MODE_IAM_AUTH"},
		{Name: r.Name, AuthMode: "AUTH_MODE_TOKEN_AUTH", AdminPassword: "pw"},
	} {
		if u := c.iamUsers(r); u != nil {
			t.Errorf("iamUsers(%s, password %q) = %v, want none", r.AuthMode, r.AdminPassword, u)
		}
	}
	// No source: the default user's own password only, so every token is
	// refused.
	if u := (&clusters{now: time.Now}).iamUsers(r); !reflect.DeepEqual(u, map[string][]any{"default": {"on", "resetpass", ">pw"}}) {
		t.Errorf("iamUsers without a source = %v", u)
	}
	if fingerprint(got) == fingerprint(map[string][]any{"default": {"on", "resetpass", ">pw"}}) || fingerprint(got) != fingerprint(want) {
		t.Error("fingerprint does not tell the users apart")
	}
}
