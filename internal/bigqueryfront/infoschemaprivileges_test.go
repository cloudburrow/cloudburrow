package bigqueryfront

import "testing"

func TestNamesObjectName(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT object_name FROM `region-us`.INFORMATION_SCHEMA.OBJECT_PRIVILEGES":                     false,
		"SELECT * FROM `region-us`.INFORMATION_SCHEMA.OBJECT_PRIVILEGES WHERE object_name = 'd'":       true,
		"SELECT * FROM `region-us`.INFORMATION_SCHEMA.OBJECT_PRIVILEGES WHERE `OBJECT_NAME` = 'd'":     true,
		"SELECT * FROM `region-us`.INFORMATION_SCHEMA.OBJECT_PRIVILEGES WHERE grantee = 'object_name'": false,
	} {
		if got := namesObjectName(sql); got != want {
			t.Errorf("namesObjectName(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestAccessGrantee(t *testing.T) {
	for _, c := range []struct {
		e    map[string]any
		want string
	}{
		{map[string]any{"userByEmail": "a@b.c"}, "user:a@b.c"},
		{map[string]any{"specialGroup": "projectOwners"}, "projectOwner"},
		{map[string]any{"iamMember": "allUsers"}, "allUsers"},
		{map[string]any{"view": map[string]any{"tableId": "v"}}, ""},
	} {
		if got := accessGrantee(c.e); got != c.want {
			t.Errorf("accessGrantee(%v) = %q, want %q", c.e, got, c.want)
		}
	}
}
