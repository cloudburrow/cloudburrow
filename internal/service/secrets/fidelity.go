package secrets

import (
	"hash/crc32"
	"strings"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Request fields Google honours that this server does not, refused by name
// rather than accepted and dropped (#580): a field kept but never acted on would let code pass locally
// that behaves differently on Google. Both transports call these, so a body
// gets the same answer over gRPC and REST.

// checkCreatable refuses a Secret whose fields this server would not keep.
//
// regional is true for a regional secret (#1519), which takes a
// customer_managed_encryption and no replication.
func checkCreatable(sec *secretmanagerpb.Secret, regional bool) error {
	unsupported := []struct {
		field string
		set   bool
	}{
		{"version_aliases", len(sec.GetVersionAliases()) > 0},
		{"customer_managed_encryption", !regional && sec.GetCustomerManagedEncryption() != nil},
		{"tags", len(sec.GetTags()) > 0},
	}
	for _, u := range unsupported {
		if u.set {
			return apierror.Unimplemented("Secret.%s is not implemented by CloudBurrow's Secret Manager; "+
				"the secret would be stored without it, so the request is refused", u.field)
		}
	}
	if regional {
		if sec.GetReplication() != nil {
			return apierror.InvalidArgument("a regional secret takes no replication policy; its replica is its location")
		}
		if c := sec.GetCustomerManagedEncryption(); c != nil && c.GetKmsKeyName() == "" {
			return apierror.InvalidArgument("customer_managed_encryption.kms_key_name is required")
		}
	}
	if err := checkCMEKFields(sec.GetReplication()); err != nil {
		return err
	}
	if um := sec.GetReplication().GetUserManaged(); um != nil {
		if len(um.GetReplicas()) == 0 {
			return apierror.InvalidArgument("replication.user_managed.replicas must name at least one replica")
		}
		for _, r := range um.GetReplicas() {
			if r.GetLocation() == "" {
				return apierror.InvalidArgument("replication.user_managed.replicas[].location is required")
			}
		}
	}
	return nil
}

// checkEtag compares a request's etag with the stored one. Google fails a
// mismatch with FAILED_PRECONDITION (HTTP 400), per
// docs.cloud.google.com/secret-manager/docs/etags; documented, not measured
// against Google. An empty request etag skips the check, as there.
func checkEtag(requested, current string) error {
	if requested == "" {
		return nil
	}
	if strings.Trim(requested, `"`) != strings.Trim(current, `"`) {
		return apierror.FailedPrecondition("etag %s does not match the current etag %s; read the resource again and retry", requested, current)
	}
	return nil
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// payloadCRC32C is the checksum Google's clients verify on access.
func payloadCRC32C(data []byte) int64 { return int64(crc32.Checksum(data, castagnoli)) }

// checkPayloadCRC32C verifies a client-supplied checksum on AddSecretVersion.
func checkPayloadCRC32C(data []byte, crc *int64) error {
	if crc == nil {
		return nil
	}
	if got := payloadCRC32C(data); got != *crc {
		return apierror.InvalidArgument("payload.data_crc32c is %d, but the payload's CRC32C is %d; the data was corrupted in transit", *crc, got)
	}
	return nil
}

// checkCMEKFields refuses a customer_managed_encryption with no
// kms_key_name, a REQUIRED field (#1453).
func checkCMEKFields(r *secretmanagerpb.Replication) error {
	if c := r.GetAutomatic().GetCustomerManagedEncryption(); c != nil && c.GetKmsKeyName() == "" {
		return apierror.InvalidArgument("replication.automatic.customer_managed_encryption.kms_key_name is required")
	}
	for _, rep := range r.GetUserManaged().GetReplicas() {
		if c := rep.GetCustomerManagedEncryption(); c != nil && c.GetKmsKeyName() == "" {
			return apierror.InvalidArgument("replication.user_managed.replicas[].customer_managed_encryption.kms_key_name is required")
		}
	}
	return nil
}
