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
	logadmin "cloud.google.com/go/logging/logadmin"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestComputeOpsAgentWritesLogsAndMetrics (#2139): a Debian 12 VM runs
// Google's Ops Agent, installed at the pinned version in the VM image and
// fenced to this instance's Cloud Logging and Cloud Monitoring. A line its
// startup-script writes with logger(1) reaches syslog (rsyslog's
// /var/log/syslog, the agent's default receiver) and, through Fluent Bit's
// stackdriver output, Cloud Logging as projects/P/logs/syslog on the
// gce_instance resource of the VM's ID and zone, read back with the
// official logadmin client; the agent's host metrics (the OpenTelemetry
// Collector's googlecloud exporter) reach Cloud Monitoring as
// agent.googleapis.com/ time series of the same gce_instance (memory/bytes_used,
// as ListTimeSeries filters name a single metric type), listed with the
// official MetricClient.
func TestComputeOpsAgentWritesLogsAndMetrics(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	logAddr := h.Endpoint(EnvLogging)
	monAddr := h.Endpoint(EnvMonitoring)
	if logAddr == "" || monAddr == "" {
		t.Fatal("no Cloud Logging or Cloud Monitoring endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	insts, err := computeapi.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { insts.Close() })
	p := h.Project()
	const zone = "us-central1-a"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	name := "ct-opsagent-" + sfx
	marker := "cloudburrow-ops-agent-" + sfx
	script := "#!/bin/bash\nfor i in $(seq 1 60); do logger -t ct-opsagent " + marker + "; sleep 10; done &\n"
	op, err := insts.Insert(ctx, &computepb.InsertInstanceRequest{Project: p, Zone: zone, InstanceResource: &computepb.Instance{
		Name:        proto.String(name),
		MachineType: proto.String("zones/" + zone + "/machineTypes/e2-medium"),
		Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String("projects/debian-cloud/global/images/family/debian-12")}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Network: proto.String("global/networks/default")}},
		Metadata:          &computepb.Metadata{Items: []*computepb.Items{{Key: proto.String("startup-script"), Value: proto.String(script)}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := insts.Delete(context.Background(), &computepb.DeleteInstanceRequest{Project: p, Zone: zone, Instance: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	vm, err := insts.Get(ctx, &computepb.GetInstanceRequest{Project: p, Zone: zone, Instance: name})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(vm.GetId())

	ac, err := logadmin.NewClient(ctx, "projects/"+p, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ac.Close() })
	filter := `logName="projects/` + p + `/logs/syslog" AND resource.type="gce_instance" AND resource.labels.instance_id="` + id + `"`
	var found bool
	for deadline := time.Now().Add(10 * time.Minute); !found && time.Now().Before(deadline); time.Sleep(10 * time.Second) {
		it := ac.Entries(ctx, logadmin.Filter(filter), logadmin.NewestFirst())
		for n := 0; n < 200; n++ {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("Entries: %v", err)
			}
			if strings.Contains(fmt.Sprint(e.Payload), marker) {
				if z := e.Resource.GetLabels()["zone"]; z != zone {
					t.Errorf("the entry's zone label is %q, want %q", z, zone)
				}
				found = true
				break
			}
		}
	}
	if !found {
		out, _ := insts.GetSerialPortOutput(ctx, &computepb.GetSerialPortOutputInstanceRequest{Project: p, Zone: zone, Instance: name})
		t.Fatalf("no syslog entry with %q from the Ops Agent; serial port:\n%s", marker, out.GetContents())
	}

	mc, err := monitoring.NewMetricClient(ctx, option.WithEndpoint(monAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mc.Close() })
	var metric string
	for deadline := time.Now().Add(10 * time.Minute); metric == "" && time.Now().Before(deadline); time.Sleep(15 * time.Second) {
		now := time.Now()
		it := mc.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{Name: "projects/" + p,
			Filter:   `metric.type = "agent.googleapis.com/memory/bytes_used" AND resource.type = "gce_instance" AND resource.labels.instance_id = "` + id + `"`,
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(now.Add(-15 * time.Minute)), EndTime: timestamppb.New(now)},
			View:     monitoringpb.ListTimeSeriesRequest_HEADERS})
		ts, err := it.Next()
		if err == nil {
			metric = ts.GetMetric().GetType()
		} else if err != iterator.Done {
			t.Fatalf("ListTimeSeries: %v", err)
		}
	}
	if metric == "" {
		t.Fatal("no agent.googleapis.com/memory/bytes_used time series of the VM in Cloud Monitoring")
	}
	t.Logf("the Ops Agent wrote %s", metric)
}
