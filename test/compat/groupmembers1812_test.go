//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"google.golang.org/protobuf/proto"
)

// waitUntil polls ok for up to a minute: a policy reaches the data
// planes asynchronously.
func waitUntil(ok func() bool) bool {
	deadline := time.Now().Add(time.Minute)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
	return true
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance
//
// TestMemorystoreIAMAuthGroupMember1812 (#1812), with the official
// memorystore/apiv1, resourcemanager/apiv3, iam/credentials/apiv1 and
// cloudidentity/v1 clients and go-redis: a Valkey instance with IAM auth
// takes the token of an account granted roles/memorystore.dbConnectionUser
// only through group:outer, which it is in through a nested group, and
// refuses it with WRONGPASS before the grant and after the nesting is
// removed.
func TestMemorystoreIAMAuthGroupMember1812(t *testing.T) {
	h := New(t)
	vc, project := valkeyAdminClient(t, h)
	svc := cloudIdentity(t, h, shardEndpoint(h, envMemorystoreResourceManager, EnvResourceManager))
	p := newIAMPrincipal(t, h, project, "vk-grp")
	sfx := time.Now().UnixNano() % 1e8
	outer, inner := groupFixture(t, svc, fmt.Sprintf("vk-outer-%d", sfx), fmt.Sprintf("vk-inner-%d", sfx))
	addMember(t, svc, inner.Name, p.email)
	nested := addMember(t, svc, outer.Name, inner.GroupKey.Id)

	parent := "projects/" + project + "/locations/us-central1"
	vid := fmt.Sprintf("compat-vkgrp-%d", sfx)
	vop, err := vc.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: vid, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}},
		AuthorizationMode:  memorystorepb.Instance_IAM_AUTH,
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := vc.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: parent + "/instances/" + vid}); err == nil {
			wctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_ = waitLRODone(wctx, op)
		}
	})
	inst, err := waitLRO(memorystoreWait(t), vop)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	addr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	ctx := h.Context()
	const wrongpass = "WRONGPASS"
	if err := authErr(ctx, addr, "", p.token); err == nil || !strings.Contains(err.Error(), wrongpass) {
		t.Fatalf("AUTH before any grant = %v, want %s", err, wrongpass)
	}
	change := projectRoles(t, h, project, "group:"+outer.GroupKey.Id)
	change(valkeyConnectRole, true)
	t.Cleanup(func() { change(valkeyConnectRole, false) })
	if !waitUntil(func() bool { return authErr(ctx, addr, "", p.token) == nil }) {
		t.Fatalf("AUTH with %s granted to group:%s: %v", valkeyConnectRole, outer.GroupKey.Id, authErr(ctx, addr, "", p.token))
	}
	if _, err := svc.Groups.Memberships.Delete(nested.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(func() bool { return authErr(ctx, addr, "", p.token) != nil }) {
		t.Errorf("AUTH after the inner group left the outer is still taken")
	}
}

// covers: google.firestore.admin.v1.FirestoreAdmin/CreateDatabase
//
// TestFirestoreMongoDBGroupMember1812 (#1812), with the official
// firestore/apiv1/admin, resourcemanager/apiv3, iam/credentials/apiv1 and
// cloudidentity/v1 clients and the MongoDB Go driver: an access-token
// sign-in of an account granted roles/datastore.user only through
// group:outer (through a nested group) inserts and reads, and is
// Unauthorized (13) before the grant and after the nesting is removed.
func TestFirestoreMongoDBGroupMember1812(t *testing.T) {
	h := New(t)
	rm := shardEndpoint(h, envFirestoreResourceManager, EnvResourceManager)
	ep, ca := mongoEndpoint(h)
	h.Endpoint(EnvFirestore)
	svc := cloudIdentity(t, h, rm)
	project := iamProject(t, h, "fm-grp-")
	p := newIAMPrincipal(t, h, project, "fm-grp")
	ctx := h.Context()
	sfx := time.Now().UnixNano() % 1e8
	outer, inner := groupFixture(t, svc, fmt.Sprintf("fm-outer-%d", sfx), fmt.Sprintf("fm-inner-%d", sfx))
	addMember(t, svc, inner.Name, p.email)
	nested := addMember(t, svc, outer.Name, inner.GroupKey.Id)

	admin := firestoreAdmin(t, h)
	id := fmt.Sprintf("mongo-grp-%d", sfx)
	op, err := admin.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: id,
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() {
		if op, err := admin.DeleteDatabase(context.Background(), &adminpb.DeleteDatabaseRequest{Name: db.Name}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	insert := func(key string) error {
		c := mongoConnect(t, mongoTokenURI(p.token, ep, id, ca)).Database(id).Collection("items")
		_, err := c.InsertOne(ctx, bson.D{{Key: "_id", Value: key}})
		return err
	}
	if err := insert("before"); mongoCode(err) != 13 {
		t.Fatalf("InsertOne before any grant = %v, want Unauthorized (13)", err)
	}
	change := projectRoles(t, h, project, "group:"+outer.GroupKey.Id)
	change("roles/datastore.user", true)
	t.Cleanup(func() { change("roles/datastore.user", false) })
	var n int
	if !waitUntil(func() bool { n++; return insert(fmt.Sprintf("granted-%d", n)) == nil }) {
		t.Fatalf("InsertOne with roles/datastore.user granted to group:%s: %v", outer.GroupKey.Id, insert("last"))
	}
	if _, err := svc.Groups.Memberships.Delete(nested.Name).Do(); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(func() bool { n++; return mongoCode(insert(fmt.Sprintf("after-%d", n))) == 13 }) {
		t.Errorf("InsertOne after the inner group left the outer is still allowed")
	}
}
