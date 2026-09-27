package secrets

import (
	"hash/crc32"
	"strings"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Request fields Google honours that this server does not, refused by name
// rather than accepted and dropped (#580): a secret created with a ttl that
// never expires, or rotation that never fires, would let code pass locally
// that behaves differently on Google. Both transports call these, so a body
// gets the same answer over gRPC and REST.

// checkCreatable refuses a Secret whose fields this server would not keep.
func checkCreatable(sec *secretmanagerpb.Secret) error {
	unsupported := []struct {
		field string
		set   bool
	}{
		{"expire_time", sec.GetExpireTime() != nil},
		{"ttl", sec.GetTtl() != nil},
		{"rotation", sec.GetRotation() != nil},
		{"topics", len(sec.GetTopics()) > 0},
		{"version_aliases", len(sec.GetVersionAliases()) > 0},
		{"version_destroy_ttl", sec.GetVersionDestroyTtl() != nil},
		{"customer_managed_encryption", sec.GetCustomerManagedEncryption() != nil},
		{"tags", len(sec.GetTags()) > 0},
		{"replication.automatic.customer_managed_encryption", sec.GetReplication().GetAutomatic().GetCustomerManagedEncryption() != nil},
	}
	for _, u := range unsupported {
		if u.set {
			return apierror.Unimplemented("Secret.%s is not implemented by CloudBurrow's Secret Manager; "+
				"the secret would be stored without it, so the request is refused", u.field)
		}
	}
	if um := sec.GetReplication().GetUserManaged(); um != nil {
		if len(um.GetReplicas()) == 0 {
			return apierror.InvalidArgument("replication.user_managed.replicas must name at least one replica")
		}
		for _, r := range um.GetReplicas() {
			if r.GetLocation() == "" {
				return apierror.InvalidArgument("replication.user_managed.replicas[].location is required")
			}
			if r.GetCustomerManagedEncryption() != nil {
				return apierror.Unimplemented("Secret.replication.user_managed.replicas[].customer_managed_encryption " +
					"is not implemented by CloudBurrow's Secret Manager")
			}
		}
	}
	return nil
}

// checkNoFilter refuses a list filter: the list would otherwise return
// every resource and look like a filter that matched them all.
func checkNoFilter(filter string) error {
	if strings.TrimSpace(filter) != "" {
		return apierror.Unimplemented("list filter %q is not implemented by CloudBurrow's Secret Manager; "+
			"list without a filter and select client-side", filter)
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
