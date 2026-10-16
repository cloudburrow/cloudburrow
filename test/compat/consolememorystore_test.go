//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestConsoleMemorystoreFlush (#1302): the Memorystore page lists the one
// instance as the admin API describes it, offers Flush, and Flush empties
// the server a Redis client reaches. The restart probe key
// TestMemorystoreAcrossRestart reads is put back afterwards.
func TestConsoleMemorystoreFlush(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	project := strings.TrimSpace(os.Getenv(EnvMemorystoreProject))
	if project == "" {
		t.Skipf("%s is not set", EnvMemorystoreProject)
	}
	rc := redisClient(t, h)
	ctx := h.Context()

	code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/memorystore?project="+url.QueryEscape(project), "")
	if code != http.StatusOK {
		t.Fatalf("console list = %d: %s", code, body)
	}
	var l struct {
		Items []struct {
			Name    string
			Status  string
			Fields  map[string]string
			Actions []struct{ ID string }
		}
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	// The instance `up` runs, beside any that CreateInstance made.
	i := -1
	for n, it := range l.Items {
		if it.Name == "memorystore" {
			i = n
		}
	}
	if i < 0 || l.Items[i].Status != "READY" ||
		l.Items[i].Fields["Endpoint"] != h.Endpoint(EnvMemorystore) || l.Items[i].Fields["Version"] != "REDIS_7_2" {
		t.Fatalf("console list = %s", body)
	}
	if len(l.Items[i].Actions) != 1 || l.Items[i].Actions[0].ID != "flush" {
		t.Fatalf("instance actions = %+v, want flush", l.Items[i].Actions)
	}

	probe, probeErr := rc.Get(ctx, restartProbeKey).Result()
	t.Cleanup(func() {
		if probeErr == nil {
			_ = rc.Set(ctx, restartProbeKey, probe, 0).Err()
		}
	})
	key := "compat:" + h.Project() + ":flush"
	if err := rc.Set(ctx, key, "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	code, body = consoleDo(t, addr, http.MethodPost, "/api/actions/memorystore?project="+url.QueryEscape(project),
		fmt.Sprintf(`{"Name":%q,"Action":"flush"}`, "memorystore"))
	if code != http.StatusOK {
		t.Fatalf("flush = %d: %s", code, body)
	}
	if _, err := rc.Get(ctx, key).Result(); err != redis.Nil {
		t.Errorf("GET after Flush = %v, want no key", err)
	}
	if n, err := rc.DBSize(ctx).Result(); err != nil || n != 0 {
		t.Errorf("DBSIZE after Flush = %d, %v; want 0", n, err)
	}

	// Another project has no instance, so nothing to flush.
	code, _ = consoleDo(t, addr, http.MethodPost, "/api/actions/memorystore?project="+h.Project(),
		`{"Name":"memorystore","Action":"flush"}`)
	if code == http.StatusOK {
		t.Errorf("flush in a project with no instance succeeded")
	}
}

// TestConsoleMemorystoreCreateEditDelete (#1302): Create instance on the
// Memorystore page makes an instance with AUTH through CreateInstance,
// which the official client reads back and go-redis reaches with the AUTH
// string its page shows; Edit resizes it and changes its labels through
// UpdateInstance; Delete removes it through DeleteInstance. The instance
// `up` runs has no Edit form, and cannot be deleted.
func TestConsoleMemorystoreCreateEditDelete(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("console-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	q := "?project=" + url.QueryEscape(project)
	body, _ := json.Marshal(map[string]string{"id": id, "region": "us-central1", "memorySizeGb": "1", "authEnabled": "true",
		"displayName": "console cache", "labels": `{"team":"a"}`, "redisConfigs": `{"maxmemory-policy":"allkeys-lru"}`})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/memorystore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance after Create instance: %v", err)
	}
	if inst.GetState() != redispb.Instance_READY || !inst.GetAuthEnabled() || inst.GetDisplayName() != "console cache" ||
		inst.GetLabels()["team"] != "a" || inst.GetRedisConfigs()["maxmemory-policy"] != "allkeys-lru" {
		t.Errorf("created from the console: %v", inst)
	}

	var page struct {
		Sections []struct {
			Groups []struct {
				Properties []struct{ Label, Value string }
			}
		}
		Edit *struct{ Label string }
	}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, "")
	if code != http.StatusOK {
		t.Fatalf("instance page = %d: %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for _, s := range page.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				props[p.Label] = p.Value
			}
		}
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil || props["AUTH string"] != auth.GetAuthString() || page.Edit == nil {
		t.Fatalf("instance page = %s (AUTH string %v)", out, err)
	}
	if props["Endpoint"] != net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))) {
		t.Errorf("page endpoint %q, instance %s:%d", props["Endpoint"], inst.GetHost(), inst.GetPort())
	}
	rc := redis.NewClient(&redis.Options{Addr: props["Endpoint"], Password: props["AUTH string"]})
	defer rc.Close()
	if err := rc.Set(ctx, "console", "1", 0).Err(); err != nil {
		t.Errorf("SET with the page's endpoint and AUTH string: %v", err)
	}

	if code, out := consoleEdit(t, addr, "memorystore", project, []string{id}, map[string]string{
		"displayName": "resized", "memorySizeGb": "2", "labels": `{"team":"b"}`, "redisConfigs": `{"maxmemory-policy":"allkeys-lru"}`}); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, out)
	}
	if inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err != nil || inst.GetMemorySizeGb() != 2 ||
		inst.GetLabels()["team"] != "b" || inst.GetDisplayName() != "resized" {
		t.Errorf("after Edit, GetInstance = %v, %v", inst, err)
	}
	if cfg, err := rc.ConfigGet(ctx, "maxmemory").Result(); err != nil || cfg["maxmemory"] != strconv.Itoa(2<<30) {
		t.Errorf("after Edit, maxmemory = %v, %v", cfg, err)
	}

	// The instance `up` runs: no Edit form, and Delete is refused.
	code, out = consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name=memorystore", "")
	if code != http.StatusOK || strings.Contains(out, `"edit"`) {
		t.Errorf("the page of the instance up runs = %d: %s", code, out)
	}
	if code, _ := consoleDo(t, addr, http.MethodDelete, "/api/resources/memorystore"+q+"&name=memorystore", ""); code == http.StatusOK {
		t.Error("Delete of the instance up runs succeeded")
	}

	if code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/memorystore"+q+"&name="+id, ""); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, out)
	}
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after Delete = %v, want NOT_FOUND", err)
	}
}

// TestConsoleMemorystoreImportExport (#1384): an instance's page offers
// Export and Import; Export writes its data to a gs:// RDB file in this
// instance's Cloud Storage through ExportInstance, and Import of that file
// after changes puts the exported keys back through ImportInstance, each
// waiting for its operation. Edit's Persistence turns on RDB snapshots
// through UpdateInstance's persistence_config.
func TestConsoleMemorystoreImportExport(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	bh := bucket(t, h, instanceStorage(t, h))
	ctx := h.Context()
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("console-io-%d", time.Now().UnixNano()%1e8)
	inst, rc := createBasic(t, h, c, parent, id, nil)
	rc.Set(ctx, "k", "v1", 0)

	q := "?project=" + url.QueryEscape(project)
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, "")
	if code != http.StatusOK || !strings.Contains(out, `"id":"export"`) || !strings.Contains(out, `"id":"import"`) {
		t.Fatalf("instance page = %d: %s; want Export and Import", code, out)
	}
	uri := "gs://" + bh.BucketName() + "/console.rdb"
	if code, out := consoleAct(t, addr, "memorystore", project, []string{id}, "export", map[string]string{"uri": uri}); code != http.StatusOK {
		t.Fatalf("console Export = %d: %s", code, out)
	}
	if _, err := bh.Object("console.rdb").Attrs(ctx); err != nil {
		t.Fatalf("the exported object: %v", err)
	}
	rc.Set(ctx, "k", "changed", 0)
	if code, out := consoleAct(t, addr, "memorystore", project, []string{id}, "import", map[string]string{"uri": uri}); code != http.StatusOK {
		t.Fatalf("console Import = %d: %s", code, out)
	}
	if v, err := rc.Get(ctx, "k").Result(); err != nil || v != "v1" {
		t.Errorf("after console Import GET k = %q, %v; want v1", v, err)
	}

	if code, out := consoleEdit(t, addr, "memorystore", project, []string{id}, map[string]string{
		"displayName": "", "memorySizeGb": "1", "labels": "{}", "redisConfigs": "{}", "persistence": "ONE_HOUR"}); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, out)
	}
	got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: inst.GetName()})
	if err != nil || got.GetPersistenceConfig().GetPersistenceMode() != redispb.PersistenceConfig_RDB ||
		got.GetPersistenceConfig().GetRdbSnapshotPeriod() != redispb.PersistenceConfig_ONE_HOUR {
		t.Errorf("after Edit's Persistence, GetInstance = %v, %v", got.GetPersistenceConfig(), err)
	}
}

// TestConsoleMemorystoreUpgrade (#1386): Create instance's Version makes a
// REDIS_7_0 instance, whose page offers Upgrade to the later versions
// only; Upgrade to REDIS_7_2 runs UpgradeInstance and waits for it, and
// the instance keeps its keys.
func TestConsoleMemorystoreUpgrade(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("console-up-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	q := "?project=" + url.QueryEscape(project)
	body, _ := json.Marshal(map[string]string{"id": id, "region": "us-central1", "memorySizeGb": "1", "redisVersion": "REDIS_7_0"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/memorystore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetRedisVersion() != "REDIS_7_0" {
		t.Fatalf("created from the console: %v, %v", inst, err)
	}
	rc := redisAt(inst, "")
	rc.Set(ctx, "k", "v", 0)
	rc.Close()
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, "")
	if code != http.StatusOK || !strings.Contains(out, `"id":"upgrade"`) || strings.Contains(out, `"REDIS_6_X"`) {
		t.Fatalf("instance page = %d: %s; want Upgrade to later versions only", code, out)
	}
	if code, out := consoleAct(t, addr, "memorystore", project, []string{id}, "upgrade", map[string]string{"redisVersion": "REDIS_7_2"}); code != http.StatusOK {
		t.Fatalf("console Upgrade = %d: %s", code, out)
	}
	inst, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetRedisVersion() != "REDIS_7_2" {
		t.Fatalf("after Upgrade: %v, %v", inst, err)
	}
	rc = redisAt(inst, "")
	defer rc.Close()
	if v, err := rc.Get(ctx, "k").Result(); err != nil || v != "v" {
		t.Errorf("after Upgrade GET k = %q, %v", v, err)
	}
}

// TestConsoleMemorystoreStandardTier (#1385): Create instance's Tier,
// read replicas and Replicas make a STANDARD_HA instance with two
// replicas, whose page shows its read endpoint, zones and nodes and
// offers Failover; Failover runs FailoverInstance and waits for it, and
// the endpoint keeps the data on a new primary; Edit's Replicas scales
// the instance through UpdateInstance.
func TestConsoleMemorystoreStandardTier(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("console-ha-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	q := "?project=" + url.QueryEscape(project)
	body, _ := json.Marshal(map[string]string{"id": id, "region": "us-central1", "memorySizeGb": "5",
		"tier": "STANDARD_HA", "readReplicas": "true", "replicaCount": "2"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/memorystore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetTier() != redispb.Instance_STANDARD_HA || inst.GetReplicaCount() != 2 ||
		inst.GetReadReplicasMode() != redispb.Instance_READ_REPLICAS_ENABLED {
		t.Fatalf("created from the console: %v, %v", inst, err)
	}
	rc := redisAt(inst, "")
	defer rc.Close()
	if err := rc.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if n, err := rc.Do(ctx, "WAIT", 2, 5000).Int(); err != nil || n < 2 {
		t.Fatalf("WAIT 2 = %d, %v", n, err)
	}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, "")
	if code != http.StatusOK || !strings.Contains(out, `"id":"failover"`) || !strings.Contains(out, "Read endpoint") ||
		!strings.Contains(out, inst.GetReadEndpoint()) || !strings.Contains(out, "node-2") {
		t.Fatalf("instance page = %d: %s; want Failover, the read endpoint and the nodes", code, out)
	}
	if code, out := consoleAct(t, addr, "memorystore", project, []string{id}, "failover",
		map[string]string{"dataProtectionMode": "LIMITED_DATA_LOSS"}); code != http.StatusOK {
		t.Fatalf("console Failover = %d: %s", code, out)
	}
	after, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || after.GetCurrentLocationId() == inst.GetCurrentLocationId() || after.GetPort() != inst.GetPort() {
		t.Fatalf("after console Failover: %v, %v", after, err)
	}
	if v, err := rc.Get(ctx, "k").Result(); err != nil || v != "v" {
		t.Errorf("after console Failover GET k = %q, %v", v, err)
	}
	if code, out := consoleEdit(t, addr, "memorystore", project, []string{id}, map[string]string{
		"displayName": "", "memorySizeGb": "5", "labels": "{}", "redisConfigs": "{}", "replicaCount": "1"}); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, out)
	}
	if got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err != nil || got.GetReplicaCount() != 1 || len(got.GetNodes()) != 2 {
		t.Errorf("after Edit's Replicas, GetInstance = %v, %v", got, err)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestConsoleMemorystoreInTransitEncryption (#1413): the console's Create
// instance with Enable in-transit encryption creates a
// SERVER_AUTHENTICATION instance, which the official client reads back;
// its page shows In-transit encryption and a Server CA tab with the CA
// server_ca_certs lists, which a go-redis TLS client trusts.
func TestConsoleMemorystoreInTransitEncryption(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("console-tls-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	q := "?project=" + url.QueryEscape(project)
	body, _ := json.Marshal(map[string]string{"id": id, "region": "us-central1", "memorySizeGb": "1", "tier": "BASIC",
		"transitEncryption": "true"})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/memorystore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetTransitEncryptionMode() != redispb.Instance_SERVER_AUTHENTICATION || len(inst.GetServerCaCerts()) != 1 {
		t.Fatalf("created from the console: %v, %v", inst, err)
	}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, "")
	var detail struct {
		Sections []struct {
			ID     string `json:"id"`
			Text   string `json:"text"`
			Groups []struct {
				Properties []struct{ Label, Value string } `json:"properties"`
			} `json:"groups"`
		} `json:"sections"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(out), &detail) != nil {
		t.Fatalf("instance page = %d: %s", code, out)
	}
	mode, caText := "", ""
	for _, s := range detail.Sections {
		if s.ID == "server-ca" {
			caText = s.Text
		}
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				if p.Label == "In-transit encryption" {
					mode = p.Value
				}
			}
		}
	}
	if !strings.HasPrefix(mode, "Server authentication") {
		t.Errorf("the page's In-transit encryption = %q", mode)
	}
	ca := inst.GetServerCaCerts()[0]
	if !strings.Contains(caText, strings.TrimSpace(ca.GetCert())) || !strings.Contains(caText, ca.GetSha1Fingerprint()) {
		t.Fatalf("the Server CA tab = %q; want the instance's CA", caText)
	}
	rc := tlsRedisAt(t, inst, "")
	defer rc.Close()
	if err := rc.Ping(ctx).Err(); err != nil {
		t.Errorf("PING over TLS with the CA the page shows: %v", err)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.kms.v1.KeyManagementService/UpdateCryptoKeyVersion
//
// TestConsoleMemorystoreCustomerManagedKey (#1463): the console's Create
// instance with a customer-managed key creates an instance with it, which
// the official client reads back; its page shows the key as Encryption,
// and, once the official KMS client disables the key's version, the
// suspension reason CUSTOMER_MANAGED_KEY_ISSUE.
func TestConsoleMemorystoreCustomerManagedKey(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	kc := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	key := memorystoreKey(t, h, project)
	id := fmt.Sprintf("console-cmek-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	q := "?project=" + url.QueryEscape(project)
	body, _ := json.Marshal(map[string]string{"id": id, "region": "us-central1", "memorySizeGb": "1", "tier": "BASIC",
		"customerManagedKey": key})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/memorystore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetCustomerManagedKey() != key {
		t.Fatalf("created from the console: %v, %v", inst, err)
	}
	if code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, ""); code != http.StatusOK ||
		!strings.Contains(out, "Customer-managed key "+key) {
		t.Fatalf("instance page = %d: %s; want the customer-managed key", code, out)
	}
	if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, "")
		if code == http.StatusOK && strings.Contains(out, "Suspension reasons") && strings.Contains(out, "CUSTOMER_MANAGED_KEY_ISSUE") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance page with the key disabled = %d: %s", code, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestConsoleMemorystoreRotateServerCA (#1463): Rotate server CA on the
// page of an instance with in-transit encryption (Memorystore's rotation
// on request) lists a second CA in server_ca_certs, which the official
// client reads back, and the server then serves a certificate of the new
// CA, which a go-redis TLS client trusting only the new CA verifies.
func TestConsoleMemorystoreRotateServerCA(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("console-rot-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/us-central1", InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := "?project=" + url.QueryEscape(project)
	if code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, ""); code != http.StatusOK || !strings.Contains(out, `"id":"rotate-ca"`) {
		t.Fatalf("instance page = %d: %s; want Rotate server CA", code, out)
	}
	if code, out := consoleAct(t, addr, "memorystore", project, []string{id}, "rotate-ca", nil); code != http.StatusOK {
		t.Fatalf("console Rotate server CA = %d: %s", code, out)
	}
	after, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || len(after.GetServerCaCerts()) != 2 || after.GetServerCaCerts()[1].GetCert() != before.GetServerCaCerts()[0].GetCert() {
		t.Fatalf("after rotation, server_ca_certs = %v, %v", after.GetServerCaCerts(), err)
	}
	onlyNew := &redispb.Instance{ServerCaCerts: after.GetServerCaCerts()[:1]}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		cur, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err == nil && cur.GetState() == redispb.Instance_READY {
			onlyNew.Host, onlyNew.Port = cur.GetHost(), cur.GetPort()
			rc := tlsRedisAt(t, onlyNew, "")
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = rc.Ping(pctx).Err()
			cancel()
			_ = rc.Close()
			if err == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server does not serve a certificate of the new CA: %v", err)
		}
		time.Sleep(time.Second)
	}
}
