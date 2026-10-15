package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
)

// The console's actions for the key purposes beyond ENCRYPT_DECRYPT, random
// bytes and deletion (#1212). Each is one call on the in-process kms.Server,
// the one an SDK reaches, except Verify and Encrypt with public key, which
// Cloud KMS leaves to the client: they run Go's standard crypto against the
// GetPublicKey PEM here, as a client would. Inputs and outputs are shown in
// the dialog and recorded nowhere.

// kmsVersionCryptoActions are what an ENABLED version of a key of purpose p
// offers on its page.
func kmsVersionCryptoActions(p kmspb.CryptoKey_CryptoKeyPurpose) []console.Action {
	data := console.Field{Name: "data", Label: "Data", Type: "textarea", Required: true, Help: "Taken as UTF-8."}
	b64 := func(name, label, help string) console.Field {
		return console.Field{Name: name, Label: label, Type: "textarea", Required: true, Help: help}
	}
	// Get public key asks for its format, so its answer has a dialog to be
	// shown in: an action with no fields is performed on click.
	pub := console.Action{ID: "publickey", Label: "Get public key", Fields: []console.Field{
		{Name: "format", Label: "Format", Type: "select", Default: "PEM", Options: []string{"PEM", "DER"},
			Help: "PEM is shown as text, DER base64-encoded."}}}
	switch p {
	case kmspb.CryptoKey_ASYMMETRIC_SIGN:
		return []console.Action{
			{ID: "sign", Label: "Sign", Fields: []console.Field{data}},
			{ID: "verify", Label: "Verify", Fields: []console.Field{data,
				b64("signature", "Signature (base64)", "As Sign returned it. Checked against the public key with standard crypto, as a client would: Cloud KMS has no verify method.")}},
			pub,
		}
	case kmspb.CryptoKey_ASYMMETRIC_DECRYPT:
		return []console.Action{
			{ID: "pubencrypt", Label: "Encrypt with public key", Fields: []console.Field{
				{Name: "plaintext", Label: "Plaintext", Type: "textarea", Required: true,
					Help: "Encrypted here with RSA-OAEP under the version's public key, as a client would: Cloud KMS has no asymmetric encrypt method."}}},
			{ID: "asymdecrypt", Label: "Decrypt", Fields: []console.Field{
				b64("ciphertext", "Ciphertext (base64)", "RSA-OAEP ciphertext made with this version's public key.")}},
			pub,
		}
	case kmspb.CryptoKey_MAC:
		return []console.Action{
			{ID: "macsign", Label: "MAC sign", Fields: []console.Field{data}},
			{ID: "macverify", Label: "MAC verify", Fields: []console.Field{data,
				b64("mac", "MAC (base64)", "As MAC sign returned it.")}},
		}
	case kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT:
		aad := console.Field{Name: "aad", Label: "Additional authenticated data", Type: "text", Help: "Optional; AES-GCM only."}
		return []console.Action{
			{ID: "rawencrypt", Label: "Raw encrypt", Fields: []console.Field{
				{Name: "plaintext", Label: "Plaintext", Type: "textarea", Required: true, Help: "Taken as UTF-8. AES-CBC needs a multiple of 16 bytes."},
				aad,
				{Name: "iv", Label: "Initialization vector (base64)", Type: "text", Help: "Optional; AES-CBC and AES-CTR only, 16 bytes. Generated when empty."}}},
			{ID: "rawdecrypt", Label: "Raw decrypt", Fields: []console.Field{
				b64("ciphertext", "Ciphertext (base64)", "As Raw encrypt returned it."),
				aad,
				{Name: "iv", Label: "Initialization vector (base64)", Type: "text", Required: true}}},
		}
	case kmspb.CryptoKey_KEY_ENCAPSULATION:
		pub.Fields = []console.Field{{Name: "format", Label: "Format", Type: "select", Default: "NIST_PQC", Options: []string{"NIST_PQC"},
			Help: "FIPS 203's encoding, base64-encoded: the format post-quantum keys need."}}
		return []console.Action{
			{ID: "decapsulate", Label: "Decapsulate", Fields: []console.Field{
				b64("ciphertext", "Ciphertext (base64)", "An ML-KEM ciphertext made with this version's public key.")}},
			pub,
		}
	}
	return nil
}

func kmsB64(field, v string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("%s is not base64: %v", field, err)
	}
	return b, nil
}

func kmsB64Result(label string, b []byte) *console.Listing {
	return kmsResult(label, base64.StdEncoding.EncodeToString(b), "")
}

// kmsVersionCryptoAct performs one of kmsVersionCryptoActions on version; ok
// is false for an action it does not know.
func kmsVersionCryptoAct(ctx context.Context, api *kms.Server, version, action string, values map[string]string) (*console.Listing, bool, error) {
	data := []byte(values["data"])
	switch action {
	case "sign":
		r, err := api.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: version, Data: data})
		if err != nil {
			return nil, true, err
		}
		return kmsB64Result("Signature", r.GetSignature()), true, nil
	case "verify":
		sig, err := kmsB64("signature", values["signature"])
		if err != nil {
			return nil, true, err
		}
		pk, err := api.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version})
		if err != nil {
			return nil, true, err
		}
		ok, err := kms.VerifySignature(pk.GetPem(), pk.GetAlgorithm(), data, sig)
		if err != nil {
			return nil, true, err
		}
		return kmsResult("Signature", map[bool]string{true: "Valid", false: "Invalid"}[ok], ""), true, nil
	case "publickey":
		format := strings.TrimSpace(values["format"])
		if format == "" {
			format = "PEM"
		}
		f, ok := kmspb.PublicKey_PublicKeyFormat_value[format]
		if !ok {
			return nil, true, fmt.Errorf("format: %q is not a public key format", format)
		}
		pk, err := api.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version, PublicKeyFormat: kmspb.PublicKey_PublicKeyFormat(f)})
		if err != nil {
			return nil, true, err
		}
		if format == "PEM" {
			return kmsResult("Public key (PEM)", pk.GetPem(), ""), true, nil
		}
		return kmsB64Result("Public key ("+format+", base64)", pk.GetPublicKey().GetData()), true, nil
	case "pubencrypt":
		pk, err := api.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version})
		if err != nil {
			return nil, true, err
		}
		ct, err := kms.EncryptWithPublicKey(pk.GetPem(), pk.GetAlgorithm(), []byte(values["plaintext"]))
		if err != nil {
			return nil, true, err
		}
		return kmsB64Result("Ciphertext", ct), true, nil
	case "asymdecrypt":
		ct, err := kmsB64("ciphertext", values["ciphertext"])
		if err != nil {
			return nil, true, err
		}
		r, err := api.AsymmetricDecrypt(ctx, &kmspb.AsymmetricDecryptRequest{Name: version, Ciphertext: ct})
		if err != nil {
			return nil, true, err
		}
		return kmsResult("Plaintext", string(r.GetPlaintext()), ""), true, nil
	case "macsign":
		r, err := api.MacSign(ctx, &kmspb.MacSignRequest{Name: version, Data: data})
		if err != nil {
			return nil, true, err
		}
		return kmsB64Result("MAC", r.GetMac()), true, nil
	case "macverify":
		mac, err := kmsB64("mac", values["mac"])
		if err != nil {
			return nil, true, err
		}
		r, err := api.MacVerify(ctx, &kmspb.MacVerifyRequest{Name: version, Data: data, Mac: mac})
		if err != nil {
			return nil, true, err
		}
		return kmsResult("MAC", map[bool]string{true: "Valid", false: "Invalid"}[r.GetSuccess()], ""), true, nil
	case "rawencrypt":
		var iv []byte
		if strings.TrimSpace(values["iv"]) != "" {
			b, err := kmsB64("initialization vector", values["iv"])
			if err != nil {
				return nil, true, err
			}
			iv = b
		}
		r, err := api.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: version, Plaintext: []byte(values["plaintext"]),
			AdditionalAuthenticatedData: []byte(values["aad"]), InitializationVector: iv})
		if err != nil {
			return nil, true, err
		}
		out := kmsB64Result("Ciphertext", r.GetCiphertext())
		out.Columns = append(out.Columns, "Initialization vector")
		out.Items[0].Fields["Initialization vector"] = base64.StdEncoding.EncodeToString(r.GetInitializationVector())
		return out, true, nil
	case "rawdecrypt":
		ct, err := kmsB64("ciphertext", values["ciphertext"])
		if err != nil {
			return nil, true, err
		}
		iv, err := kmsB64("initialization vector", values["iv"])
		if err != nil {
			return nil, true, err
		}
		r, err := api.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: version, Ciphertext: ct, InitializationVector: iv,
			AdditionalAuthenticatedData: []byte(values["aad"])})
		if err != nil {
			return nil, true, err
		}
		return kmsResult("Plaintext", string(r.GetPlaintext()), ""), true, nil
	case "decapsulate":
		ct, err := kmsB64("ciphertext", values["ciphertext"])
		if err != nil {
			return nil, true, err
		}
		r, err := api.Decapsulate(ctx, &kmspb.DecapsulateRequest{Name: version, Ciphertext: ct})
		if err != nil {
			return nil, true, err
		}
		return kmsB64Result("Shared secret", r.GetSharedSecret()), true, nil
	case "deleteversion":
		op, err := api.DeleteCryptoKeyVersion(ctx, &kmspb.DeleteCryptoKeyVersionRequest{Name: version})
		if err == nil && op.GetError() != nil {
			err = fmt.Errorf("%s", op.GetError().GetMessage())
		}
		return nil, true, err
	}
	return nil, false, nil
}

// kmsRandomBytesAction generates random bytes in a ring's location.
var kmsRandomBytesAction = console.Action{ID: "randombytes", Label: "Generate random bytes", Fields: []console.Field{
	{Name: "length", Label: "Length in bytes", Type: "text", Required: true, Default: "32", Pattern: `^[0-9]{1,4}$`,
		Help: "8 to 1024. Generated at protection level HSM, the only one Cloud KMS documents for this method; locally they come from Go's crypto/rand."},
}}

func kmsRandomBytes(ctx context.Context, api *kms.Server, ring string, values map[string]string) (*console.Listing, error) {
	n, err := strconv.Atoi(strings.TrimSpace(values["length"]))
	if err != nil {
		return nil, fmt.Errorf("length: %q is not a number", values["length"])
	}
	r, err := api.GenerateRandomBytes(ctx, &kmspb.GenerateRandomBytesRequest{Location: strings.Join(strings.SplitN(ring, "/", 5)[:4], "/"), LengthBytes: int32(n),
		ProtectionLevel: kmspb.ProtectionLevel_HSM})
	if err != nil {
		return nil, err
	}
	return kmsB64Result("Random bytes", r.GetData()), nil
}

// kmsPurposeField and kmsAlgorithmField are Create key's choices: every
// purpose and algorithm the service implements.
func kmsPurposeField() console.Field {
	return console.Field{Name: "purpose", Label: "Purpose", Type: "select", Default: "ENCRYPT_DECRYPT", Options: kms.Purposes(),
		Help: "ENCRYPT_DECRYPT is symmetric encryption; the others need an algorithm of their own."}
}

func kmsAlgorithmField() console.Field {
	opts := []string{""}
	for _, p := range kms.Purposes() {
		opts = append(opts, kms.Algorithms(kmspb.CryptoKey_CryptoKeyPurpose(kmspb.CryptoKey_CryptoKeyPurpose_value[p]))...)
	}
	return console.Field{Name: "algorithm", Label: "Algorithm", Type: "select", Options: opts,
		Help: "Empty is GOOGLE_SYMMETRIC_ENCRYPTION, for ENCRYPT_DECRYPT. Another purpose needs one of its own: " +
			"EC_SIGN_* and RSA_SIGN_* sign, RSA_DECRYPT_OAEP_* decrypt, HMAC_* MAC, AES_* raw encryption, ML_KEM_* key encapsulation."}
}

// Import jobs and retired resources (#1219). An import job's page is
// [ring, kmsImportJobsSegment, job ID] and a retired key's is
// [ring, kmsRetiredSegment, retired resource ID]: a key ID is letters,
// digits, - and _, so neither segment can name a key.
const (
	kmsImportJobsSegment = "~importJobs"
	kmsRetiredSegment    = "~retired"
)

// kmsImportMethods are the import methods CreateImportJob implements, the
// RSA-AES ones first: they carry keys of any size.
var kmsImportMethods = []string{
	"RSA_OAEP_3072_SHA1_AES_256", "RSA_OAEP_4096_SHA1_AES_256",
	"RSA_OAEP_3072_SHA256_AES_256", "RSA_OAEP_4096_SHA256_AES_256",
	"RSA_OAEP_3072_SHA256", "RSA_OAEP_4096_SHA256",
}

// kmsCreateImportJobAction is Create import job on a ring's page.
func kmsCreateImportJobAction() console.Action {
	return console.Action{ID: "createimportjob", Label: "Create import job", Fields: []console.Field{
		{Name: "importJobId", Label: "Import job name", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_\-]{1,63}$`,
			Help: "1-63 letters, digits, hyphens and underscores. An import job cannot be deleted; it expires three days after it is created."},
		{Name: "importMethod", Label: "Import method", Type: "select", Default: kmsImportMethods[0], Options: kmsImportMethods,
			Help: "How key material is wrapped for this job. The *_AES_256 methods wrap an ephemeral AES-256 key with RSA-OAEP and the key with " +
				"AES-KWP, and carry any key; the others wrap the key with RSA-OAEP alone, so cannot carry an RSA key. The HPKE methods are not implemented."},
		{Name: "protectionLevel", Label: "Protection level", Type: "select", Default: "SOFTWARE", Options: []string{"SOFTWARE"},
			Help: "Only SOFTWARE keys exist locally; HSM and EXTERNAL are not implemented."},
	}}
}

func kmsCreateImportJob(ctx context.Context, api *kms.Server, ring string, values map[string]string) error {
	m, ok := kmspb.ImportJob_ImportMethod_value[strings.TrimSpace(values["importMethod"])]
	if !ok {
		return fmt.Errorf("import method: %q is not an import method", values["importMethod"])
	}
	pl := strings.TrimSpace(values["protectionLevel"])
	if pl == "" {
		pl = "SOFTWARE"
	}
	lvl, ok := kmspb.ProtectionLevel_value[pl]
	if !ok {
		return fmt.Errorf("protection level: %q is not a protection level", pl)
	}
	_, err := api.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{
		Parent: ring, ImportJobId: strings.TrimSpace(values["importJobId"]),
		ImportJob: &kmspb.ImportJob{ImportMethod: kmspb.ImportJob_ImportMethod(m), ProtectionLevel: kmspb.ProtectionLevel(lvl)},
	})
	return err
}

// kmsImportJobsSection is the Import jobs tab of a ring's page.
func kmsImportJobsSection(ctx context.Context, api *kms.Server, ring string) console.Section {
	out := console.Listing{
		Columns:    []string{"Import method", "Protection level", "Created", "Expires"},
		NameColumn: "Import job", Noun: "import jobs", AlwaysStatus: true,
	}
	resp, err := api.ListImportJobs(ctx, &kmspb.ListImportJobsRequest{Parent: ring, PageSize: detailLimit})
	if err != nil {
		out.Unavailable = "cannot list import jobs: " + consoleErr(err)
		return console.Section{ID: "importjobs", Label: "Import jobs", Listing: out}
	}
	for _, j := range resp.GetImportJobs() {
		id := lastSegment(j.GetName())
		out.Items = append(out.Items, console.Resource{
			Name: id, Status: j.GetState().String(),
			Opens: []string{ring, kmsImportJobsSegment, id},
			Fields: map[string]string{
				"Import method":    j.GetImportMethod().String(),
				"Protection level": j.GetProtectionLevel().String(),
				"Created":          kmsTime(j.GetCreateTime()),
				"Expires":          kmsTime(j.GetExpireTime()),
			},
		})
	}
	out.Total = len(out.Items)
	if int(resp.GetTotalSize()) > len(out.Items) {
		out.Note = truncatedNote(len(out.Items), "import jobs")
	} else if out.Total == 0 {
		out.Note = "This key ring has no import jobs. Create import job makes one; a key's Import version brings key material in through it."
	}
	return console.Section{ID: "importjobs", Label: "Import jobs", Listing: out}
}

// kmsImportJobDetail is an import job's page: what GetImportJob returns,
// with the wrapping public key's PEM.
func kmsImportJobDetail(ctx context.Context, api *kms.Server, ring, id string) console.Detail {
	trail := []console.Crumb{{Label: lastSegment(ring), Path: []string{ring}}, {Label: id}}
	j, err := api.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: ring + "/importJobs/" + id})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the import job: " + consoleErr(err), Trail: trail}
	}
	d := console.Detail{
		Trail: trail,
		Summary: []console.Property{
			{Label: "State", Value: j.GetState().String()},
			{Label: "Import method", Value: j.GetImportMethod().String()},
			{Label: "Expires", Value: kmsTime(j.GetExpireTime())},
		},
		Sections: []console.Section{
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Import job", Properties: []console.Property{
					{Label: "Resource name", Value: j.GetName()},
					{Label: "State", Value: j.GetState().String()},
					{Label: "Import method", Value: j.GetImportMethod().String()},
					{Label: "Protection level", Value: j.GetProtectionLevel().String()},
					{Label: "Created", Value: kmsTime(j.GetCreateTime())},
					{Label: "Generated", Value: kmsTime(j.GetGenerateTime())},
					{Label: "Expires", Value: kmsTime(j.GetExpireTime())},
				}}},
				Note: "An ACTIVE job imports key material through a key's Import version until it expires."},
			{ID: "publickey", Label: "Wrapping public key", Kind: console.KindText, Text: j.GetPublicKey().GetPem(),
				Note: "Wrap key material under this key, as the import method names, to import it; or let Import version wrap it here."},
		},
	}
	if j.GetExpireEventTime() != nil {
		g := &d.Sections[0].Groups[0]
		g.Properties = append(g.Properties, console.Property{Label: "Expired", Value: kmsTime(j.GetExpireEventTime())})
	}
	return d
}

// kmsImportVersionAction is Import version on a key's page: a choice of the
// ring's ACTIVE import jobs, and none when it has no such job.
func kmsImportVersionAction(ctx context.Context, api *kms.Server, ring string, k *kmspb.CryptoKey) (console.Action, bool) {
	resp, err := api.ListImportJobs(ctx, &kmspb.ListImportJobsRequest{Parent: ring, Filter: "state = ACTIVE", PageSize: detailLimit})
	if err != nil || len(resp.GetImportJobs()) == 0 {
		return console.Action{}, false
	}
	var jobs []string
	for _, j := range resp.GetImportJobs() {
		jobs = append(jobs, lastSegment(j.GetName()))
	}
	algs := kms.Algorithms(k.GetPurpose())
	// The key's own algorithm first: its versions are usually of it.
	def := algs[0]
	if a := k.GetVersionTemplate().GetAlgorithm().String(); slices.Contains(algs, a) {
		def = a
	}
	return console.Action{ID: "importversion", Label: "Import version", Fields: []console.Field{
		{Name: "importJob", Label: "Import job", Type: "select", Required: true, Default: jobs[0], Options: jobs,
			Help: "An ACTIVE import job of this key ring."},
		{Name: "algorithm", Label: "Algorithm", Type: "select", Required: true, Default: def, Options: algs,
			Help: "What the key material is; it must suit the key's purpose. Symmetric keys are raw bytes, asymmetric ones PKCS #8 DER."},
		{Name: "format", Label: "Key material", Type: "select", Default: kmsMaterialWrapped,
			Options: []string{kmsMaterialWrapped, kmsMaterialPlain},
			Help: "Wrapped: already wrapped under the job's public key, as its import method names. Plain: the key itself, which the console " +
				"wraps here under the job's public key (RSA-OAEP, then AES-KWP for the *_AES_256 methods), as gcloud's --target-key-file does."},
		{Name: "file", Label: "Key material file", Type: console.FileFieldType,
			Help: "The key material as a binary file. Leave empty to give it as base64 below."},
		{Name: "material", Label: "Key material (base64)", Type: "textarea",
			Help: "Used when no file is chosen. Shown nowhere and recorded nowhere."},
	}}, true
}

const (
	kmsMaterialWrapped = "WRAPPED"
	kmsMaterialPlain   = "PLAIN"
)

// kmsImportMaterialLimit bounds key material read from a file: a wrapped
// 4096-bit RSA key in PKCS #8 is under 4 KiB.
const kmsImportMaterialLimit = 64 << 10

// kmsImportVersion imports material, wrapped or plain, into key through the
// ring's import job.
func kmsImportVersion(ctx context.Context, api *kms.Server, key string, values map[string]string, material []byte) error {
	if len(material) == 0 {
		return errors.New("choose a key material file or give the key material as base64")
	}
	a, ok := kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm_value[strings.TrimSpace(values["algorithm"])]
	if !ok {
		return fmt.Errorf("algorithm: %q is not a key algorithm", values["algorithm"])
	}
	job := kmsRingOf(key) + "/importJobs/" + strings.TrimSpace(values["importJob"])
	wrapped := material
	switch f := strings.TrimSpace(values["format"]); f {
	case "", kmsMaterialWrapped:
	case kmsMaterialPlain:
		j, err := api.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: job})
		if err != nil {
			return err
		}
		if wrapped, err = kms.WrapForImport(j.GetPublicKey().GetPem(), j.GetImportMethod(), material); err != nil {
			return fmt.Errorf("wrap the key material under %s: %v", j.GetName(), err)
		}
	default:
		return fmt.Errorf("key material: %q is neither %s nor %s", f, kmsMaterialWrapped, kmsMaterialPlain)
	}
	_, err := api.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{
		Parent: key, ImportJob: job, Algorithm: kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm(a),
		WrappedKey: wrapped,
	})
	return err
}

// kmsRingOf returns the ring a key name belongs to.
func kmsRingOf(key string) string {
	if i := strings.Index(key, "/cryptoKeys/"); i >= 0 {
		return key[:i]
	}
	return key
}

// kmsRetiredSection is the Retired resources tab of a ring's page: the
// ring's deleted keys, from ListRetiredResources on its location. A retired
// resource keeps a deleted key's name from being used again.
func kmsRetiredSection(ctx context.Context, api *kms.Server, ring string) console.Section {
	out := console.Listing{
		Columns:    []string{"Original resource", "Resource type", "Deleted"},
		NameColumn: "Retired resource", Noun: "retired resources",
	}
	location := strings.Join(strings.SplitN(ring, "/", 5)[:4], "/")
	token := ""
	for {
		resp, err := api.ListRetiredResources(ctx, &kmspb.ListRetiredResourcesRequest{Parent: location, PageToken: token})
		if err != nil {
			out.Unavailable = "cannot list retired resources: " + consoleErr(err)
			break
		}
		for _, r := range resp.GetRetiredResources() {
			if !strings.HasPrefix(r.GetOriginalResource(), ring+"/") {
				continue
			}
			id := lastSegment(r.GetName())
			out.Items = append(out.Items, console.Resource{
				Name: id, Opens: []string{ring, kmsRetiredSegment, id},
				Fields: map[string]string{
					"Original resource": r.GetOriginalResource(),
					"Resource type":     r.GetResourceType(),
					"Deleted":           kmsTime(r.GetDeleteTime()),
				},
			})
		}
		token = resp.GetNextPageToken()
		if token == "" || len(out.Items) >= detailLimit {
			break
		}
	}
	out.Total = len(out.Items)
	if out.Total == 0 && out.Unavailable == "" {
		out.Note = "No key of this ring has been deleted. A deleted key leaves a retired resource here, and its name can never be used again."
	}
	return console.Section{ID: "retired", Label: "Retired resources", Listing: out}
}

// kmsRetiredDetail is a retired resource's page, from GetRetiredResource.
func kmsRetiredDetail(ctx context.Context, api *kms.Server, ring, id string) console.Detail {
	trail := []console.Crumb{{Label: lastSegment(ring), Path: []string{ring}}, {Label: id}}
	location := strings.Join(strings.SplitN(ring, "/", 5)[:4], "/")
	r, err := api.GetRetiredResource(ctx, &kmspb.GetRetiredResourceRequest{Name: location + "/retiredResources/" + id})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the retired resource: " + consoleErr(err), Trail: trail}
	}
	if !strings.HasPrefix(r.GetOriginalResource(), ring+"/") {
		return console.Detail{Unavailable: r.GetName() + " is not a retired resource of " + ring, Trail: trail}
	}
	return console.Detail{
		Trail: trail,
		Summary: []console.Property{
			{Label: "Original resource", Value: r.GetOriginalResource()},
			{Label: "Deleted", Value: kmsTime(r.GetDeleteTime())},
		},
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Retired resource", Properties: []console.Property{
				{Label: "Resource name", Value: r.GetName()},
				{Label: "Original resource", Value: r.GetOriginalResource()},
				{Label: "Resource type", Value: r.GetResourceType()},
				{Label: "Deleted", Value: kmsTime(r.GetDeleteTime())},
			}}},
			Note: "The original resource's name cannot be used again in Cloud KMS."}},
	}
}

// ActAtFile implements console.FileActor: Import version with its key
// material in a file.
func (p kmsProvider) ActAtFile(ctx context.Context, project string, path []string, action string, values map[string]string, file console.UploadedFile) (*console.Listing, error) {
	api := p.api()
	if api == nil {
		return nil, errors.New(kmsNotStarted)
	}
	if action != "importversion" || len(path) != 2 {
		return nil, fmt.Errorf("unknown action %q with a file", action)
	}
	ring, err := kmsRing(project, path)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(file, kmsImportMaterialLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > kmsImportMaterialLimit {
		return nil, fmt.Errorf("the key material file is larger than %d KiB", kmsImportMaterialLimit>>10)
	}
	return nil, kmsImportVersion(ctx, api, ring+"/cryptoKeys/"+path[1], values, b)
}

var _ console.FileActor = kmsProvider{}
