package main

import (
	"context"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleCloudIdentitySearch2103: the Users and Devices pages' filter
// boxes take the Directory user query and the Mobile device search
// fields, and show a refused filter's error.
func TestConsoleCloudIdentitySearch2103(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	d := resourcemanager.OrganizationDomain
	for _, u := range []map[string]any{
		{"primaryEmail": "ann@" + d, "password": "password-1", "name": map[string]any{"givenName": "Ann", "familyName": "Lee"},
			"organizations": []any{map[string]any{"department": "Engineering", "title": "Manager"}}},
		{"primaryEmail": "bob@" + d, "password": "password-2", "name": map[string]any{"givenName": "Bob", "familyName": "Ray"}},
	} {
		if _, err := reg.InsertUser(u); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := reg.SignIn(resourcemanager.SignInInput{UserEmail: "ann@" + d, SerialNumber: "S1", DeviceType: "IOS", OSVersion: "iOS 17"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.SignIn(resourcemanager.SignInInput{UserEmail: "bob@" + d, SerialNumber: "S2", DeviceType: "ANDROID"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	users := ciUsersProvider{registry: reg}
	l, _ := users.ListFiltered(ctx, "", "orgDepartment=Engineering orgTitle:Manager")
	if l.ServerFilter == nil || len(l.Items) != 1 || l.Items[0].Name != "ann@"+d {
		t.Errorf("users filtered = %+v", l)
	}
	if l, _ := users.ListFiltered(ctx, "", "color=red"); l.ServerFilter.Error == "" || len(l.Items) != 0 {
		t.Errorf("a refused query = %+v", l)
	}
	devs := ciDevicesProvider{registry: reg}
	if l, _ := devs.ListFiltered(ctx, "", "os:ios name:Ann"); len(l.Items) != 1 || l.Items[0].Fields["Serial number"] != "S1" {
		t.Errorf("devices filtered = %+v", l)
	}
	if l, _ := devs.List(ctx, ""); len(l.Items) != 2 {
		t.Errorf("devices = %+v", l)
	}
	if l, _ := devs.ListFiltered(ctx, "", "color:red"); l.ServerFilter.Error == "" {
		t.Errorf("a refused filter = %+v", l)
	}
}
