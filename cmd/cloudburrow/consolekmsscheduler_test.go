package main

import (
	"context"
	"strings"
	"testing"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	schedulerapi "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func clientOpts(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// Every service the CLI can enable is listed under a human title, not its
// config ID, which reads as a placeholder on the dashboard (#593).
func TestEveryServiceHasAHumanTitle(t *testing.T) {
	for _, s := range config.KnownServices() {
		if serviceTitle(s) == string(s) {
			t.Errorf("service %q has no human title", s)
		}
	}
}

// A key ring and key created from the console are the ones the official
// KMS client lists, and a version disabled from the console is DISABLED
// through the client: the provider acts through the same server (#593).
func TestConsoleKMSActsThroughTheAPIAnSDKSees(t *testing.T) {
	ctx := context.Background()
	svc := &kmsService{cfg: config.Config{BindAddress: "127.0.0.1"}, db: store.NewMemory()}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	p := kmsProvider{svc: svc}
	ring, err := p.Create(ctx, "demo-proj", map[string]string{"keyRingId": "console-ring", "location": "global"})
	if err != nil {
		t.Fatalf("create ring: %v", err)
	}
	if _, err := p.ActAtResult(ctx, "demo-proj", []string{ring}, "createkey", map[string]string{"cryptoKeyId": "k1"}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	c, err := kmsapi.NewKeyManagementClient(ctx, clientOpts(svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	it := c.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: "projects/demo-proj/locations/global"})
	var rings []string
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rings = append(rings, r.GetName())
	}
	if len(rings) != 1 || rings[0] != ring {
		t.Fatalf("the SDK lists %v; want the console's %s", rings, ring)
	}

	if err := p.ActAt(ctx, "demo-proj", []string{ring, "k1", "1"}, "disable", nil); err != nil {
		t.Fatalf("disable from the console: %v", err)
	}
	v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: ring + "/cryptoKeys/k1/cryptoKeyVersions/1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.GetState() != kmspb.CryptoKeyVersion_DISABLED {
		t.Errorf("the SDK sees %v; want DISABLED", v.GetState())
	}

	// A ring of another project is not opened from this one's page.
	if d, _ := p.Detail(ctx, "other-proj", []string{ring}); d.Unavailable == "" {
		t.Error("another project's ring opened from this project's page")
	}
}

// A job the official Scheduler client creates is listed by the console with
// its schedule and state, and pausing it from the console is what the
// client then reads (#593).
func TestConsoleSchedulerFollowsTheSDK(t *testing.T) {
	ctx := context.Background()
	svc := &schedulerService{cfg: config.Config{BindAddress: "127.0.0.1", Mode: config.ModeEphemeral}}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	c, err := schedulerapi.NewCloudSchedulerClient(ctx, clientOpts(svc.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := "projects/demo-proj/locations/us-central1/jobs/nightly"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: "projects/demo-proj/locations/us-central1", Job: &schedulerpb.Job{
		Name: name, Schedule: "0 3 * * *", TimeZone: "UTC",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://127.0.0.1:1/"}},
	}}); err != nil {
		t.Fatal(err)
	}
	p := schedulerProvider{svc: svc}
	l, err := p.List(ctx, "demo-proj")
	if err != nil {
		t.Fatal(err)
	}
	var row *console.Resource
	for i := range l.Items {
		if l.Items[i].Name == name {
			row = &l.Items[i]
		}
	}
	if row == nil || row.Fields["Schedule"] != "0 3 * * *" || !strings.EqualFold(row.Status, "enabled") {
		t.Fatalf("the console lists %+v; want the job with its schedule and ENABLED", l.Items)
	}
	if err := p.Act(ctx, "demo-proj", name, "pause"); err != nil {
		t.Fatalf("pause from the console: %v", err)
	}
	j, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if j.GetState() != schedulerpb.Job_PAUSED {
		t.Errorf("the SDK sees %v; want PAUSED", j.GetState())
	}
}
