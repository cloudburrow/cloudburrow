package secrets

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

var fakeStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func newClockStore(t *testing.T) (*Store, *sched.FakeClock) {
	t.Helper()
	db := store.NewMemory()
	t.Cleanup(func() { _ = db.Close() })
	clock := sched.NewFakeClock(fakeStart)
	return NewStoreOnClock(db, clock), clock
}

// A ttl is answered as the expire_time it gives; at that time the secret
// and its versions are gone, on the store's clock (#1225).
func TestSecretExpiresOnTheStoreClock(t *testing.T) {
	t.Parallel()
	s, clock := newClockStore(t)
	g := NewGRPCServer(s)
	ctx := context.Background()
	sec, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "short",
		Secret: &secretmanagerpb.Secret{Replication: automatic(), Expiration: &secretmanagerpb.Secret_Ttl{Ttl: durationpb.New(time.Hour)}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := sec.GetExpireTime().AsTime(); !got.Equal(fakeStart.Add(time.Hour)) {
		t.Fatalf("expire_time = %s, want now+ttl %s", got, fakeStart.Add(time.Hour))
	}
	if sec.GetTtl() != nil {
		t.Errorf("ttl is input only, but the response carries it")
	}
	if _, err := s.AddVersion("demo", "short", []byte("x")); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return clock.HasWaiterAt(fakeStart.Add(time.Hour)) })

	clock.Advance(59 * time.Minute)
	if _, err := s.GetSecret("demo", "short"); err != nil {
		t.Fatalf("before expire_time: %v", err)
	}
	clock.Advance(time.Minute)
	// Run deletes it from storage, not only from reads.
	waitFor(t, func() bool {
		keys, _ := s.db.List("")
		return len(keys) == 0
	})
	_, err = g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/demo/secrets/short"})
	wantGRPCCode(t, err, codes.NotFound)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

// A read sees an expired secret as gone even before Run reaches it.
func TestExpiredSecretIsGoneOnRead(t *testing.T) {
	t.Parallel()
	s, clock := newClockStore(t)
	if _, err := s.CreateSecretExpiring("demo", "a", nil, nil, fakeStart.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err := s.GetSecret("demo", "a"); err == nil {
		t.Fatal("an expired secret was returned")
	}
	if all, _ := s.ListSecrets("demo"); len(all) != 0 {
		t.Fatalf("list has %d secrets", len(all))
	}
	if _, err := s.CreateSecret("demo", "a", nil, nil, ""); err != nil {
		t.Fatalf("an expired ID is free again: %v", err)
	}
}

// UpdateSecret sets, moves and clears the expiry through its mask.
func TestUpdateSecretExpiry(t *testing.T) {
	t.Parallel()
	s, _ := newClockStore(t)
	g := NewGRPCServer(s)
	ctx := context.Background()
	if _, err := s.CreateSecret("demo", "a", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	name := "projects/demo/secrets/a"
	at := fakeStart.Add(48 * time.Hour)
	got, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: name, Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(at)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
	if err != nil || !got.GetExpireTime().AsTime().Equal(at) {
		t.Fatalf("set expire_time = %v, %v", got.GetExpireTime(), err)
	}
	got, err = g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: name, Expiration: &secretmanagerpb.Secret_Ttl{Ttl: durationpb.New(time.Hour)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl"}}})
	if err != nil || !got.GetExpireTime().AsTime().Equal(fakeStart.Add(time.Hour)) {
		t.Fatalf("set ttl = %v, %v", got.GetExpireTime(), err)
	}
	got, err = g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{Name: name}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
	if err != nil || got.GetExpireTime() != nil {
		t.Fatalf("clear = %v, %v", got.GetExpireTime(), err)
	}
	_, err = g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: name, Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(fakeStart.Add(-time.Hour))}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
	wantGRPCCode(t, err, codes.InvalidArgument)
}

// The REST resource carries expireTime, and a PATCH clears it.
func TestRESTExpiry(t *testing.T) {
	t.Parallel()
	srv, _ := newREST(t)
	code, body := do(t, srv, "POST", "/v1/projects/demo/secrets?secretId=a", `{"replication":{"automatic":{}},"expireTime":"2099-01-01T00:00:00Z"}`)
	if code != http.StatusOK || !strings.Contains(body, `"expireTime":"2099-01-01T00:00:00Z"`) {
		t.Fatalf("create = %d %s", code, body)
	}
	code, body = do(t, srv, "PATCH", "/v1/projects/demo/secrets/a?updateMask=expireTime", `{}`)
	if code != http.StatusOK || strings.Contains(body, "expireTime") {
		t.Fatalf("clear = %d %s", code, body)
	}
}

func secretNames(t *testing.T, g *GRPCServer, filter string) []string {
	t.Helper()
	resp, err := g.ListSecrets(context.Background(), &secretmanagerpb.ListSecretsRequest{Parent: "projects/demo", Filter: filter})
	if err != nil {
		t.Fatalf("filter %q: %v", filter, err)
	}
	if resp.GetTotalSize() != 0 {
		t.Errorf("filter %q: total_size = %d, Google reports none with a filter", filter, resp.GetTotalSize())
	}
	var out []string
	for _, s := range resp.GetSecrets() {
		out = append(out, s.GetName()[strings.LastIndex(s.GetName(), "/")+1:])
	}
	sort.Strings(out)
	return out
}

// The documented filter grammar, from the filtering page's examples (#1228).
func TestListSecretsFilters(t *testing.T) {
	t.Parallel()
	s, clock := newClockStore(t)
	g := NewGRPCServer(s)
	mk := func(id string, labels map[string]string, expire time.Time, repl string, locs ...string) {
		t.Helper()
		if _, err := s.CreateSecretExpiring("demo", id, labels, nil, expire, repl, locs...); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Hour)
	}
	mk("MyApp-db", map[string]string{"environment": "production"}, time.Time{}, "")
	mk("myapp-cache", map[string]string{"environment": "dev"}, fakeStart.Add(1000*time.Hour), "")
	mk("other", nil, time.Time{}, "user-managed", "us-central1")
	mk("west", nil, time.Time{}, "user-managed", "us-west1")

	cases := map[string]string{
		"name:myapp":                       "MyApp-db myapp-cache",
		`name:"MYAPP"`:                     "MyApp-db myapp-cache",
		"labels.environment=production":    "MyApp-db",
		"labels.environment:*":             "MyApp-db myapp-cache",
		"NOT labels.environment:*":         "other west",
		"-labels.environment:*":            "other west",
		"labels.environment!=production":   "myapp-cache other west",
		"labels:environment":               "MyApp-db myapp-cache",
		"create_time<2026-10-01T01:30:00Z": "MyApp-db myapp-cache",
		"create_time>2026-10-01T00:30:00Z AND create_time<2026-10-01T02:30:00Z": "myapp-cache other",
		"create_time>=2026-10-01T03:00:00Z":                                     "west",
		"expire_time:*":                                                         "myapp-cache",
		"expire_time<2027-01-01":                                                "myapp-cache",
		"replication.automatic:*":                                               "MyApp-db myapp-cache",
		"replication.user_managed.replicas.location:* AND NOT replication.user_managed.replicas.location:(us-central1 OR us-east1)": "west",
		"name:other OR name:west":           "other west",
		"name:myapp labels.environment=dev": "myapp-cache",
		"(name:other OR name:west) AND replication.user_managed.replicas.location:us-west1": "west",
		"NOT rotation.next_rotation_time:*":                                                 "MyApp-db myapp-cache other west",
		"rotation.rotation_period>259200s":                                                  "",
		"replication.user_managed.replicas.customerManagedEncryption:*":                     "",
	}
	for filter, want := range cases {
		if got := strings.Join(secretNames(t, g, filter), " "); got != want {
			t.Errorf("filter %q = [%s], want [%s]", filter, got, want)
		}
	}
	for _, bad := range []string{"name:(", "create_time<yesterday", `name:"x`, "AND name:x", "labels.env=*"} {
		_, err := g.ListSecrets(context.Background(), &secretmanagerpb.ListSecretsRequest{Parent: "projects/demo", Filter: bad})
		wantGRPCCode(t, err, codes.InvalidArgument)
	}
}

func TestListSecretVersionsFilters(t *testing.T) {
	t.Parallel()
	s, clock := newClockStore(t)
	g := NewGRPCServer(s)
	if _, err := s.CreateSecret("demo", "a", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		clock.Advance(time.Hour)
		if _, err := s.AddVersion("demo", "a", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetVersionState("demo", "a", "2", StateDisabled); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if _, err := s.DestroyVersion("demo", "a", "1"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"state:(ENABLED OR DISABLED)":                 "3 2",
		"state:DESTROYED AND destroy_time>2026-10-01": "1",
		"state=ENABLED":                               "3",
		"create_time>2026-10-01T01:30:00Z":            "3 2",
		"name:versions/3":                             "3",
	}
	for filter, want := range cases {
		resp, err := g.ListSecretVersions(context.Background(), &secretmanagerpb.ListSecretVersionsRequest{Parent: "projects/demo/secrets/a", Filter: filter})
		if err != nil {
			t.Fatalf("filter %q: %v", filter, err)
		}
		var got []string
		for _, v := range resp.GetVersions() {
			got = append(got, v.GetName()[strings.LastIndex(v.GetName(), "/")+1:])
		}
		if strings.Join(got, " ") != want {
			t.Errorf("filter %q = %v, want %s", filter, got, want)
		}
	}
	srv, rs := newREST(t)
	seed(t, rs, "demo", "a", "x", "y")
	if code, body := do(t, srv, "GET", "/v1/projects/demo/secrets/a/versions?filter=name%3Aversions%2F2", ""); code != http.StatusOK ||
		!strings.Contains(body, "versions/2") || strings.Contains(body, "versions/1\"") {
		t.Errorf("REST versions filter = %d %s", code, body)
	}
	if code, body := do(t, srv, "GET", "/v1/projects/demo/secrets?filter=name%3Anope", ""); code != http.StatusOK || strings.Contains(body, "projects/demo/secrets/a") {
		t.Errorf("REST secrets filter = %d %s", code, body)
	}
}

// On the Kubernetes backend the expiry survives a round trip, and the sweep
// deletes the Kubernetes Secret itself (#1225).
func TestKubeBackedSecretExpires(t *testing.T) {
	t.Parallel()
	fake := newFakeCluster()
	clock := sched.NewFakeClock(fakeStart)
	s := NewStoreOnClock(NewKubeStore(k8s.NewWith(fake, "", "", "cloudburrow"), "test"), clock)
	if _, err := s.CreateSecretExpiring("demo", "k", nil, nil, fakeStart.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddVersion("demo", "k", []byte("x")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSecret("demo", "k")
	if err != nil || !got.Expire.Equal(fakeStart.Add(time.Hour)) {
		t.Fatalf("expire after a Kubernetes round trip = %v, %v", got.Expire, err)
	}
	clock.Advance(time.Hour)
	if _, err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	if n := fake.names(); len(n) != 0 {
		t.Fatalf("Kubernetes Secrets left after expiry: %v", n)
	}
}
