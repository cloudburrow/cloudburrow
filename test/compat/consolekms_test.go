//go:build compat

package compat

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestConsoleKMSEditKey (#794), in the served shard, whose instance serves
// Cloud KMS: the console's Edit key on a key the official client created is
// prefilled with its labels, and the labels saved there are what the official
// client's GetCryptoKey reads, with the primary version unchanged. A label key
// UpdateCryptoKey refuses is refused by the console with the message the
// official client's own UpdateCryptoKey receives for it, and changes nothing.
// A rotation period and next rotation time saved there are what GetCryptoKey
// reads, and a period below the 24-hour minimum is refused with the message
// the official client's UpdateCryptoKey receives for it (#816).
func TestConsoleKMSEditKey(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := h.Project()

	// Rings and keys cannot be deleted; the project is the test's own.
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
		Parent: "projects/" + project + "/locations/global", KeyRingId: "console-edit"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, Labels: map[string]string{"env": "dev"}}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}

	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/kms?project="+project+
		"&name="+url.QueryEscape(ring.GetName())+"&name=k", "")
	if code != http.StatusOK {
		t.Fatalf("console detail = %d: %s", code, body)
	}
	var detail struct {
		Unavailable string
		Edit        *struct {
			Label  string
			Fields []struct {
				Name, Default string
				Immutable     bool
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if detail.Edit == nil || detail.Edit.Label != "Edit key" {
		t.Fatalf("the key's page offers no Edit key (unavailable: %q): %s", detail.Unavailable, body)
	}
	values := map[string]string{}
	for _, f := range detail.Edit.Fields {
		if !f.Immutable {
			values[f.Name] = f.Default
		}
	}
	if len(values) != 3 || values["labels"] != `{"env":"dev"}` || values["rotationPeriod"] != "" || values["nextRotationTime"] != "" {
		t.Fatalf("Edit key would submit %v; want the key's labels and an empty rotation schedule", values)
	}
	edit := func(v map[string]string) (int, string) {
		b, _ := json.Marshal(map[string]any{"Path": []string{ring.GetName(), "k"}, "Values": v})
		return consoleDo(t, addr, http.MethodPatch, "/api/resources/kms?project="+project, string(b))
	}

	values["labels"] = `{"env":"prod","team":"payments"}`
	if code, body := edit(values); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, body)
	}
	got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
	if err != nil {
		t.Fatalf("GetCryptoKey: %v", err)
	}
	if l := got.GetLabels(); len(l) != 2 || l["env"] != "prod" || l["team"] != "payments" {
		t.Errorf("GetCryptoKey reads labels %v; want what the console saved", l)
	}
	if got.GetPrimary().GetName() != key.GetPrimary().GetName() {
		t.Errorf("the edit moved the primary from %s to %s", key.GetPrimary().GetName(), got.GetPrimary().GetName())
	}

	// A label key with a capital: the console's refusal is the API's.
	_, sdkErr := c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{
		CryptoKey:  &kmspb.CryptoKey{Name: key.GetName(), Labels: map[string]string{"Env": "x"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	st, _ := status.FromError(sdkErr)
	if sdkErr == nil || st.Message() == "" {
		t.Fatalf("UpdateCryptoKey with label key Env = %v; want it refused", sdkErr)
	}
	code, body = edit(map[string]string{"labels": `{"Env":"x"}`})
	var refusal struct{ Error string }
	_ = json.Unmarshal([]byte(body), &refusal)
	if want := st.Code().String() + ": " + st.Message(); code != http.StatusBadRequest || refusal.Error != want {
		t.Errorf("console edit with label key Env = %d %s; want 400 with UpdateCryptoKey's own %q", code, body, want)
	}
	if got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); err != nil || got.GetLabels()["env"] != "prod" {
		t.Errorf("after a refused edit GetCryptoKey reads labels %v (%v)", got.GetLabels(), err)
	}

	// The rotation schedule (#816): set from the console, read back by the
	// official client.
	next := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	values["rotationPeriod"], values["nextRotationTime"] = "30d", next.Format(time.RFC3339)
	if code, body := edit(values); code != http.StatusOK {
		t.Fatalf("console edit of the rotation schedule = %d: %s", code, body)
	}
	got, err = c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()})
	if err != nil || got.GetRotationPeriod().AsDuration() != 30*24*time.Hour || !got.GetNextRotationTime().AsTime().Equal(next) {
		t.Errorf("GetCryptoKey reads rotation_period %v, next_rotation_time %v (%v); want 720h and %v", got.GetRotationPeriod(), got.GetNextRotationTime(), err, next)
	}

	// A period below the minimum: the console's refusal is the API's.
	_, sdkErr = c.UpdateCryptoKey(ctx, &kmspb.UpdateCryptoKeyRequest{
		CryptoKey:  &kmspb.CryptoKey{Name: key.GetName(), RotationSchedule: &kmspb.CryptoKey_RotationPeriod{RotationPeriod: durationpb.New(23 * time.Hour)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rotation_period"}}})
	st, _ = status.FromError(sdkErr)
	if sdkErr == nil || !strings.Contains(st.Message(), "at least 24 hours") {
		t.Fatalf("UpdateCryptoKey with rotation_period 23h = %v; want it refused naming the minimum", sdkErr)
	}
	values["rotationPeriod"] = "23h"
	code, body = edit(values)
	refusal.Error = ""
	_ = json.Unmarshal([]byte(body), &refusal)
	if want := st.Code().String() + ": " + st.Message(); code != http.StatusBadRequest || refusal.Error != want {
		t.Errorf("console edit with rotation period 23h = %d %s; want 400 with UpdateCryptoKey's own %q", code, body, want)
	}
	if got, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); err != nil || got.GetRotationPeriod().AsDuration() != 30*24*time.Hour {
		t.Errorf("after a refused edit GetCryptoKey reads rotation_period %v (%v)", got.GetRotationPeriod(), err)
	}
}

// TestConsoleKMSImportJobsAndRetiredResources (#1219), in the served shard,
// whose instance serves Cloud KMS. Through the console API: Create import job
// on a ring's page makes the job the official client's GetImportJob reads;
// Import version with plain key material, as base64 and as a file through the
// action upload route, wraps it under the job's public key and imports it, so
// the official client's MacSign with the new version is the HMAC of that key;
// and the ring's page lists the job, and the key the official client deleted
// under Retired resources, as its ListRetiredResources does.
func TestConsoleKMSImportJobsAndRetiredResources(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	c, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := h.Project()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{
		Parent: "projects/" + project + "/locations/global", KeyRingId: "console-import"})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	act := func(path []string, action string, values map[string]string) (int, string) {
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/kms?project="+project, string(b))
	}
	if code, body := act([]string{ring.GetName()}, "createimportjob",
		map[string]string{"importJobId": "job", "importMethod": "RSA_OAEP_3072_SHA256_AES_256", "protectionLevel": "SOFTWARE"}); code != http.StatusOK {
		t.Fatalf("Create import job = %d: %s", code, body)
	}
	job, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: ring.GetName() + "/importJobs/job"})
	if err != nil || job.GetState() != kmspb.ImportJob_ACTIVE || job.GetImportMethod() != kmspb.ImportJob_RSA_OAEP_3072_SHA256_AES_256 {
		t.Fatalf("GetImportJob = %v, %v", job, err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "mac",
		SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_MAC,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_HMAC_SHA256}}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	values := map[string]string{"importJob": "job", "algorithm": "HMAC_SHA256", "format": "PLAIN"}
	check := func(version string, secret []byte) {
		t.Helper()
		r, err := c.MacSign(ctx, &kmspb.MacSignRequest{Name: version, Data: []byte("hello")})
		if err != nil {
			t.Fatalf("MacSign %s: %v", version, err)
		}
		m := hmac.New(sha256.New, secret)
		m.Write([]byte("hello"))
		if !bytes.Equal(r.GetMac(), m.Sum(nil)) {
			t.Errorf("MacSign with %s is not the HMAC of the key the console imported", version)
		}
	}

	k1 := bytes.Repeat([]byte{1}, 32)
	values["material"] = base64.StdEncoding.EncodeToString(k1)
	if code, body := act([]string{ring.GetName(), "mac"}, "importversion", values); code != http.StatusOK {
		t.Fatalf("Import version (base64) = %d: %s", code, body)
	}
	check(key.GetName()+"/cryptoKeyVersions/1", k1)

	k2 := bytes.Repeat([]byte{2}, 32)
	delete(values, "material")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormField("request")
	_ = json.NewEncoder(w).Encode(map[string]any{"Path": []string{ring.GetName(), "mac"}, "Action": "importversion", "Values": values})
	part, _ := mw.CreateFormFile("file", "key.bin")
	_, _ = part.Write(k2)
	_ = mw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/actions/kms/upload?project="+url.QueryEscape(project), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Import version (file) = %d: %s", resp.StatusCode, out)
	}
	check(key.GetName()+"/cryptoKeyVersions/2", k2)

	// A deleted key leaves a retired resource.
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "gone",
		SkipInitialVersionCreation: true, CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	op, err := c.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: ring.GetName() + "/cryptoKeys/gone"})
	if err != nil {
		t.Fatalf("DeleteCryptoKey: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("DeleteCryptoKey: %v", err)
	}
	var retired []string
	it := c.ListRetiredResources(ctx, &kmspb.ListRetiredResourcesRequest{Parent: "projects/" + project + "/locations/global"})
	for {
		r, err := it.Next()
		if err != nil {
			break
		}
		if strings.HasPrefix(r.GetOriginalResource(), ring.GetName()+"/") {
			retired = append(retired, r.GetName())
		}
	}
	if len(retired) != 1 {
		t.Fatalf("ListRetiredResources lists %v for this ring; want the deleted key", retired)
	}

	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/kms?project="+project+"&name="+url.QueryEscape(ring.GetName()), "")
	if code != http.StatusOK {
		t.Fatalf("console detail = %d: %s", code, body)
	}
	var detail struct {
		Sections []struct {
			ID      string
			Listing struct {
				Items []struct {
					Name   string
					Status string
					Fields map[string]string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	found := map[string]bool{}
	for _, s := range detail.Sections {
		for _, it := range s.Listing.Items {
			switch {
			case s.ID == "importjobs" && it.Name == "job" && it.Status == "ACTIVE":
				found["importjobs"] = true
			case s.ID == "retired" && strings.HasSuffix(retired[0], "/"+it.Name) &&
				it.Fields["Original resource"] == ring.GetName()+"/cryptoKeys/gone":
				found["retired"] = true
			}
		}
	}
	if !found["importjobs"] || !found["retired"] {
		t.Errorf("the ring's page lists %v of the import job and the retired key: %s", found, body)
	}
}
