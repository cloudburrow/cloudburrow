//go:build browser

package browser

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/chromedp/chromedp"
)

// kmsConsoleAct runs one of a provider's actions through the console API.
func kmsConsoleAct(t *testing.T, service, project string, path []string, action string, values map[string]string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/"+service+"?project="+project, string(b)); code != http.StatusOK {
		t.Fatalf("%s %s through the console API = %d: %s", service, action, code, body)
	}
}

// hsmSign signs the challenge in cs that is for the public key pm, as a
// two-factor key's holder does.
func hsmSign(t *testing.T, k *rsa.PrivateKey, pm string, cs []*kmspb.Challenge) string {
	t.Helper()
	for _, ch := range cs {
		if ch.GetPublicKeyPem() == pm {
			d := sha256.Sum256(ch.GetChallenge())
			sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
			if err != nil {
				t.Fatal(err)
			}
			return base64.StdEncoding.EncodeToString(sig)
		}
	}
	t.Fatalf("no challenge for the key %q", pm)
	return ""
}

// TestKMSTrustedKeyWrappingThroughThePage (#1592), in the run shard, whose
// instance serves Cloud KMS. Over a single-tenant HSM instance whose quorum
// is registered and an AES_WRAPPING version the quorum made hsm_trusted
// (both through the console API, the challenges signed here):
//   - Create key on the ring's page with Trusted wrapping checked makes an
//     HSM_SINGLE_TENANT key whose first version GetCryptoKeyVersion reads
//     with trusted_wrapping_enabled;
//   - Export trusted-wrapped on that version's page shows the key wrapped
//     under the trusted version;
//   - Import trusted-wrapped version on a second key's page imports it, and
//     the official client's RawDecrypt with the copy opens what RawEncrypt
//     sealed with the original.
func TestKMSTrustedKeyWrappingThroughThePage(t *testing.T) {
	needService(t, "kms")
	needService(t, "kms-hsm")
	opts := kmsSDKOptions(t)
	ctx := context.Background()
	c, err := kms.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	hc, err := kms.NewHsmManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hc.Close() })
	project := uniqueProject(t)
	inst := "projects/" + project + "/locations/us-central1/singleTenantHsmInstances/browser-hsm"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms-hsm?project="+project,
		`{"singleTenantHsmInstanceId":"browser-hsm","location":"us-central1","totalApproverCount":"3","keyPortabilityEnabled":"true"}`); code != http.StatusOK {
		t.Fatalf("create a single-tenant HSM instance through the console API = %d: %s", code, body)
	}
	var keys []*rsa.PrivateKey
	var pems []string
	for i := 0; i < 3; i++ {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
		keys, pems = append(keys, k), append(pems, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	}
	hsm := func(action string, values map[string]string) {
		t.Helper()
		kmsConsoleAct(t, "kms-hsm", project, []string{inst}, action, values)
	}
	proposal := func(id string) *kmspb.SingleTenantHsmInstanceProposal {
		t.Helper()
		pr, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: inst + "/proposals/" + id})
		if err != nil {
			t.Fatal(err)
		}
		return pr
	}
	hsm("propose", map[string]string{"proposal": "reg", "operationType": "register_2fa_keys", "requiredApproverCount": "2",
		"twoFactorPublicKeyPems": strings.Join(pems, "")})
	reg := proposal("reg")
	for i := range keys {
		hsm("approve", map[string]string{"proposal": "reg", "publicKeyPem": pems[i],
			"signedChallenge": hsmSign(t, keys[i], pems[i], reg.GetQuorumParameters().GetChallenges())})
	}
	hsm("execute", map[string]string{"proposal": "reg"})

	ring := "projects/" + project + "/locations/us-central1/keyRings/browser-trust"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project,
		`{"keyRingId":"browser-trust","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	for _, v := range []map[string]string{
		{"cryptoKeyId": "wrap", "purpose": "AES_WRAPPING", "algorithm": "AES_256_KWP"},
		{"cryptoKeyId": "copy", "purpose": "RAW_ENCRYPT_DECRYPT", "algorithm": "AES_256_GCM", "skipInitialVersionCreation": "true"},
	} {
		v["protectionLevel"], v["cryptoKeyBackend"] = "HSM_SINGLE_TENANT", inst
		kmsConsoleAct(t, "kms", project, []string{ring}, "createkey", v)
	}
	wv := ring + "/cryptoKeys/wrap/cryptoKeyVersions/1"
	hsm("propose", map[string]string{"proposal": "up", "operationType": "upgrade_key_trust", "cryptoKeyVersion": wv, "twoFactorPublicKeyPems": pems[2]})
	rq := proposal("up").GetRequiredActionQuorumParameters()
	hsm("approve", map[string]string{"proposal": "up", "kind": "required", "publicKeyPem": pems[2],
		"signedChallenge": hsmSign(t, keys[2], pems[2], rq.GetRequiredChallenges())})
	for i := 0; i < 2; i++ {
		hsm("approve", map[string]string{"proposal": "up", "kind": "quorum", "publicKeyPem": pems[i],
			"signedChallenge": hsmSign(t, keys[i], pems[i], rq.GetQuorumChallenges())})
	}
	hsm("execute", map[string]string{"proposal": "up"})
	if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: wv}); err != nil || !v.GetHsmTrusted() {
		t.Fatalf("the wrapping version after upgrade_key_trust = %v, %v; want hsm_trusted", v, err)
	}

	// Create key with Trusted wrapping, its fields set in order: the purpose
	// chooses the algorithms offered.
	p := open(t)
	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring)
	p.clickText("#view .page-actions button", "Create key")
	p.waitFor(`document.querySelector(".modal.is-open #f-cryptoKeyId") !== null`)
	for _, f := range [][2]string{{"cryptoKeyId", "raw"}, {"protectionLevel", "HSM_SINGLE_TENANT"}, {"purpose", "RAW_ENCRYPT_DECRYPT"},
		{"algorithm", "AES_256_GCM"}, {"cryptoKeyBackend", inst}, {"trustedWrappingEnabled", "true"}} {
		setField(p, ".modal.is-open", "f-"+f[0], f[1])
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null || [...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var refused string
	p.eval(`[...document.querySelectorAll(".modal.is-open .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refused)
	if refused != "" {
		t.Fatalf("Create key was refused: %s", refused)
	}
	rv := ring + "/cryptoKeys/raw/cryptoKeyVersions/1"
	if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: rv}); err != nil || !v.GetTrustedWrappingEnabled() ||
		v.GetProtectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		t.Fatalf("GetCryptoKeyVersion after Create key with Trusted wrapping = %v, %v; want an HSM_SINGLE_TENANT version with trusted wrapping", v, err)
	}

	// Export trusted-wrapped on the version's page.
	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring)
	p.clickText("#view tbody a", "raw")
	p.waitFor(`location.pathname.includes("raw")`)
	p.clickText("#view tbody a", "1")
	p.waitFor(`document.querySelector("#view").textContent.includes("Trusted wrapping")`)
	p.clickText("#view .page-actions button", "Export trusted-wrapped")
	p.waitFor(`document.querySelector(".modal.is-open #f-wrappingKey") !== null`)
	setField(p, ".modal.is-open", "f-wrappingKey", wv)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector("#action-result-table") !== null || [...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var wrapped string
	p.eval(`(() => { const t = document.querySelector("#action-result-table"); if (!t) return "";
		return [...t.querySelectorAll("td")].map((c) => c.textContent.trim()).filter((s) => /^[A-Za-z0-9+/=]{40,}$/.test(s))[0] || ""; })()`, &wrapped)
	if wrapped == "" {
		p.eval(`[...document.querySelectorAll(".modal.is-open .form-error, #action-result-table")].map((e) => e.textContent).join(" ")`, &refused)
		t.Fatalf("Export trusted-wrapped showed no wrapped key: %s", refused)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)

	// Import trusted-wrapped version on the copy's page.
	p.navigate("/kms/keyrings?project=" + project)
	p.clickText("#view tbody a", ring)
	p.clickText("#view tbody a", "copy")
	p.waitFor(`location.pathname.includes("copy")`)
	p.clickText("#view .page-actions button", "Import trusted-wrapped version")
	p.waitFor(`document.querySelector(".modal.is-open #f-material") !== null`)
	for _, f := range [][2]string{{"importingKey", wv}, {"algorithm", "AES_256_GCM"}, {"material", wrapped}} {
		setField(p, ".modal.is-open", "f-"+f[0], f[1])
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null || [...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	p.eval(`[...document.querySelectorAll(".modal.is-open .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refused)
	if refused != "" {
		t.Fatalf("Import trusted-wrapped version was refused: %s", refused)
	}
	enc, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: rv, Plaintext: []byte("carried by the page")})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: ring + "/cryptoKeys/copy/cryptoKeyVersions/1",
		Ciphertext: enc.GetCiphertext(), InitializationVector: enc.GetInitializationVector()})
	if err != nil || string(dec.GetPlaintext()) != "carried by the page" {
		t.Fatalf("RawDecrypt with the version Import trusted-wrapped version made = %v, %v; want what the original sealed", dec, err)
	}
}
