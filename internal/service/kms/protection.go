package kms

import (
	"strings"

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
// EXTERNAL and EXTERNAL_VPC keys are served with the instance acting as
// the external key manager (external.go, #1469). HSM_SINGLE_TENANT keys
// name an ACTIVE SingleTenantHsmInstance in crypto_key_backend (hsm.go,
// #1469), and are otherwise served as HSM keys are.

// protectionLevelOf validates a requested protection level: unset is
// SOFTWARE; SOFTWARE, HSM, EXTERNAL, EXTERNAL_VPC and HSM_SINGLE_TENANT are
// served.
func protectionLevelOf(field string, pl kmspb.ProtectionLevel) (kmspb.ProtectionLevel, error) {
	switch pl {
	case kmspb.ProtectionLevel_PROTECTION_LEVEL_UNSPECIFIED, kmspb.ProtectionLevel_SOFTWARE:
		return kmspb.ProtectionLevel_SOFTWARE, nil
	case kmspb.ProtectionLevel_HSM, kmspb.ProtectionLevel_EXTERNAL, kmspb.ProtectionLevel_EXTERNAL_VPC, kmspb.ProtectionLevel_HSM_SINGLE_TENANT:
		return pl, nil
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

// Key Access Justifications (#1452, enforced #1506): a key's
// key_access_justifications_policy is stored and returned as set, and its
// encrypt, decrypt and sign operations are checked against it, as
// kmspb.KeyAccessJustificationsPolicy documents: "Zero allowed access
// reasons means all encrypt, decrypt, and sign operations for the
// CryptoKey associated with this policy will fail." Every call to this
// instance is the customer's own, so its reason is CUSTOMER_INITIATED_ACCESS
// ("Customer uses their account to perform any access to their own data
// which their IAM policy authorizes", AccessReason): a policy whose
// allowed_access_reasons lacks it refuses the call. The reasons Google's
// staff and systems carry (GOOGLE_INITIATED_*, THIRD_PARTY_DATA_REQUEST,
// ...) never reach a local key. The refusal's code and message are
// UNVERIFIED: FAILED_PRECONDITION, naming the policy. A key with no policy,
// and the project and organization default policies, which this instance
// does not serve, allow every call.

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

// kajRefusal refuses an encrypt, decrypt or sign operation on k that its
// key_access_justifications_policy does not allow, or returns nil.
func kajRefusal(k cryptoKey) error {
	p := k.KAJPolicy
	if p == nil {
		return nil
	}
	for _, r := range p.Reasons {
		if r == kmspb.AccessReason_CUSTOMER_INITIATED_ACCESS.String() {
			return nil
		}
	}
	if len(p.Reasons) == 0 {
		return apierror.Wrap(apierror.FailedPrecondition("CryptoKey %s's key_access_justifications_policy allows no access reasons, "+
			"so every encrypt, decrypt and sign operation on it fails", k.Name))
	}
	return apierror.Wrap(apierror.FailedPrecondition("CryptoKey %s's key_access_justifications_policy does not allow "+
		"CUSTOMER_INITIATED_ACCESS, the reason of a call made with your own account; it allows %s", k.Name, strings.Join(p.Reasons, ", ")))
}

// kajAllows checks the policy of the key that version name belongs to.
func (s *Server) kajAllows(name string) error {
	keyName, _, err := parseCryptoKeyVersion("name", name)
	if err != nil {
		return apierror.Wrap(err)
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, keyName), &k, "CryptoKey", keyName); err != nil {
		return err
	}
	return kajRefusal(k)
}
