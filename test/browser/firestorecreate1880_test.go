//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestFirestoreCreatesDatastoreModeAndKMSKeyDatabasesThroughThePage
// (#1880, #1881), in the emulators shard: on Create a Firestore database,
// Datastore mode makes a DATASTORE_MODE database, and Cloud KMS key's
// Browse lists the project's symmetric keys in the database's Cloud KMS
// location (us, for nam5), not another location's, and the key chosen is
// the database's cmek_config; GetDatabase reads both back.
func TestFirestoreCreatesDatastoreModeAndKMSKeyDatabasesThroughThePage(t *testing.T) {
	needService(t, "firestore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the APIs themselves", envFirestore)
	}
	kmsOpts := kmsSDKOptions(t)
	ctx := context.Background()
	c, err := admin.NewFirestoreAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	kc, err := kms.NewKeyManagementClient(ctx, kmsOpts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project

	ring := func(loc string) string {
		r, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/" + loc,
			KeyRingId: fmt.Sprintf("fs-%s-%d", loc, time.Now().UnixNano()%1e9)})
		if err != nil {
			t.Fatalf("CreateKeyRing: %v", err)
		}
		return r.GetName()
	}
	usRing, eastRing := ring("us"), ring("us-east1")
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: usRing, CryptoKeyId: "firestore",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: eastRing, CryptoKeyId: "elsewhere",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
		t.Fatal(err)
	}

	get := func(id string) *adminpb.Database {
		t.Helper()
		var db *adminpb.Database
		var err error
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
			if db, err = c.GetDatabase(ctx, &adminpb.GetDatabaseRequest{Name: "projects/" + project + "/databases/" + id}); err == nil {
				return db
			}
		}
		t.Fatalf("after Create Database, GetDatabase %s: %v", id, err)
		return nil
	}

	// Datastore mode.
	p.navigate("/firestore-databases/create" + q)
	p.waitFor(`document.querySelector("#view #f-mode-1") !== null && !document.querySelector("#view #f-mode-1").disabled`)
	p.setField("#view #f-database", "ds-mode")
	p.run(chromedp.Click(chromedp.CSS(`#view #f-mode-1`)))
	p.clickText(`#view form button[type="submit"]`, "Create Database")
	if db := get("ds-mode"); db.GetType() != adminpb.Database_DATASTORE_MODE {
		t.Errorf("the Datastore mode database = %v", db)
	}

	// A Cloud KMS key, chosen with Browse, in nam5: Cloud KMS's us.
	p.navigate("/firestore-databases/create" + q)
	p.waitFor(`document.querySelector("#view #f-encryption-1") !== null`)
	p.setField("#view #f-database", "with-key")
	p.run(chromedp.Click(chromedp.CSS(`#view #f-encryption-1`)))
	p.waitFor(`!document.querySelector("#view #f-kmsKey").closest(".form-row").hidden`)
	p.run(chromedp.Click(chromedp.CSS(`#view #browse-kmsKey`)))
	p.waitFor(`document.querySelector(".modal.is-open .fs-row-link") !== null`)
	var offered []string
	p.eval(`[...document.querySelectorAll(".modal.is-open .fs-row-link")].map((b) => b.textContent)`, &offered)
	if len(offered) != 1 || offered[0] != key.GetName() {
		t.Fatalf("Browse offers %v, want only %s", offered, key.GetName())
	}
	p.clickText(".modal.is-open .fs-row-link", key.GetName())
	p.waitFor(`document.querySelector(".modal.is-open") === null && document.querySelector("#view #f-kmsKey").value !== ""`)
	p.clickText(`#view form button[type="submit"]`, "Create Database")
	db := get("with-key")
	if db.GetCmekConfig().GetKmsKeyName() != key.GetName() || len(db.GetCmekConfig().GetActiveKeyVersion()) != 1 {
		t.Errorf("the database's cmek_config = %v, want %s and its version in use", db.GetCmekConfig(), key.GetName())
	}
}
