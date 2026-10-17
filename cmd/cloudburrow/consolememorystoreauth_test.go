package main

import (
	"context"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

func sectionByID(d console.Detail, id string) *console.Section {
	for i := range d.Sections {
		if d.Sections[i].ID == id {
			return &d.Sections[i]
		}
	}
	return nil
}

func msHasAction(as []console.Action, id string) bool {
	for _, a := range as {
		if a.ID == id {
			return true
		}
	}
	return false
}

// TestConsoleMemorystoreTokenAuthBackups (#1549): the Memorystore for
// Valkey and Redis Cluster pages create a token auth instance and cluster;
// their pages offer Add token auth user, Add auth token, Back up now,
// Export and Delete backup (and Create, Update, Delete and Set ACL policy
// on a cluster, its ACL policies tab showing the attachment); a user and
// its generated token appear on the Token auth tab, a backup on the
// Backups tab, and Delete backup removes it.
func TestConsoleMemorystoreTokenAuthBackups(t *testing.T) {
	srv := redisadmin.NewServer(redisadmin.Instance{Project: "p", Host: "127.0.0.1", Port: 9016, Created: time.Unix(100, 0)}, valkeyNoBackend{})
	srv.EnableCreate(context.Background(), valkeyNoProv{}, func(string, string, string) redisadmin.DataPlane { return nil }, nil, nil)
	// Each API's nodes apart: the fake counts every running node as known.
	vnodes, cnodes := &fakeValkeyNodes{running: map[string]string{}}, &fakeValkeyNodes{running: map[string]string{}, next: 100}
	srv.EnableValkey(context.Background(), vnodes, func(addr, _, _ string) redisadmin.ClusterNode { return fakeValkeyNode{vnodes, addr} }, nil)
	srv.EnableClusters(context.Background(), cnodes, func(addr, _, _ string) redisadmin.ClusterNode { return fakeValkeyNode{cnodes, addr} }, nil)
	ctx := context.Background()
	type page interface {
		DetailActions(context.Context, string, []string) []console.Action
		ActAt(context.Context, string, []string, string, map[string]string) error
		Detail(context.Context, string, []string) (console.Detail, error)
	}
	vp := valkeyProvider{server: func() *redisadmin.Server { return srv }}
	cp := redisClusterProvider{svc: &memorystoreAdminService{api: srv}}
	if _, err := vp.Create(ctx, "p", map[string]string{"id": "vk-token", "region": redisadmin.Region, "mode": "CLUSTER", "shardCount": "1",
		"nodeType": "SHARED_CORE_NANO", "tokenAuth": "true"}); err != nil {
		t.Fatalf("Create instance: %v", err)
	}
	if _, err := cp.Create(ctx, "p", map[string]string{"id": "cl-token", "region": redisadmin.Region, "shardCount": "2",
		"nodeType": "REDIS_SHARED_CORE_NANO", "tokenAuth": "true"}); err != nil {
		t.Fatalf("Create cluster: %v", err)
	}
	for _, tc := range []struct {
		p       page
		id      string
		cluster bool
	}{{vp, "vk-token", false}, {cp, "cl-token", true}} {
		path := []string{tc.id}
		acts := tc.p.DetailActions(ctx, "p", path)
		for _, want := range []string{"add-token-user", "add-token", "delete-token", "delete-token-user", "backup", "export-backup", "delete-backup"} {
			if !msHasAction(acts, want) {
				t.Errorf("%s's page has no %s action", tc.id, want)
			}
		}
		if msHasAction(acts, "acl-policy") != tc.cluster {
			t.Errorf("%s's page acl-policy action = %v, want %v", tc.id, !tc.cluster, tc.cluster)
		}
		for _, a := range []struct {
			id     string
			values map[string]string
		}{{"add-token-user", map[string]string{"user": "app"}}, {"add-token", map[string]string{"user": "app"}}, {"backup", map[string]string{"backup": "first"}}} {
			if err := tc.p.ActAt(ctx, "p", path, a.id, a.values); err != nil {
				t.Fatalf("%s %s: %v", tc.id, a.id, err)
			}
		}
		d, err := tc.p.Detail(ctx, "p", path)
		if err != nil {
			t.Fatal(err)
		}
		tok, bk := sectionByID(d, "token-auth"), sectionByID(d, "backups")
		if tok == nil || len(tok.Listing.Items) != 1 || tok.Listing.Items[0].Fields["User"] != "app" || len(tok.Listing.Items[0].Fields["Token"]) < 16 {
			t.Errorf("%s's Token auth tab = %+v", tc.id, tok)
		}
		if bk == nil || len(bk.Listing.Items) != 1 || bk.Listing.Items[0].Name != "first" {
			t.Errorf("%s's Backups tab = %+v", tc.id, bk)
		}
		if tc.cluster {
			for _, a := range []struct {
				id     string
				values map[string]string
			}{{"create-acl-policy", map[string]string{"policyId": "readers", "rules": `{"reader":"on >pw ~app:* +@read"}`}},
				{"acl-policy", map[string]string{"policy": "projects/p/locations/" + redisadmin.Region + "/aclPolicies/readers"}},
				{"update-acl-policy", map[string]string{"policyId": "readers", "rules": `{"reader":"on >pw ~* +@read"}`}}} {
				if err := tc.p.ActAt(ctx, "p", path, a.id, a.values); err != nil {
					t.Fatalf("%s %s: %v", tc.id, a.id, err)
				}
			}
			d, _ := tc.p.Detail(ctx, "p", path)
			pol := sectionByID(d, "acl-policies")
			if pol == nil || len(pol.Listing.Items) != 1 || pol.Listing.Items[0].Fields["Attached to"] != tc.id || pol.Listing.Items[0].Fields["Revision"] != "2" {
				t.Errorf("%s's ACL policies tab = %+v", tc.id, pol)
			}
			if err := tc.p.ActAt(ctx, "p", path, "delete-acl-policy", map[string]string{"policyId": "readers"}); err == nil {
				t.Errorf("Delete ACL policy of an attached policy succeeded")
			}
			if err := tc.p.ActAt(ctx, "p", path, "acl-policy", map[string]string{"policy": ""}); err != nil {
				t.Fatal(err)
			}
			if err := tc.p.ActAt(ctx, "p", path, "delete-acl-policy", map[string]string{"policyId": "readers"}); err != nil {
				t.Errorf("Delete ACL policy: %v", err)
			}
		}
		if err := tc.p.ActAt(ctx, "p", path, "delete-backup", map[string]string{"backup": "first"}); err != nil {
			t.Fatalf("%s delete-backup: %v", tc.id, err)
		}
		if d, _ := tc.p.Detail(ctx, "p", path); len(sectionByID(d, "backups").Listing.Items) != 0 {
			t.Errorf("%s's Backups tab after Delete backup = %+v", tc.id, sectionByID(d, "backups").Listing.Items)
		}
	}
}
