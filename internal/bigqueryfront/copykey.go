package bigqueryfront

import (
	"encoding/json"
	"strings"
)

// A copy job's destinationEncryptionConfiguration (#1762).
//
// BigQuery's documentation (customer-managed-encryption, read 2026-11-18,
// https://docs.cloud.google.com/bigquery/docs/customer-managed-encryption):
//
//   - bq cp --destination_kms_key "copy a table protected by default
//     encryption into a new table, or into the original table, protected
//     by Cloud KMS", and the Go, Java and Python samples set the copy job's
//     destinationEncryptionConfiguration: the destination's
//     encryptionConfiguration is the key given.
//   - "If you use cp, you can change the Cloud KMS key used for a
//     CMEK-protected table, change a table from default encryption to
//     CMEK-protection, or change a table from CMEK-protection to default
//     encryption": the last by copying the table "to itself by running bq
//     cp without using the --destination_kms_key flag". So a WRITE_TRUNCATE
//     gives the table the key given, or none.
//   - "Existing table encryption settings don't match encryption settings
//     specified in the request": "use the TRUNCATE write disposition to
//     replace the table, or specify a different destination table". A
//     copy given a key into a table that exists and is kept (WRITE_APPEND,
//     or WRITE_EMPTY into an empty one) and is encrypted otherwise, by
//     another key or by default encryption, fails the job.
//   - "This region is not supported": "The region of the Cloud KMS key
//     doesn't match the region of the BigQuery dataset for the destination
//     table". A key of the global location is accepted ("you can use it
//     with the bq tool or GoogleSQL").
//   - The key is "in the form:
//     projects/KMS_PROJECT_ID/locations/LOCATION/keyRings/KEY_RING/cryptoKeys/KEY",
//     and "You cannot specify a key resource ID that includes the
//     /cryptoKeyVersions/ token".
//
// UNVERIFIED (docs re-read 2026-11-18: customer-managed-encryption,
// JobConfigurationTableCopy, error-messages, managing-tables,
// bq-cli-reference's bq cp): the reason of a mismatch or of a key of
// another region (CloudBurrow: invalid, "any type of invalid input other
// than an invalid query"), whether they fail the request or the job
// (CloudBurrow: the job, as a partitioning mismatch), and the message of a
// key not in that form (CloudBurrow: 400 invalid, its own wording). As
// every key in CloudBurrow, it is recorded, not used: the emulator
// encrypts nothing. A key with a partition decorator on a table stays 501.

// copyKeyName reads a copy job's destinationEncryptionConfiguration: its
// kmsKeyName, "" when none is given, and why it cannot be used, or "".
func copyKeyName(raw json.RawMessage) (key, why string) {
	if len(raw) == 0 || isNull(raw) {
		return "", ""
	}
	var c struct {
		KmsKeyName string `json:"kmsKeyName"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return "", "Invalid value for destinationEncryptionConfiguration: " + string(raw)
	}
	if c.KmsKeyName == "" {
		return "", ""
	}
	if strings.Contains(c.KmsKeyName, "/cryptoKeyVersions/") {
		// UNVERIFIED wording (above).
		return "", "Invalid value for destinationEncryptionConfiguration.kmsKeyName: " + c.KmsKeyName +
			". A key resource ID cannot include /cryptoKeyVersions/."
	}
	if keyLocation(c.KmsKeyName) == "" {
		// UNVERIFIED wording (above).
		return "", "Invalid value for destinationEncryptionConfiguration.kmsKeyName: " + c.KmsKeyName +
			". The key is in the form projects/KMS_PROJECT_ID/locations/LOCATION/keyRings/KEY_RING/cryptoKeys/KEY."
	}
	return c.KmsKeyName, ""
}

// keyLocation is the location of key, a Cloud KMS key resource ID
// (above), or "" when key is not one.
func keyLocation(key string) string {
	p := strings.Split(key, "/")
	if len(p) != 8 || p[0] != "projects" || p[2] != "locations" || p[4] != "keyRings" || p[6] != "cryptoKeys" {
		return ""
	}
	for _, s := range p {
		if s == "" {
			return ""
		}
	}
	return p[3]
}

// keyRegionMismatch is why key cannot encrypt a table of a dataset in
// location loc (above), or "".
func keyRegionMismatch(key, loc string) string {
	kl := keyLocation(key)
	if strings.EqualFold(kl, "global") || sameLocation(kl, loc) {
		return ""
	}
	return "This region is not supported: the Cloud KMS key " + key + " is in " + kl +
		", and the destination table's dataset is in " + locOrUS(loc) + "."
}

func locOrUS(loc string) string {
	if loc == "" {
		return "US"
	}
	return loc
}

// keyMismatchMessage is BigQuery's message of a key given for a table
// that exists and is encrypted otherwise (above).
const keyMismatchMessage = "Existing table encryption settings don't match encryption settings specified in the request."

// tableKeyOf is the kmsKeyName of a tables.get answer's
// encryptionConfiguration, or "".
func tableKeyOf(got []byte) string {
	var t struct {
		EncryptionConfiguration *struct {
			KmsKeyName string `json:"kmsKeyName"`
		} `json:"encryptionConfiguration"`
	}
	if json.Unmarshal(got, &t) != nil || t.EncryptionConfiguration == nil {
		return ""
	}
	return t.EncryptionConfiguration.KmsKeyName
}
