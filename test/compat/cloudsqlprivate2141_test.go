//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/proto"
)

// TestCloudSQLZonesPrivateIPAndPSCEndpoints (#2141), with Cloud SQL and
// Compute Engine on one instance, through the official clients
// (sqladmin/v1, compute/apiv1): an instance in a chosen zone and secondary
// zone, on a network's private services access range, answers its zones
// and a PRIVATE address of that range; an instance with Private Service
// Connect answers a service attachment, and a forwarding rule targeting
// it at a reserved internal address is a Private Service Connect endpoint,
// ACCEPTED for a project on the instance's allowed list and PENDING once
// the project is taken off it.
// covers: compute.forwardingRules.insert, compute.forwardingRules.get, compute.forwardingRules.list, compute.forwardingRules.aggregatedList, compute.forwardingRules.delete
func TestCloudSQLZonesPrivateIPAndPSCEndpoints(t *testing.T) {
	h := New(t)
	opts := sqlComputeOpts(t, h)
	svc := sqlAdminService(t, h)
	ctx := h.Context()
	p := h.Project()
	const region = "us-central1"
	gaddrs, _ := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	rules, err := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	rng := "ct-psa-" + suffix
	op, err := gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(rng), AddressType: proto.String("INTERNAL"), Purpose: proto.String("VPC_PEERING"),
		PrefixLength: proto.Int32(24), Network: proto.String("global/networks/default")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = gaddrs.Delete(ctx, &computepb.DeleteGlobalAddressRequest{Project: p, Address: rng}) })

	create := func(name string, ic *sqladmin.IpConfiguration, loc *sqladmin.LocationPreference, avail string) *sqladmin.DatabaseInstance {
		t.Helper()
		op, err := svc.Instances.Insert(p, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: region,
			Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", AvailabilityType: avail, IpConfiguration: ic, LocationPreference: loc}}).Do()
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			if d, err := svc.Instances.Delete(p, name).Do(); err == nil {
				waitSQLOp(t, svc, p, d)
			}
		})
		waitSQLOp(t, svc, p, op)
		in, err := svc.Instances.Get(p, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		return in
	}

	priv := create("ct-priv-"+suffix, &sqladmin.IpConfiguration{Ipv4Enabled: true, PrivateNetwork: "projects/" + p + "/global/networks/default",
		AllocatedIpRange: rng}, &sqladmin.LocationPreference{Zone: "us-central1-c", SecondaryZone: "us-central1-f"}, "REGIONAL")
	if priv.GceZone != "us-central1-c" || priv.SecondaryGceZone != "us-central1-f" {
		t.Errorf("zones %s, %s; want us-central1-c, us-central1-f", priv.GceZone, priv.SecondaryGceZone)
	}
	r, _ := gaddrs.Get(ctx, &computepb.GetGlobalAddressRequest{Project: p, Address: rng})
	var private string
	for _, a := range priv.IpAddresses {
		if a.Type == "PRIVATE" {
			private = a.IpAddress
		}
	}
	if private == "" || !strings.HasPrefix(private, strings.TrimSuffix(r.GetAddress(), ".0")) {
		t.Errorf("ipAddresses %+v; want a PRIVATE address of %s/24", priv.IpAddresses, r.GetAddress())
	}

	psc := create("ct-psc-"+suffix, &sqladmin.IpConfiguration{Ipv4Enabled: false, ForceSendFields: []string{"Ipv4Enabled"},
		PscConfig: &sqladmin.PscConfig{PscEnabled: true, AllowedConsumerProjects: []string{p}}}, nil, "")
	if psc.PscServiceAttachmentLink == "" {
		t.Fatal("no pscServiceAttachmentLink")
	}
	addr := "ct-psc-ep-" + suffix
	op, err = addrs.Insert(ctx, &computepb.InsertAddressRequest{Project: p, Region: region, AddressResource: &computepb.Address{
		Name: proto.String(addr), AddressType: proto.String("INTERNAL"), Subnetwork: proto.String("default")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	ep := "ct-psc-" + suffix
	op, err = rules.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(ep), Network: proto.String("projects/" + p + "/global/networks/default"), IPAddress: proto.String(addr),
		Target: proto.String(psc.PscServiceAttachmentLink)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := rules.Delete(ctx, &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep}); err == nil {
			_ = op.Wait(ctx)
		}
		_, _ = addrs.Delete(ctx, &computepb.DeleteAddressRequest{Project: p, Region: region, Address: addr})
	})
	fr, err := rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep})
	if err != nil || fr.GetPscConnectionStatus() != "ACCEPTED" {
		t.Fatalf("the endpoint: %v, %v; want ACCEPTED", fr, err)
	}
	listed := false
	it := rules.List(ctx, &computepb.ListForwardingRulesRequest{Project: p, Region: region})
	for {
		x, err := it.Next()
		if err != nil {
			break
		}
		listed = listed || x.GetName() == ep
	}
	agg := rules.AggregatedList(ctx, &computepb.AggregatedListForwardingRulesRequest{Project: p})
	inAgg := false
	for {
		x, err := agg.Next()
		if err != nil {
			break
		}
		for _, f := range x.Value.GetForwardingRules() {
			inAgg = inAgg || f.GetName() == ep
		}
	}
	if !listed || !inAgg {
		t.Errorf("the endpoint is listed %v, aggregated %v", listed, inAgg)
	}

	// Off the allowed list, the endpoint is PENDING.
	op2, err := svc.Instances.Patch(p, psc.Name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{
		Ipv4Enabled: false, ForceSendFields: []string{"Ipv4Enabled"},
		PscConfig: &sqladmin.PscConfig{PscEnabled: true, AllowedConsumerProjects: []string{"another-project"}}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, p, op2)
	fr, _ = rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep})
	if fr.GetPscConnectionStatus() != "PENDING" {
		t.Errorf("off the allowed list the endpoint is %s; want PENDING", fr.GetPscConnectionStatus())
	}
}

// TestCloudSQLPrivateIPAndPSCReachableFromAVM (#2141), with Cloud SQL and
// Compute Engine's VMs on one instance: a VM on the default network
// opens a TCP connection to a Cloud SQL instance's private IP on its
// port, and to a Private Service Connect endpoint of another instance,
// each reaching the instance's server (the startup script's lines read
// back from the serial port). Both instances are Enterprise Plus with
// managed connection pooling on, and the VM reaches each one's pooler on
// 6432 at the same addresses, as Google's private IP and Private Service
// Connect endpoints serve it: the pooler answers PostgreSQL's SSLRequest.
// A MySQL instance pooling connections on a private IP greets the VM on
// 3306 through its thread pool.
func TestCloudSQLPrivateIPAndPSCReachableFromAVM(t *testing.T) {
	h := New(t)
	opts := sqlComputeOpts(t, h)
	svc := sqlAdminService(t, h)
	ctx := h.Context()
	p := h.Project()
	const region, zone = "us-central1", "us-central1-a"
	gaddrs, _ := computeapi.NewGlobalAddressesRESTClient(ctx, opts...)
	addrs, _ := computeapi.NewAddressesRESTClient(ctx, opts...)
	rules, _ := computeapi.NewForwardingRulesRESTClient(ctx, opts...)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	rng := "ct-reach-" + suffix
	op, err := gaddrs.Insert(ctx, &computepb.InsertGlobalAddressRequest{Project: p, AddressResource: &computepb.Address{
		Name: proto.String(rng), AddressType: proto.String("INTERNAL"), Purpose: proto.String("VPC_PEERING"),
		PrefixLength: proto.Int32(24), Network: proto.String("global/networks/default")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	t.Cleanup(func() { _, _ = gaddrs.Delete(ctx, &computepb.DeleteGlobalAddressRequest{Project: p, Address: rng}) })
	create := func(name, version string, ic *sqladmin.IpConfiguration) *sqladmin.DatabaseInstance {
		t.Helper()
		op, err := svc.Instances.Insert(p, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: version, Region: region,
			Settings: &sqladmin.Settings{Tier: "db-perf-optimized-N-2", Edition: "ENTERPRISE_PLUS", IpConfiguration: ic,
				ConnectionPoolConfig: &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true}}}).Do()
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			if d, err := svc.Instances.Delete(p, name).Do(); err == nil {
				waitSQLOp(t, svc, p, d)
			}
		})
		waitSQLOp(t, svc, p, op)
		in, err := svc.Instances.Get(p, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	privateOf := func(in *sqladmin.DatabaseInstance) string {
		for _, a := range in.IpAddresses {
			if a.Type == "PRIVATE" {
				return a.IpAddress
			}
		}
		return ""
	}
	priv := create("ct-reach-priv-"+suffix, "POSTGRES_17", &sqladmin.IpConfiguration{Ipv4Enabled: false, ForceSendFields: []string{"Ipv4Enabled"},
		PrivateNetwork: "projects/" + p + "/global/networks/default", AllocatedIpRange: rng})
	privateIP := privateOf(priv)
	// MySQL's thread pool is on 3306 itself: the VM's connection to the
	// private IP is greeted by the server through it.
	my := create("ct-reach-my-"+suffix, "MYSQL_8_4", &sqladmin.IpConfiguration{Ipv4Enabled: false, ForceSendFields: []string{"Ipv4Enabled"},
		PrivateNetwork: "projects/" + p + "/global/networks/default", AllocatedIpRange: rng})
	myIP := privateOf(my)
	psc := create("ct-reach-psc-"+suffix, "POSTGRES_17", &sqladmin.IpConfiguration{Ipv4Enabled: false, ForceSendFields: []string{"Ipv4Enabled"},
		PscConfig: &sqladmin.PscConfig{PscEnabled: true, AllowedConsumerProjects: []string{p}}})
	ep := "ct-reach-ep-" + suffix
	op, err = rules.Insert(ctx, &computepb.InsertForwardingRuleRequest{Project: p, Region: region, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(ep), Network: proto.String("global/networks/default"), Target: proto.String(psc.PscServiceAttachmentLink)}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op.Wait(ctx)
	t.Cleanup(func() {
		if op, err := rules.Delete(ctx, &computepb.DeleteForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	fr, err := rules.Get(ctx, &computepb.GetForwardingRuleRequest{Project: p, Region: region, ForwardingRule: ep})
	if err != nil {
		t.Fatal(err)
	}
	pscIP := fr.GetIPAddress()
	_ = addrs

	vm := "ct-reach-vm-" + suffix
	script := fmt.Sprintf("#!/bin/bash\nfor i in $(seq 1 40); do\n"+
		"  if timeout 3 bash -c '</dev/tcp/%s/5432' 2>/dev/null; then echo PRIVATE-IP-REACHED; break; fi; sleep 2\ndone\n"+
		"for i in $(seq 1 40); do\n"+
		"  if timeout 3 bash -c '</dev/tcp/%s/5432' 2>/dev/null; then echo PSC-ENDPOINT-REACHED; break; fi; sleep 2\ndone\n"+
		// The pooler: PostgreSQL's SSLRequest, which PgBouncer without
		// client TLS answers N.
		"pooler() { timeout 5 bash -c \"exec 3<>/dev/tcp/$1/6432; printf '\\\\x00\\\\x00\\\\x00\\\\x08\\\\x04\\\\xd2\\\\x16\\\\x2f' >&3; head -c1 <&3\" 2>/dev/null; }\n"+
		"for i in $(seq 1 40); do\n"+
		"  if [ \"$(pooler %s)\" = N ]; then echo PRIVATE-IP-POOLER-REACHED; break; fi; sleep 2\ndone\n"+
		"for i in $(seq 1 40); do\n"+
		"  if [ \"$(pooler %s)\" = N ]; then echo PSC-ENDPOINT-POOLER-REACHED; break; fi; sleep 2\ndone\n"+
		"for i in $(seq 1 40); do\n"+
		"  timeout 3 bash -c 'exec 3<>/dev/tcp/%s/3306; cat <&3' > /tmp/greeting 2>/dev/null\n"+
		"  if tr -d '\\000' < /tmp/greeting | grep -q caching_sha2_password; then echo MYSQL-THREAD-POOL-REACHED; break; fi; sleep 2\ndone\n"+
		"nft list chain inet cloudburrow private-services 2>&1 | sed 's/^/NFT: /'\necho REACH-DONE\n", privateIP, pscIP, privateIP, pscIP, myIP)
	op, err = insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(vm),
		MachineType: proto.String("zones/" + zone + "/machineTypes/e2-small"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"), Value: proto.String(script)}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := insts.Delete(ctx, &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: vm}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Minute)
	// The serial port is read on from where the last read ended (next).
	var serial string
	var next int64
	for time.Now().Before(deadline) {
		out, err := insts.GetSerialPortOutput(ctx, &computepb.GetSerialPortOutputInstanceRequest{Project: p, Zone: zone, Instance: vm,
			Start: proto.Int64(next)})
		if err == nil {
			serial += out.GetContents()
			next = out.GetNext()
			if strings.Contains(serial, "REACH-DONE") {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	for _, w := range []string{"PRIVATE-IP-REACHED", "PSC-ENDPOINT-REACHED", "PRIVATE-IP-POOLER-REACHED", "PSC-ENDPOINT-POOLER-REACHED",
		"MYSQL-THREAD-POOL-REACHED"} {
		if !strings.Contains(serial, w) {
			tail := serial
			if len(tail) > 3000 {
				tail = tail[len(tail)-3000:]
			}
			t.Errorf("the VM's serial port lacks %s (private IP %s, endpoint %s): ...%s", w, privateIP, pscIP, tail)
		}
	}
}
