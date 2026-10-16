package kms

import (
	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Protection levels (#1452).
//
// SOFTWARE and HSM keys are served. An HSM key is the same key a SOFTWARE
// one is, with the same algorithms and the same operations, and its key
// material is kept where every other key's is: it is NOT in a hardware
// security module, and nothing local is FIPS 140-2 Level 3. What Google
// documents as differing for HSM keys is that the key never leaves the HSM,
// the attestation statement a version carries, and quotas. The attestation
// is a statement signed by the HSM manufacturer's and Google's certificate
// chains, which CloudBurrow cannot produce, so CryptoKeyVersion.attestation
// and ImportJob.attestation are always absent, the FULL view included,
// rather than faked. No quota is applied to either level.
//
// EXTERNAL and EXTERNAL_VPC keys are kept in an external key manager that
// Cloud KMS reaches over the Cloud EKM protocol, which no local key manager
// speaks. #1469 looked for one to run: Google publishes no specification or
// reference server of that protocol, its EKM partners' managers are
// commercial, and no open-source implementation exists (Cosmian KMS serves
// Workspace client-side encryption and Azure's EKM proxy, not Cloud EKM).
// A key manager built here would be guessed behaviour, so they stay
// UNIMPLEMENTED, and so do crypto_key_backend (an
// EkmConnection for EXTERNAL_VPC, or a single-tenant HSM instance) and
// external_protection_level_options. HSM_SINGLE_TENANT needs a single-tenant
// HSM instance, which the HsmManagement service would create; it is
// UNIMPLEMENTED too.

// errExternal is why the external protection levels are UNIMPLEMENTED.
const errExternal = "%s %s is not implemented: EXTERNAL and EXTERNAL_VPC keys live in an external key manager reached over the Cloud EKM protocol, and CloudBurrow runs none"

// protectionLevelOf validates a requested protection level: unset is
// SOFTWARE; SOFTWARE and HSM are served, the others are UNIMPLEMENTED naming
// field.
func protectionLevelOf(field string, pl kmspb.ProtectionLevel) (kmspb.ProtectionLevel, error) {
	switch pl {
	case kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED, kmspb.ProtectionLevel_SOFTWARE:
		return kmspb.ProtectionLevel_SOFTWARE, nil
	case kmspb.ProtectionLevel_HSM:
		return pl, nil
	case kmspb.ProtectionLevel_EXTERNAL, kmspb.ProtectionLevel_EXTERNAL_VPC:
		return 0, apierror.Unimplemented(errExternal, field, pl)
	case kmspb.ProtectionLevel_HSM_SINGLE_TENANT:
		return 0, apierror.Unimplemented("%s %s is not implemented: it needs a single-tenant HSM instance, which CloudBurrow does not provide", field, pl)
	}
	return 0, apierror.InvalidArgument("%s %d is not a ProtectionLevel", field, pl)
}

// storedLevel is how a level is stored: empty for SOFTWARE, so records
// written before HSM existed read unchanged.
func storedLevel(pl kmspb.ProtectionLevel) string {
	if pl == kmspb.ProtectionLevel_SOFTWARE || pl == kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED {
		return ""
	}
	return pl.String()
}

func levelOf(stored string) kmspb.ProtectionLevel {
	if stored == "" {
		return kmspb.ProtectionLevel_SOFTWARE
	}
	return kmspb.ProtectionLevel(kmspb.ProtectionLevel_value[stored])
}

func (k cryptoKey) protectionLevel() kmspb.ProtectionLevel  { return levelOf(k.ProtectionLevel) }
func (v keyVersion) protectionLevel() kmspb.ProtectionLevel { return levelOf(v.ProtectionLevel) }
func (j importJob) protectionLevel() kmspb.ProtectionLevel  { return levelOf(j.ProtectionLevel) }

// Key Access Justifications (#1452): key_access_justifications_policy is
// stored and returned as set, and NOT enforced. An encrypt or decrypt
// request carries no access justification locally (Google attaches one
// to calls its own staff make), so there is nothing to check a policy
// against: every call succeeds whatever the policy allows.

// kajPolicy is a stored key_access_justifications_policy.
type kajPolicy struct {
	// Reasons are the allowed_access_reasons, by enum name.
	Reasons []string `json:"allowedAccessReasons,omitempty"`
}

func kajPolicyOf(p *kmspb.KeyAccessJustificationsPolicy) (*kajPolicy, error) {
	if p == nil {
		return nil, nil
	}
	out := &kajPolicy{}
	for _, r := range p.GetAllowedAccessReasons() {
		if _, ok := kmspb.AccessReason_name[int32(r)]; !ok {
			return nil, apierror.InvalidArgument("key_access_justifications_policy.allowed_access_reasons %d is not an AccessReason", r)
		}
		out.Reasons = append(out.Reasons, r.String())
	}
	return out, nil
}

func (p *kajPolicy) proto() *kmspb.KeyAccessJustificationsPolicy {
	if p == nil {
		return nil
	}
	out := &kmspb.KeyAccessJustificationsPolicy{}
	for _, r := range p.Reasons {
		out.AllowedAccessReasons = append(out.AllowedAccessReasons, kmspb.AccessReason(kmspb.AccessReason_value[r]))
	}
	return out
}
