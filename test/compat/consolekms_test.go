//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestConsoleKMSEditKey (#794), in the served shard, whose instance serves
// Cloud KMS: the console's Edit key on a key the official client created is
// prefilled with its labels, and the labels saved there are what the official
// client's GetCryptoKey reads, with the primary version unchanged. A label key
// UpdateCryptoKey refuses is refused by the console with the message the
// official client's own UpdateCryptoKey receives for it, and changes nothing.
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
	if len(values) != 1 || values["labels"] != `{"env":"dev"}` {
		t.Fatalf("Edit key would submit %v; want the key's labels and nothing else", values)
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
}
