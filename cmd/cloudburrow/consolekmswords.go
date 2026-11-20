package main

import (
	"fmt"
	"strings"
	"time"
)

// Google's console names Cloud KMS's choices in words, not enums (#1851):
// Create key's Purpose is "Symmetric encrypt/decrypt" and its Algorithm
// "3072 bit RSA - OAEP Padding - SHA256 Digest"
// (https://docs.cloud.google.com/kms/docs/create-key). The forms keep the
// enum as each option's value, so a request reads as the API's, and show
// these words (Field.OptionLabels).

var kmsPurposeLabels = map[string]string{
	"ENCRYPT_DECRYPT":     "Symmetric encrypt/decrypt",
	"ASYMMETRIC_SIGN":     "Asymmetric sign",
	"ASYMMETRIC_DECRYPT":  "Asymmetric decrypt",
	"MAC":                 "MAC signing/verification",
	"RAW_ENCRYPT_DECRYPT": "Raw encrypt/decrypt",
	"KEY_ENCAPSULATION":   "Key encapsulation",
}

var kmsProtectionLabels = map[string]string{
	"SOFTWARE":          "Software",
	"HSM":               "HSM",
	"HSM_SINGLE_TENANT": "Single-tenant HSM",
	"EXTERNAL":          "External",
	"EXTERNAL_VPC":      "External via VPC",
}

// kmsPurposeWords is a purpose as Google's console words it, or the enum
// when it has no wording here.
func kmsPurposeWords(p string) string { return wordsOr(kmsPurposeLabels, p) }

// kmsProtectionWords is a protection level as Google's console words it.
func kmsProtectionWords(p string) string { return wordsOr(kmsProtectionLabels, p) }

func wordsOr(m map[string]string, k string) string {
	if w, ok := m[k]; ok {
		return w
	}
	return k
}

// kmsAlgorithmWords words an algorithm enum the way Google's console does:
// "<bits> bit RSA - <padding> Padding - <digest> Digest", "Elliptic Curve
// P-256 - SHA256 Digest". An enum it does not recognise is shown as itself.
func kmsAlgorithmWords(a string) string {
	switch a {
	case "":
		return "Default for the purpose"
	case "GOOGLE_SYMMETRIC_ENCRYPTION":
		return "Google symmetric key (AES-256-GCM)"
	case "EXTERNAL_SYMMETRIC_ENCRYPTION":
		return "External symmetric key"
	case "EC_SIGN_ED25519":
		return "Elliptic Curve Ed25519"
	case "AES_256_KWP":
		return "AES-256 key wrapping (KWP)"
	case "KEM_XWING":
		return "X-Wing (ML-KEM-768 + X25519)"
	case "PQ_SIGN_SLH_DSA_SHA2_128S":
		return "SLH-DSA-SHA2-128s"
	case "PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256":
		return "HashSLH-DSA-SHA2-128s - SHA256 Digest"
	}
	parts := strings.Split(a, "_")
	switch {
	case strings.HasPrefix(a, "RSA_SIGN_RAW_PKCS1_") && len(parts) == 5:
		return parts[4] + " bit RSA - PKCS#1 v1.5 padding - no digest"
	case strings.HasPrefix(a, "RSA_SIGN_") && len(parts) == 5:
		padding := map[string]string{"PSS": "PSS Padding", "PKCS1": "PKCS#1 v1.5 padding"}[parts[2]]
		if padding == "" {
			break
		}
		return parts[3] + " bit RSA - " + padding + " - " + parts[4] + " Digest"
	case strings.HasPrefix(a, "RSA_DECRYPT_OAEP_") && len(parts) == 5:
		return parts[3] + " bit RSA - OAEP Padding - " + parts[4] + " Digest"
	case strings.HasPrefix(a, "EC_SIGN_") && len(parts) == 4:
		curve := map[string]string{"P256": "P-256", "P384": "P-384", "SECP256K1": "secp256k1"}[parts[2]]
		if curve == "" {
			break
		}
		return "Elliptic Curve " + curve + " - " + parts[3] + " Digest"
	case strings.HasPrefix(a, "HMAC_") && len(parts) == 2:
		return "HMAC " + parts[1]
	case strings.HasPrefix(a, "AES_") && len(parts) == 3:
		return "AES-" + parts[1] + " " + parts[2]
	case strings.HasPrefix(a, "ML_KEM_") && len(parts) == 3:
		return "ML-KEM-" + parts[2]
	case strings.HasPrefix(a, "PQ_SIGN_ML_DSA_") && len(parts) == 5:
		return "ML-DSA-" + parts[4]
	case strings.HasPrefix(a, "PQ_SIGN_ML_DSA_") && strings.HasSuffix(a, "_EXTERNAL_MU") && len(parts) == 7:
		return "ML-DSA-" + parts[4] + " - external μ"
	}
	return a
}

// kmsLabelsFor maps each option to its words.
func kmsLabelsFor(options []string, words func(string) string) map[string]string {
	out := make(map[string]string, len(options))
	for _, o := range options {
		out[o] = words(o)
	}
	return out
}

// kmsRotationPresets are Edit Rotation Period's and Create key's Rotation
// period choices: "Never (manual rotation)" and periods in days, 90 days
// being the console's default.
var kmsRotationPresets = []string{"", "30d", "60d", "90d", "180d", "365d"}

// kmsDefaultRotation is Create key's default Rotation period.
const kmsDefaultRotation = 90 * 24 * time.Hour

// kmsRotationField is a Rotation period select over the presets, with cur
// added when it is none of them.
func kmsRotationField(cur string) []string {
	opts := append([]string(nil), kmsRotationPresets...)
	for _, o := range opts {
		if o == cur {
			return opts
		}
	}
	return append(opts, cur)
}

// kmsPeriodLabel is a rotation period option's words.
func kmsPeriodLabel(v string) string {
	if v == "" {
		return "Never (manual rotation)"
	}
	if d, ok, err := kmsParsePeriod(v); err == nil && ok {
		return kmsPeriodWords(d)
	}
	return v
}

// kmsPeriodWords is a period in days when it is whole days.
func kmsPeriodWords(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		n := int64(d / (24 * time.Hour))
		if n == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", n)
	}
	return d.String()
}
