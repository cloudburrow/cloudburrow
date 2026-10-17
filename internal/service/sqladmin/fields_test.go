package sqladmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// discoverySchemas reads the schemas of the pinned module's sqladmin v1
// discovery document, the one methods.go is built from.
func discoverySchemas(t *testing.T) map[string]map[string]json.RawMessage {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "google.golang.org/api").Output()
	if err != nil {
		t.Skipf("cannot locate google.golang.org/api: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "sqladmin", "v1", "sqladmin-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Schemas map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schemas"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	s := map[string]map[string]json.RawMessage{}
	for k, v := range d.Schemas {
		s[k] = v.Properties
	}
	return s
}

// fieldTables are the request bodies each table classifies, by schema.
var fieldTables = map[string]map[string]fieldRule{
	"DatabaseInstance": instanceFields, "Settings": settingsFields, "User": userFields,
	"Database": databaseFields, "BackupRun": backupRunFields, "Backup": backupFields,
	"SslCertsInsertRequest": sslCertFields,
}

// Every property of every request body served is classified, and nothing
// that is not a property is.
func TestSQLFieldTablesMatchTheDiscoveryDocument(t *testing.T) {
	schemas := discoverySchemas(t)
	for schema, rules := range fieldTables {
		props := schemas[schema]
		if len(props) == 0 {
			t.Errorf("the discovery document has no schema %s", schema)
		}
		for k := range props {
			if _, ok := rules[k]; !ok {
				t.Errorf("%s.%s is in the discovery document but not classified: apply it, record it, or refuse it", schema, k)
			}
		}
		for k, r := range rules {
			if _, ok := props[k]; !ok {
				t.Errorf("%s.%s is classified but not in the discovery document", schema, k)
			}
			if r.how == fieldRefused && r.refuse == nil {
				t.Errorf("%s.%s is refused with no reason", schema, k)
			}
		}
	}
}

// fakeKMS wraps by prefixing, and refuses while disabled.
type fakeKMS struct {
	mu       sync.Mutex
	disabled bool
}

func (k *fakeKMS) set(disabled bool) { k.mu.Lock(); k.disabled = disabled; k.mu.Unlock() }
func (k *fakeKMS) err(key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.disabled {
		return fmt.Errorf("CryptoKeyVersion %s/cryptoKeyVersions/1 is DISABLED: only an ENABLED version can decrypt", key)
	}
	return nil
}
func (k *fakeKMS) Encrypt(_ context.Context, key string, pt []byte) ([]byte, string, error) {
	if err := k.err(key); err != nil {
		return nil, "", err
	}
	return append([]byte("w:"), pt...), key + "/cryptoKeyVersions/1", nil
}
func (k *fakeKMS) Decrypt(_ context.Context, key string, ct []byte) ([]byte, error) {
	if err := k.err(key); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(ct, []byte("w:")) {
		return nil, errors.New("not wrapped by this key")
	}
	return ct[2:], nil
}

func fieldsServer(t *testing.T) (*httptest.Server, *fakeKMS) {
	t.Helper()
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), "")
	if err := srv.EnableBackups(&fakeDumper{data: map[string]string{}}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	k := &fakeKMS{}
	srv.UseKMS(k)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, k
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// getRunnable reads an instance once its operation has finished.
func getRunnable(t *testing.T, url string) map[string]any {
	t.Helper()
	var in map[string]any
	for i := 0; i < 200; i++ {
		code, body := call(t, "GET", url, "")
		if code != 200 {
			t.Fatalf("get %s = %d %s", url, code, body)
		}
		in = map[string]any{}
		_ = json.Unmarshal([]byte(body), &in)
		if in["state"] != "PENDING_CREATE" {
			return in
		}
		time.Sleep(10 * time.Millisecond)
	}
	return in
}

const testKey = "projects/proj/locations/us-central1/keyRings/r/cryptoKeys/k"

// instanceSamples are a meaningful value for each settable DatabaseInstance
// property, and what insert does with it: echo (answered back), accept
// (taken, input only or never answered), refuse (400 or 501 naming it), or
// elsewhere (tested where its behaviour is: replicas, source
// representation instances).
var instanceSamples = map[string][2]string{
	"name":                                  {`"x"`, "echo"},
	"databaseVersion":                       {`"POSTGRES_17"`, "echo"},
	"rootPassword":                          {`"pw"`, "accept"},
	"instanceType":                          {`"CLOUD_SQL_INSTANCE"`, "accept"},
	"region":                                {`"europe-west1"`, "echo"},
	"settings":                              {`{"tier":"db-custom-1-3840"}`, "echo"},
	"includeReplicasForMajorVersionUpgrade": {`true`, "accept"},
	"diskEncryptionConfiguration":           {`{"kmsKeyName":"` + testKey + `"}`, "echo"},
	"failoverReplica":                       {`{"name":"f"}`, "refuse:failover replica"},
	"masterInstanceName":                    {"", "elsewhere"},
	"replicaConfiguration":                  {"", "elsewhere"},
	"onPremisesConfiguration":               {"", "elsewhere"},

	"databaseCenterIntegrationEnabled":           {`true`, "refuse"},
	"geminiConfig":                               {`{"entitled":true}`, "refuse"},
	"maintenanceVersion":                         {`"POSTGRES_17_11.R20260101.00_00"`, "refuse"},
	"nodeCount":                                  {`2`, "refuse"},
	"replicationCluster":                         {`{"failoverDrReplicaName":"dr"}`, "refuse"},
	"switchTransactionLogsToCloudStorageEnabled": {`true`, "refuse"},
	"tags": {`{"tagKeys/1":"tagValues/2"}`, "refuse"},
}

// settingsSamples are the same for Settings.
var settingsSamples = map[string][2]string{
	"tier":                          {`"db-custom-2-7680"`, "echo"},
	"userLabels":                    {`{"env":"dev"}`, "echo"},
	"databaseFlags":                 {`[{"name":"max_connections","value":"50"}]`, "echo"},
	"deletionProtectionEnabled":     {`true`, "echo"},
	"availabilityType":              {`"REGIONAL"`, "echo"},
	"activationPolicy":              {`"ALWAYS"`, "echo"},
	"backupConfiguration":           {`{"enabled":true,"startTime":"03:00"}`, "echo"},
	"dataApiAccess":                 {`"ALLOW_DATA_API"`, "echo"},
	"edition":                       {`"ENTERPRISE_PLUS"`, "echo"},
	"dataDiskSizeGb":                {`"20"`, "echo"},
	"settingsVersion":               {`"1"`, "echo"},
	"databaseReplicationEnabled":    {`true`, "refuse"},
	"retainBackupsOnDelete":         {`true`, "echo"},
	"finalBackupConfig":             {`{"enabled":true,"retentionDays":7}`, "echo"},
	"authorizedGaeApplications":     {`["app"]`, "echo"},
	"autoUpgradeEnabled":            {`true`, "echo"},
	"crashSafeReplicationEnabled":   {`true`, "echo"},
	"dataCacheConfig":               {`{"dataCacheEnabled":true}`, "echo"},
	"dataDiskProvisionedIops":       {`"3000"`, "echo"},
	"dataDiskProvisionedThroughput": {`"140"`, "echo"},
	"dataDiskType":                  {`"PD_HDD"`, "echo"},
	"denyMaintenancePeriods":        {`[{"startDate":"2026-12-20","endDate":"2027-01-05","time":"00:00:00"}]`, "echo"},
	"enableDataplexIntegration":     {`true`, "echo"},
	"insightsConfig":                {`{"queryInsightsEnabled":true}`, "echo"},
	"ipConfiguration":               {`{"ipv4Enabled":true,"authorizedNetworks":[{"value":"10.0.0.0/8"}]}`, "echo"},
	"locationPreference":            {`{"zone":"us-central1-f"}`, "echo"},
	"maintenanceWindow":             {`{"day":7,"hour":3}`, "echo"},
	"performanceCaptureConfig":      {`{"enabled":true}`, "echo"},
	"pricingPlan":                   {`"PER_USE"`, "echo"},
	"replicationLagMaxSeconds":      {`60`, "echo"},
	"replicationType":               {`"SYNCHRONOUS"`, "echo"},
	"storageAutoResize":             {`false`, "echo"},
	"storageAutoResizeLimit":        {`"100"`, "echo"},
	"acceleratedReplicaMode":        {`true`, "echo"},

	"activeDirectoryConfig":     {`{"domain":"ad.example.com"}`, "refuse"},
	"advancedMachineFeatures":   {`{"threadsPerCore":1}`, "refuse"},
	"collation":                 {`"SQL_Latin1_General_CP1_CI_AS"`, "refuse"},
	"entraidConfig":             {`{"tenantId":"t"}`, "refuse"},
	"sqlServerAuditConfig":      {`{"bucket":"gs://b"}`, "refuse"},
	"timeZone":                  {`"UTC"`, "refuse"},
	"connectionPoolConfig":      {`{"connectionPoolingEnabled":true}`, "refuse"},
	"connectorEnforcement":      {`"REQUIRED"`, "refuse"},
	"enableGoogleMlIntegration": {`true`, "refuse"},
	"passwordValidationPolicy":  {`{"enablePasswordPolicy":true,"minLength":12}`, "refuse"},
	"readPoolAutoScaleConfig":   {`{"enabled":true}`, "refuse"},
}

// Every settable DatabaseInstance and Settings property is applied and
// answered, taken, or refused with 400 or 501 naming it, on an insert
// (#1538): never accepted and dropped.
func TestSQLFieldsNeverSilentlyDropped(t *testing.T) {
	schemas := discoverySchemas(t)
	ts, _ := fieldsServer(t)
	base := ts.URL + "/v1/projects/proj/instances"
	check := func(schema string, samples map[string][2]string, rules map[string]fieldRule) {
		var props []string
		for k := range schemas[schema] {
			props = append(props, k)
		}
		sort.Strings(props)
		for i, k := range props {
			if rules[k].how == fieldOutput {
				continue
			}
			s, ok := samples[k]
			if !ok {
				t.Errorf("%s.%s: settable but no sample", schema, k)
				continue
			}
			if s[1] == "elsewhere" {
				continue
			}
			name := fmt.Sprintf("f-%s-%02d", strings.ToLower(schema[:1]), i)
			var body string
			if schema == "Settings" {
				st := fmt.Sprintf(`{"tier":"db-custom-1-3840",%q:%s}`, k, s[0])
				if k == "tier" {
					st = fmt.Sprintf(`{%q:%s}`, k, s[0])
				}
				body = fmt.Sprintf(`{"name":%q,"databaseVersion":"POSTGRES_17","settings":%s}`, name, st)
			} else {
				m := map[string]json.RawMessage{"name": json.RawMessage(fmt.Sprintf("%q", name)),
					"databaseVersion": json.RawMessage(`"POSTGRES_17"`), "settings": json.RawMessage(`{"tier":"db-custom-1-3840"}`)}
				if k != "name" {
					m[k] = json.RawMessage(s[0])
				}
				b, _ := json.Marshal(m)
				body = string(b)
			}
			code, resp := call(t, "POST", base, body)
			label := schema + "." + k
			if want, ok := strings.CutPrefix(s[1], "refuse"); ok {
				want = strings.TrimPrefix(want, ":")
				if want == "" {
					want = k
				}
				if (code != 400 && code != 501) || !strings.Contains(resp, want) {
					t.Errorf("%s = %d %s; want 400 or 501 naming it", label, code, resp)
				}
				continue
			}
			if code != 200 {
				t.Errorf("%s = %d %s; want it taken", label, code, resp)
				continue
			}
			in := getRunnable(t, base+"/"+name)
			if s[1] != "echo" {
				continue
			}
			got := in
			if schema == "Settings" {
				got, _ = in["settings"].(map[string]any)
			}
			if _, ok := got[k]; !ok {
				t.Errorf("%s: taken but not answered: %v", label, got)
			}
		}
	}
	check("DatabaseInstance", instanceSamples, instanceFields)
	check("Settings", settingsSamples, settingsFields)

	// An unknown field is refused, not dropped.
	code, resp := call(t, "POST", base, `{"name":"unk","databaseVersion":"POSTGRES_17","settings":{"tier":"t","diskSizeGb":"10"}}`)
	if code != 400 || !strings.Contains(resp, "diskSizeGb") {
		t.Errorf("an unknown settings field = %d %s", code, resp)
	}
	// A CAS-issued server CA is refused by name.
	code, resp = call(t, "POST", base, `{"name":"cas","databaseVersion":"POSTGRES_17","settings":{"tier":"t",`+
		`"ipConfiguration":{"serverCaMode":"GOOGLE_MANAGED_CAS_CA"}}}`)
	if code != 501 || !strings.Contains(resp, "serverCaMode") {
		t.Errorf("serverCaMode GOOGLE_MANAGED_CAS_CA = %d %s", code, resp)
	}
}

// The other request bodies: each refused field is refused by name, and an
// unknown one with 400.
func TestSQLRequestBodyFieldsNeverSilentlyDropped(t *testing.T) {
	ts, _ := fieldsServer(t)
	inst := ts.URL + "/v1/projects/proj/instances/pg"
	for _, c := range []struct{ method, path, body, want string }{
		{"POST", "/users", `{"name":"u","databaseRoles":["pg_read_all_data"]}`, "databaseRoles"},
		{"POST", "/users", `{"name":"u","passwordPolicy":{"allowedFailedAttempts":3}}`, "passwordPolicy"},
		{"POST", "/users", `{"name":"u","dualPasswordType":"DUAL_PASSWORD"}`, "dualPasswordType"},
		{"POST", "/users", `{"name":"u","iamEmail":"a@example.com"}`, "iamEmail"},
		{"POST", "/users", `{"name":"u","serverRoles":["sysadmin"]}`, "serverRoles"},
		{"POST", "/users", `{"name":"u","sqlserverUserDetails":{"disabled":true}}`, "sqlserverUserDetails"},
		{"PUT", "/users?name=cloudburrow", `{"password":"p","passwordPolicy":{"enableFailedAttemptsCheck":true}}`, "passwordPolicy"},
		{"POST", "/users", `{"name":"u","nickname":"n"}`, "nickname"},
		{"POST", "/databases", `{"name":"d","owner":"o"}`, "owner"},
		{"POST", "/backupRuns", `{"backupKind":"PHYSICAL"}`, "backupKind"},
		{"POST", "/backupRuns", `{"location":"europe-west1"}`, "europe-west1"},
		{"POST", "/backupRuns", `{"diskEncryptionConfiguration":{"kmsKeyName":"` + testKey + `"}}`, "diskEncryptionConfiguration"},
		{"POST", "/backupRuns", `{"timeZone":"UTC"}`, "timeZone"},
		{"POST", "/sslCerts", `{"commonName":"c","validity":"1y"}`, "validity"},
	} {
		code, resp := call(t, c.method, inst+c.path, c.body)
		if (code != 400 && code != 501) || !strings.Contains(resp, c.want) {
			t.Errorf("%s %s %s = %d %s; want 400 or 501 naming %s", c.method, c.path, c.body, code, resp, c.want)
		}
	}
	code, resp := call(t, "POST", ts.URL+"/v1/projects/proj/backups", `{"instance":"pg","retention":"7d"}`)
	if code != 400 || !strings.Contains(resp, "retention") {
		t.Errorf("CreateBackup with an unknown field = %d %s", code, resp)
	}
	// What is taken still works: output-only fields are ignored.
	if code, resp := call(t, "POST", inst+"/users", `{"name":"u2","kind":"sql#user","etag":"e","dualPasswordType":"NO_DUAL_PASSWORD"}`); code != 200 {
		t.Errorf("users.insert with output fields = %d %s", code, resp)
	}
	if code, resp := call(t, "POST", inst+"/backupRuns", `{"description":"d","backupKind":"SNAPSHOT","location":"us-central1"}`); code != 200 {
		t.Errorf("backupRuns.insert = %d %s", code, resp)
	}
}

// diskEncryptionConfiguration (#1538): the key is kept and answered with
// the version that wrapped the instance's data key; it must be in the
// instance's region; while its version is disabled the instance answers
// SUSPENDED, a restart and the connectors are refused, and it is RUNNABLE
// again once the version is enabled. The key cannot be changed, a replica
// in the primary's region inherits it, and a clone keeps it.
func TestSQLDiskEncryptionConfiguration(t *testing.T) {
	ts, k := fieldsServer(t)
	base := ts.URL + "/v1/projects/proj/instances"
	code, resp := call(t, "POST", base, `{"name":"far","databaseVersion":"POSTGRES_17","settings":{"tier":"t"},`+
		`"diskEncryptionConfiguration":{"kmsKeyName":"projects/proj/locations/europe-west1/keyRings/r/cryptoKeys/k"}}`)
	if code != 400 || !strings.Contains(resp, "region") {
		t.Errorf("a key in another region = %d %s", code, resp)
	}
	code, resp = call(t, "POST", base, `{"name":"bad","databaseVersion":"POSTGRES_17","settings":{"tier":"t"},`+
		`"diskEncryptionConfiguration":{"kmsKeyName":"k"}}`)
	if code != 400 || !strings.Contains(resp, "kmsKeyName") {
		t.Errorf("a malformed key = %d %s", code, resp)
	}
	k.set(true)
	code, resp = call(t, "POST", base, `{"name":"off","databaseVersion":"POSTGRES_17","settings":{"tier":"t"},`+
		`"diskEncryptionConfiguration":{"kmsKeyName":"`+testKey+`"}}`)
	if code != 400 || !strings.Contains(resp, testKey) {
		t.Errorf("a create while the key is disabled = %d %s", code, resp)
	}
	k.set(false)
	if code, resp := call(t, "POST", base, `{"name":"enc","databaseVersion":"POSTGRES_17","settings":{"tier":"t"},`+
		`"diskEncryptionConfiguration":{"kmsKeyName":"`+testKey+`"}}`); code != 200 {
		t.Fatalf("insert = %d %s", code, resp)
	}
	in := getRunnable(t, base+"/enc")
	dec, _ := in["diskEncryptionConfiguration"].(map[string]any)
	st, _ := in["diskEncryptionStatus"].(map[string]any)
	if in["state"] != "RUNNABLE" || dec["kmsKeyName"] != testKey || st["kmsKeyVersionName"] != testKey+"/cryptoKeyVersions/1" {
		t.Fatalf("instance = %v", in)
	}
	code, resp = call(t, "PATCH", base+"/enc", `{"diskEncryptionConfiguration":{"kmsKeyName":"`+testKey+`2"}}`)
	if code != 400 || !strings.Contains(resp, "diskEncryptionConfiguration") {
		t.Errorf("a key change = %d %s", code, resp)
	}
	code, resp = call(t, "PATCH", base+"/enc", `{"settings":{"settingsVersion":"7"}}`)
	if code != 412 || !strings.Contains(resp, "settingsVersion") {
		t.Errorf("a stale settingsVersion = %d %s", code, resp)
	}

	k.set(true)
	in = getRunnable(t, base+"/enc")
	if in["state"] != "SUSPENDED" || fmt.Sprint(in["suspensionReason"]) != "[KMS_KEY_ISSUE]" {
		t.Errorf("with the key disabled: state %v %v", in["state"], in["suspensionReason"])
	}
	if code, resp := call(t, "POST", base+"/enc/restart", ""); code != 400 || !strings.Contains(resp, testKey) {
		t.Errorf("a restart with the key disabled = %d %s", code, resp)
	}
	if code, resp := call(t, "GET", base+"/enc/connectSettings", ""); code != 412 || !strings.Contains(resp, "KMS_KEY_ISSUE") {
		t.Errorf("connectSettings with the key disabled = %d %s", code, resp)
	}
	k.set(false)
	if in = getRunnable(t, base+"/enc"); in["state"] != "RUNNABLE" {
		t.Errorf("with the key enabled again: %v", in["state"])
	}
	if code, resp := call(t, "POST", base+"/enc/restart", ""); code != 200 {
		t.Errorf("a restart with the key enabled = %d %s", code, resp)
	}
}

// A read replica in its primary's region inherits the primary's key, and
// may not have another; one in another region needs a key of its own.
func TestSQLReplicaKey(t *testing.T) {
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(newFakeProv(), "")
	srv.UseKMS(&fakeKMS{})
	ctx := context.Background()
	primary := Spec{Name: "p", Project: "proj"}
	if err := srv.encryptWith(ctx, &primary, testKey); err != nil {
		t.Fatal(err)
	}
	srv.writes.byName["p"] = &created{spec: primary}
	same := Spec{Name: "r", MasterInstanceName: "p"}
	if err := srv.replicaKey(ctx, &same, ""); err != nil || same.KMSKey != testKey || len(same.WrappedKey) == 0 {
		t.Errorf("a same-region replica: %v %+v", err, same)
	}
	other := Spec{Name: "r2", MasterInstanceName: "p"}
	if err := srv.replicaKey(ctx, &other, testKey+"2"); err == nil || !strings.Contains(err.Error(), "inherits") {
		t.Errorf("a same-region replica with another key: %v", err)
	}
	far := Spec{Name: "r3", MasterInstanceName: "p", Region: "europe-west1"}
	if err := srv.replicaKey(ctx, &far, ""); err == nil || !strings.Contains(err.Error(), "kmsKeyName") {
		t.Errorf("a cross-region replica without a key: %v", err)
	}
	farKey := "projects/proj/locations/europe-west1/keyRings/r/cryptoKeys/k"
	if err := srv.replicaKey(ctx, &far, farKey); err != nil || far.KMSKey != farKey {
		t.Errorf("a cross-region replica with its region's key: %v %+v", err, far)
	}
}

// settings.retainBackupsOnDelete keeps an instance's backups after delete,
// and settings.finalBackupConfig takes a FINAL backup on a delete that does
// not say (#1538).
func TestSQLRetainBackupsAndFinalBackupConfig(t *testing.T) {
	ts, _ := fieldsServer(t)
	base := ts.URL + "/v1/projects/proj/instances"
	backups := func() string {
		_, body := call(t, "GET", ts.URL+"/v1/projects/proj/backups", "")
		return body
	}
	waitDone := func(name string) {
		t.Helper()
		for i := 0; i < 300; i++ {
			if code, _ := call(t, "GET", base+"/"+name, ""); code == 404 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s was not deleted", name)
	}
	for _, c := range []struct{ name, settings, want string }{
		{"keep", `"retainBackupsOnDelete":true`, `"ON_DEMAND"`},
		{"final", `"finalBackupConfig":{"enabled":true,"retentionDays":7}`, `"FINAL"`},
		{"none", `"storageAutoResize":true`, ""},
	} {
		if code, resp := call(t, "POST", base, `{"name":"`+c.name+`","databaseVersion":"POSTGRES_17","settings":{"tier":"t",`+c.settings+`}}`); code != 200 {
			t.Fatalf("insert %s = %d %s", c.name, code, resp)
		}
		getRunnable(t, base+"/"+c.name)
		if c.name == "keep" {
			if code, resp := call(t, "POST", base+"/keep/backupRuns", `{"description":"kept"}`); code != 200 {
				t.Fatalf("backupRuns.insert = %d %s", code, resp)
			}
			for i := 0; i < 300 && !strings.Contains(backups(), "kept"); i++ {
				time.Sleep(10 * time.Millisecond)
			}
		}
		for i := 0; i < 300; i++ {
			code, _ := call(t, "DELETE", base+"/"+c.name, "")
			if code == 200 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		waitDone(c.name)
		got := backups()
		has := strings.Contains(got, `"instance":"`+c.name+`"`)
		if c.want == "" && has {
			t.Errorf("%s: a backup was kept: %s", c.name, got)
		}
		if c.want != "" && (!has || !strings.Contains(got, c.want)) {
			t.Errorf("%s: want a %s backup kept: %s", c.name, c.want, got)
		}
	}
}
