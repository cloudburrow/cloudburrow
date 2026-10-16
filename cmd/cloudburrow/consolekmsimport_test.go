package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func kmsSection(t *testing.T, d console.Detail, id string) console.Section {
	t.Helper()
	for _, s := range d.Sections {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no %s section in %+v", id, d.Sections)
	return console.Section{}
}

// Create import job on a ring's page makes the job the official client
// reads; the ring's Import jobs tab lists it and its page shows the wrapping
// public key. Import version on a key's page takes key material wrapped
// elsewhere, as a file or base64, or plain key material it wraps under the
// job's public key itself, for both families of import method, and the
// version imported holds that material: MacSign with it is the HMAC of the
// key given (#1219).
func TestConsoleKMSImportJobsAndImportVersion(t *testing.T) {
	ctx := context.Background()
	p, c, ring := kmsConsole(t)
	if !slices.Contains(kmsActionIDs(p, []string{ring}), "createimportjob") {
		t.Fatalf("a ring offers %v; want Create import job", kmsActionIDs(p, []string{ring}))
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "mac",
		SkipInitialVersionCreation: true,
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_MAC,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_HMAC_SHA256}}}); err != nil {
		t.Fatal(err)
	}
	// No ACTIVE job yet: no Import version to fail.
	if slices.Contains(kmsActionIDs(p, []string{ring, "mac"}), "importversion") {
		t.Fatal("Import version is offered on a ring with no import job")
	}
	for _, m := range []string{"RSA_OAEP_3072_SHA256_AES_256", "RSA_OAEP_3072_SHA256"} {
		if err := p.ActAt(ctx, "demo-proj", []string{ring}, "createimportjob",
			map[string]string{"importJobId": strings.ToLower(strings.ReplaceAll(m, "_", "-")), "importMethod": m, "protectionLevel": "SOFTWARE"}); err != nil {
			t.Fatalf("Create import job %s: %v", m, err)
		}
	}
	if err := p.ActAt(ctx, "demo-proj", []string{ring}, "createimportjob",
		map[string]string{"importJobId": "external", "importMethod": kmsImportMethods[0], "protectionLevel": "EXTERNAL"}); err == nil {
		t.Error("Create import job at EXTERNAL was accepted")
	}
	aesJob := ring + "/importJobs/rsa-oaep-3072-sha256-aes-256"
	got, err := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: aesJob})
	if err != nil || got.GetImportMethod() != kmspb.ImportJob_RSA_OAEP_3072_SHA256_AES_256 || got.GetState() != kmspb.ImportJob_ACTIVE {
		t.Fatalf("GetImportJob = %v, %v", got, err)
	}

	d, err := p.Detail(ctx, "demo-proj", []string{ring})
	if err != nil {
		t.Fatal(err)
	}
	jobs := kmsSection(t, d, "importjobs").Listing
	if jobs.Total != 2 || jobs.Items[0].Status != "ACTIVE" || jobs.Items[0].Fields["Import method"] == "" ||
		!slices.Equal(jobs.Items[0].Opens, []string{ring, kmsImportJobsSegment, jobs.Items[0].Name}) {
		t.Fatalf("Import jobs tab = %+v", jobs)
	}
	jd, err := p.Detail(ctx, "demo-proj", []string{ring, kmsImportJobsSegment, "rsa-oaep-3072-sha256-aes-256"})
	if err != nil || jd.Unavailable != "" {
		t.Fatalf("import job page: %v %q", err, jd.Unavailable)
	}
	if pk := kmsSection(t, jd, "publickey"); pk.Text != got.GetPublicKey().GetPem() {
		t.Errorf("the job's page shows %q; want GetImportJob's PEM", pk.Text)
	}
	if acts := kmsActionIDs(p, []string{ring, kmsImportJobsSegment, "rsa-oaep-3072-sha256-aes-256"}); len(acts) != 0 {
		t.Errorf("an import job's page offers %v", acts)
	}

	acts := p.DetailActions(ctx, "demo-proj", []string{ring, "mac"})
	i := slices.IndexFunc(acts, func(a console.Action) bool { return a.ID == "importversion" })
	if i < 0 {
		t.Fatalf("a key offers %v; want Import version", kmsActionIDs(p, []string{ring, "mac"}))
	}
	var fileField bool
	for _, f := range acts[i].Fields {
		if f.Name == "importJob" && len(f.Options) != 2 {
			t.Errorf("Import job choices %v; want both ACTIVE jobs", f.Options)
		}
		fileField = fileField || f.Type == console.FileFieldType
	}
	if !fileField {
		t.Error("Import version has no file field")
	}

	check := func(version string, key []byte) {
		t.Helper()
		r, err := c.MacSign(ctx, &kmspb.MacSignRequest{Name: version, Data: []byte("hello")})
		if err != nil {
			t.Fatalf("MacSign %s: %v", version, err)
		}
		m := hmac.New(sha256.New, key)
		m.Write([]byte("hello"))
		if !bytes.Equal(r.GetMac(), m.Sum(nil)) {
			t.Errorf("%s does not hold the key imported", version)
		}
		v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: version})
		if err != nil || v.GetImportJob() == "" || v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
			t.Errorf("GetCryptoKeyVersion %s = %v, %v; want an ENABLED imported version", version, v, err)
		}
	}
	newKey := func() []byte {
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		return k
	}
	path := []string{ring, "mac"}
	vals := func(job, format, material string) map[string]string {
		return map[string]string{"importJob": job, "algorithm": "HMAC_SHA256", "format": format, "material": material}
	}

	// 1: plain, as base64, wrapped here with RSA-OAEP and AES-KWP.
	k1 := newKey()
	if err := p.ActAt(ctx, "demo-proj", path, "importversion",
		vals("rsa-oaep-3072-sha256-aes-256", kmsMaterialPlain, base64.StdEncoding.EncodeToString(k1))); err != nil {
		t.Fatalf("Import version, plain: %v", err)
	}
	check(ring+"/cryptoKeys/mac/cryptoKeyVersions/1", k1)

	// 2: plain, as a file, under the RSA-OAEP-only job.
	k2 := newKey()
	if _, err := p.ActAtFile(ctx, "demo-proj", path, "importversion", vals("rsa-oaep-3072-sha256", kmsMaterialPlain, ""),
		console.UploadedFile{Name: "key.bin", Reader: bytes.NewReader(k2)}); err != nil {
		t.Fatalf("Import version, plain file: %v", err)
	}
	check(ring+"/cryptoKeys/mac/cryptoKeyVersions/2", k2)

	// 3: wrapped elsewhere, with RSA-OAEP alone, sent as a file.
	k3 := newKey()
	b, _ := pem.Decode([]byte(func() string {
		j, _ := c.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: ring + "/importJobs/rsa-oaep-3072-sha256"})
		return j.GetPublicKey().GetPem()
	}()))
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub.(*rsa.PublicKey), k3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ActAtFile(ctx, "demo-proj", path, "importversion", vals("rsa-oaep-3072-sha256", kmsMaterialWrapped, ""),
		console.UploadedFile{Name: "wrapped.bin", Reader: bytes.NewReader(wrapped)}); err != nil {
		t.Fatalf("Import version, wrapped file: %v", err)
	}
	check(ring+"/cryptoKeys/mac/cryptoKeyVersions/3", k3)

	// Plain material sent as wrapped is refused with ImportCryptoKeyVersion's
	// message, and imports nothing.
	_, sdkErr := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: ring + "/cryptoKeys/mac",
		ImportJob: aesJob, Algorithm: kmspb.CryptoKeyVersion_HMAC_SHA256, WrappedKey: k1})
	err = p.ActAt(ctx, "demo-proj", path, "importversion", vals("rsa-oaep-3072-sha256-aes-256", kmsMaterialWrapped, base64.StdEncoding.EncodeToString(k1)))
	if err == nil || sdkErr == nil || consoleErr(err) != consoleErr(sdkErr) {
		t.Errorf("Import version of unwrapped material as wrapped = %v; want ImportCryptoKeyVersion's %v", err, sdkErr)
	}
	if err := p.ActAt(ctx, "demo-proj", path, "importversion", vals("rsa-oaep-3072-sha256", kmsMaterialPlain, "")); err == nil {
		t.Error("Import version with no key material was accepted")
	}
	if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: ring + "/cryptoKeys/mac/cryptoKeyVersions/4"}); err == nil {
		t.Errorf("a refused import left version %v", v)
	}
}

// A deleted key leaves a retired resource, which the ring's Retired
// resources tab lists, as ListRetiredResources returns it, and whose page is
// what GetRetiredResource returns. Another ring's retired keys are not
// listed (#1219).
func TestConsoleKMSRetiredResources(t *testing.T) {
	ctx := context.Background()
	p, c, ring := kmsConsole(t)
	other, err := p.Create(ctx, "demo-proj", map[string]string{"keyRingId": "other", "location": "global"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "demo-proj", []string{ring})
	if err != nil {
		t.Fatal(err)
	}
	if r := kmsSection(t, d, "retired").Listing; r.Total != 0 || r.Note == "" {
		t.Errorf("a ring with no deleted key lists %+v", r)
	}
	for _, r := range []string{ring, other} {
		if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r, CryptoKeyId: "gone",
			SkipInitialVersionCreation: true, CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); err != nil {
			t.Fatal(err)
		}
		if err := p.ActAt(ctx, "demo-proj", []string{r, "gone"}, "deletekey", nil); err != nil {
			t.Fatalf("Delete key: %v", err)
		}
	}
	d, err = p.Detail(ctx, "demo-proj", []string{ring})
	if err != nil {
		t.Fatal(err)
	}
	retired := kmsSection(t, d, "retired").Listing
	if retired.Total != 1 || retired.Items[0].Fields["Original resource"] != ring+"/cryptoKeys/gone" {
		t.Fatalf("Retired resources = %+v; want this ring's deleted key only", retired)
	}
	id := retired.Items[0].Name
	sdk, err := c.GetRetiredResource(ctx, &kmspb.GetRetiredResourceRequest{Name: "projects/demo-proj/locations/global/retiredResources/" + id})
	if err != nil || sdk.GetOriginalResource() != ring+"/cryptoKeys/gone" {
		t.Fatalf("GetRetiredResource = %v, %v", sdk, err)
	}
	rd, err := p.Detail(ctx, "demo-proj", retired.Items[0].Opens)
	if err != nil || rd.Unavailable != "" || len(rd.Summary) == 0 || rd.Summary[0].Value != sdk.GetOriginalResource() {
		t.Fatalf("retired resource page = %+v, %v", rd, err)
	}
	// The other ring's retired key is not this ring's page.
	od, _ := p.Detail(ctx, "demo-proj", []string{other})
	oid := kmsSection(t, od, "retired").Listing.Items[0].Name
	if d, _ := p.Detail(ctx, "demo-proj", []string{ring, kmsRetiredSegment, oid}); d.Unavailable == "" {
		t.Error("a ring's page opens another ring's retired resource")
	}
}

// /admin/reset of Cloud KMS waits for the server: a sweep or a write in
// flight holds a record it read before the delete and would write it back
// after, leaving the reset incomplete (TestKMSResources).
func TestKMSResetWaitsForTheServer(t *testing.T) {
	p, _, _ := kmsConsole(t)
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = p.api().Exclusive(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	done := make(chan error, 1)
	go func() { done <- (&kmsResetter{svc: p.svc}).Reset(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("Reset ran while the server was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rings, err := p.api().KeyRingsInProject("demo-proj"); err != nil || len(rings) != 0 {
		t.Errorf("after Reset the project has rings %v (%v)", rings, err)
	}
}
