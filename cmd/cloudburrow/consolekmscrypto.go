package main

import (
	"context"
	"encoding/base64"
	"fmt"
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
