//go:build browser

package browser

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const envKMS = "CLOUDBURROW_TEST_KMS"

// kmsSDKOptions are the official Cloud KMS client's options for this
// instance's KMS endpoint, over gRPC; the test skips when it is not set.
func kmsSDKOptions(t *testing.T) []option.ClientOption {
	t.Helper()
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envKMS)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the Cloud KMS API itself", envKMS)
	}
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// ekmCertificatePEM is a self-signed leaf certificate for the EKM replica's
// hostname, as PEM.
func ekmCertificatePEM(t *testing.T, host string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1452), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestKMSEkmConnectionsThroughTheBrowser (#1452), in the run shard, whose
// instance serves Cloud KMS: on the EKM connections screen, Create EKM
// connection with a hostname, a Service Directory service and a PEM
// certificate makes the connection the official EkmService client's
// GetEkmConnection reads, with the certificate parsed; its page shows the
// certificate's subject. Edit, to CLOUD_KMS with a crypto space path, is
// read back the same way, and Set as default makes the location's EkmConfig
// name it.
func TestKMSEkmConnectionsThroughTheBrowser(t *testing.T) {
	needService(t, "kms-ekm")
	ctx := context.Background()
	c, err := kms.NewEkmClient(ctx, kmsSDKOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := uniqueProject(t)
	const host = "ekm.browser.test"
	loc := "projects/" + project + "/locations/us-east1"
	name := loc + "/ekmConnections/browser-ekm"

	p.navigate("/kms/ekm?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button.primary")].some((b) => b.textContent.startsWith("Create"))`)
	p.eval(`[...document.querySelectorAll("#view button.primary")].find((b) => b.textContent.startsWith("Create")).click()`, nil)
	p.waitFor(`document.querySelector("#view #f-ekmConnectionId, .modal.is-open #f-ekmConnectionId") !== null`)
	scope := "#view"
	var inModal bool
	p.eval(`document.querySelector(".modal.is-open #f-ekmConnectionId") !== null`, &inModal)
	if inModal {
		scope = ".modal.is-open"
	}
	setField(p, scope, "f-ekmConnectionId", "browser-ekm")
	setField(p, scope, "f-location", "us-east1")
	setField(p, scope, "f-serviceDirectoryService", loc+"/namespaces/ekm/services/replica")
	setField(p, scope, "f-hostname", host)
	setField(p, scope, "f-certificate", ekmCertificatePEM(t, host))
	p.run(chromedp.Click(scope+` button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, host))

	got, err := c.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: name})
	if err != nil {
		t.Fatalf("GetEkmConnection after Create EKM connection: %v", err)
	}
	rs := got.GetServiceResolvers()
	if len(rs) != 1 || rs[0].GetHostname() != host || rs[0].GetServiceDirectoryService() != loc+"/namespaces/ekm/services/replica" ||
		len(rs[0].GetServerCertificates()) != 1 || !rs[0].GetServerCertificates()[0].GetParsed() ||
		rs[0].GetServerCertificates()[0].GetSubject() != "CN="+host || got.GetKeyManagementMode() != kmspb.EkmConnection_MANUAL {
		t.Fatalf("GetEkmConnection = %v; want the form's resolver, a parsed certificate and MANUAL", got)
	}

	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q)).querySelector("a").click()`, host), nil)
	p.waitFor(`document.querySelector("#view").textContent.includes("CN=` + host + `")`)

	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-keyManagementMode") !== null`)
	setField(p, ".modal.is-open", "f-keyManagementMode", "CLOUD_KMS")
	setField(p, ".modal.is-open", "f-cryptoSpacePath", "v0/longlived/browser")
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	got, err = c.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: name})
	if err != nil || got.GetKeyManagementMode() != kmspb.EkmConnection_CLOUD_KMS || got.GetCryptoSpacePath() != "v0/longlived/browser" {
		t.Fatalf("GetEkmConnection after Edit = %v, %v; want CLOUD_KMS and the crypto space path", got, err)
	}

	// Set as default changes the location's EkmConfig: it is not sent until
	// the connection's name is typed back, as its confirmation promises.
	edits := len(p.sent(http.MethodPost, "/api/actions/kms-ekm"))
	p.clickText("#view .page-actions button", "Set as default")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	if sent := p.sent(http.MethodPost, "/api/actions/kms-ekm"); len(sent) != edits {
		t.Fatalf("Set as default was sent before it was confirmed: %v", sent[edits:])
	}
	p.confirmByName(name)
	p.waitFor(`document.querySelector(".modal") === null`)
	cfg, err := c.GetEkmConfig(ctx, &kmspb.GetEkmConfigRequest{Name: loc + "/ekmConfig"})
	if err != nil || cfg.GetDefaultEkmConnection() != name {
		t.Fatalf("GetEkmConfig after Set as default = %v, %v; want %s", cfg, err, name)
	}
}

// TestKMSAutokeyThroughTheBrowser (#1452), in the run shard: on the Autokey
// screen a new project's own configuration row is listed; Edit
// configuration, with the project itself as the key project, is read back by
// the official AutokeyAdmin client's GetAutokeyConfig and
// ShowEffectiveAutokeyConfig. Create key handle for a Cloud Storage bucket
// then makes the handle the Autokey client's ListKeyHandles lists, naming an
// HSM key in the key ring "autokey", which the project's page lists on its
// Key handles tab.
func TestKMSAutokeyThroughTheBrowser(t *testing.T) {
	needService(t, "kms-autokey")
	ctx := context.Background()
	opts := kmsSDKOptions(t)
	admin, err := kms.NewAutokeyAdminClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ak, err := kms.NewAutokeyClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ak.Close() })
	km, err := kms.NewKeyManagementClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = km.Close() })
	p := open(t)
	project := uniqueProject(t)
	cfgName := "projects/" + project + "/autokeyConfig"

	p.navigate("/kms/autokey?project=" + project)
	p.clickText("#view tbody a", cfgName)
	p.clickText("#view .page-actions button", "Edit configuration")
	p.waitFor(`document.querySelector(".modal.is-open #f-keyProject") !== null`)
	setField(p, ".modal.is-open", "f-keyProjectResolutionMode", "DEDICATED_KEY_PROJECT")
	setField(p, ".modal.is-open", "f-keyProject", "projects/"+project)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)

	cfg, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: cfgName})
	if err != nil || cfg.GetKeyProject() != "projects/"+project || cfg.GetState() != kmspb.AutokeyConfig_ACTIVE {
		t.Fatalf("GetAutokeyConfig after Edit configuration = %v, %v; want ACTIVE with key project projects/%s", cfg, err, project)
	}
	eff, err := admin.ShowEffectiveAutokeyConfig(ctx, &kmspb.ShowEffectiveAutokeyConfigRequest{Parent: "projects/" + project})
	if err != nil || eff.GetKeyProject() != "projects/"+project {
		t.Fatalf("ShowEffectiveAutokeyConfig = %v, %v", eff, err)
	}

	p.navigate("/kms/autokey?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button.primary")].some((b) => b.textContent === "Create key handle")`)
	p.eval(`[...document.querySelectorAll("#view button.primary")].find((b) => b.textContent === "Create key handle").click()`, nil)
	p.waitFor(`document.querySelector("#view #f-keyHandleId, .modal.is-open #f-keyHandleId") !== null`)
	scope := "#view"
	var inModal bool
	p.eval(`document.querySelector(".modal.is-open #f-keyHandleId") !== null`, &inModal)
	if inModal {
		scope = ".modal.is-open"
	}
	setField(p, scope, "f-location", "us-central1")
	setField(p, scope, "f-resourceTypeSelector", "storage.googleapis.com/Bucket")
	setField(p, scope, "f-keyHandleId", "browser-bucket")
	p.run(chromedp.Click(scope+` button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#f-keyHandleId") === null`)
	p.clickText("#view tbody a", cfgName)
	p.clickText("#view .tab-strip .tab", "Key handles")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser-bucket"))`)

	parent := "projects/" + project + "/locations/us-central1"
	var handle *kmspb.KeyHandle
	it := ak.ListKeyHandles(ctx, &kmspb.ListKeyHandlesRequest{Parent: parent})
	for {
		h, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListKeyHandles: %v", err)
		}
		if h.GetName() == parent+"/keyHandles/browser-bucket" {
			handle = h
		}
	}
	wantRing := "projects/" + project + "/locations/us-central1/keyRings/autokey/cryptoKeys/"
	if handle == nil || handle.GetResourceTypeSelector() != "storage.googleapis.com/Bucket" || !strings.HasPrefix(handle.GetKmsKey(), wantRing) {
		t.Fatalf("ListKeyHandles found %v; want browser-bucket for a bucket with a key under %s", handle, wantRing)
	}
	key, err := km.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: handle.GetKmsKey()})
	if err != nil || key.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT || key.GetVersionTemplate().GetProtectionLevel() != kmspb.ProtectionLevel_HSM {
		t.Errorf("the handle's key = %v, %v; want an HSM ENCRYPT_DECRYPT key", key, err)
	}
}
