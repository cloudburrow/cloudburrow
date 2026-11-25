//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestComputeZonalDiskGroupReplicationThroughTheGoClient (#2206), the
// official compute/apiv1 REST client: a zonal disk replicated to a
// secondary in us-east1-b, the secondary put in a consistency group of
// us-east1, the group cloned in its zone by disks.bulkInsert
// (sourceConsistencyGroupPolicy), and its replication stopped by
// disks.stopGroupAsyncReplication.
// covers: compute.disks.bulkInsert, compute.disks.stopGroupAsyncReplication, compute.disks.addResourcePolicies, compute.disks.removeResourcePolicies
func TestComputeZonalDiskGroupReplicationThroughTheGoClient(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	disks, err := computeapi.NewDisksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	pols, _ := computeapi.NewResourcePoliciesRESTClient(ctx, opts...)
	p := h.Project()
	const za, zs, region2 = "us-central1-a", "us-east1-b", "us-east1"
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e6)
	prim, sec, cg := "zg-prim-"+sfx, "zg-sec-"+sfx, "zg-cg-"+sfx
	wait := func(op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, d := range [][2]string{{zs, sec + "-clone"}, {zs, sec}, {za, prim}} {
			if op, err := disks.Delete(c, &computepb.DeleteDiskRequest{Project: p, Zone: d[0], Disk: d[1]}); err == nil {
				_ = op.Wait(c)
			}
		}
		if op, err := pols.Delete(c, &computepb.DeleteResourcePolicyRequest{Project: p, Region: region2, ResourcePolicy: cg}); err == nil {
			_ = op.Wait(c)
		}
	})
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: za, DiskResource: &computepb.Disk{
		Name: proto.String(prim), SizeGb: proto.Int64(10), Type: proto.String("zones/" + za + "/diskTypes/pd-balanced")}}))
	wait(disks.Insert(ctx, &computepb.InsertDiskRequest{Project: p, Zone: zs, DiskResource: &computepb.Disk{
		Name: proto.String(sec), Type: proto.String("zones/" + zs + "/diskTypes/pd-balanced"),
		AsyncPrimaryDisk: &computepb.DiskAsyncReplication{Disk: proto.String("projects/" + p + "/zones/" + za + "/disks/" + prim)}}}))
	wait(disks.StartAsyncReplication(ctx, &computepb.StartAsyncReplicationDiskRequest{Project: p, Zone: za, Disk: prim,
		DisksStartAsyncReplicationRequestResource: &computepb.DisksStartAsyncReplicationRequest{AsyncSecondaryDisk: proto.String("projects/" + p + "/zones/" + zs + "/disks/" + sec)}}))
	cgURL := "projects/" + p + "/regions/" + region2 + "/resourcePolicies/" + cg
	wait(pols.Insert(ctx, &computepb.InsertResourcePolicyRequest{Project: p, Region: region2, ResourcePolicyResource: &computepb.ResourcePolicy{
		Name: proto.String(cg), DiskConsistencyGroupPolicy: &computepb.ResourcePolicyDiskConsistencyGroupPolicy{}}}))
	wait(disks.AddResourcePolicies(ctx, &computepb.AddResourcePoliciesDiskRequest{Project: p, Zone: zs, Disk: sec,
		DisksAddResourcePoliciesRequestResource: &computepb.DisksAddResourcePoliciesRequest{ResourcePolicies: []string{cgURL}}}))
	wait(disks.BulkInsert(ctx, &computepb.BulkInsertDiskRequest{Project: p, Zone: zs,
		BulkInsertDiskResourceResource: &computepb.BulkInsertDiskResource{SourceConsistencyGroupPolicy: proto.String(cgURL)}}))
	if c, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zs, Disk: sec + "-clone"}); err != nil || c.GetStatus() != "READY" ||
		!strings.HasSuffix(c.GetSourceConsistencyGroupPolicy(), "/resourcePolicies/"+cg) {
		t.Fatalf("the clone of the zonal group: %v %v", c, err)
	}
	wait(disks.StopGroupAsyncReplication(ctx, &computepb.StopGroupAsyncReplicationDiskRequest{Project: p, Zone: zs,
		DisksStopGroupAsyncReplicationResourceResource: &computepb.DisksStopGroupAsyncReplicationResource{ResourcePolicy: proto.String(cgURL)}}))
	if s, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zs, Disk: sec}); err != nil || s.GetResourceStatus().GetAsyncPrimaryDisk().GetState() != "STOPPED" {
		t.Fatalf("the zonal secondary after stopGroupAsyncReplication: %v %v", s.GetResourceStatus(), err)
	}
	wait(disks.RemoveResourcePolicies(ctx, &computepb.RemoveResourcePoliciesDiskRequest{Project: p, Zone: zs, Disk: sec,
		DisksRemoveResourcePoliciesRequestResource: &computepb.DisksRemoveResourcePoliciesRequest{ResourcePolicies: []string{cgURL}}}))
	if s, err := disks.Get(ctx, &computepb.GetDiskRequest{Project: p, Zone: zs, Disk: sec}); err != nil || len(s.GetResourcePolicies()) != 0 {
		t.Errorf("the secondary's policies after removeResourcePolicies: %v %v", s.GetResourcePolicies(), err)
	}
}
