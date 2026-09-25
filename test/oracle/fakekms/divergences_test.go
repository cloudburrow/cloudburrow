package oracle

// divergences are the known differences between fakekms and CloudBurrow,
// keyed "<step> <path>". Each has a reason and a council-report citation; an
// unlisted difference fails the oracle. fakekms is a reference, not ground
// truth (council report §3 condition 4: "Do not copy its error codes as
// ground truth"): a divergence here is not evidence that CloudBurrow is
// wrong, and agreement is not evidence that it is right.
var divergences = map[string]string{
	"GetCryptoKeyVersion malformed code": "the version ID 01 has a leading zero. CloudBurrow parses names before any " +
		"lookup and answers INVALID_ARGUMENT for a malformed one (#394); fakekms treats it as a missing version, " +
		"NOT_FOUND. Neither code is stated on any KMS page (council report §6, \"Codes for bad names\"), so both stay UNVERIFIED",
	"CreateCryptoKey with labels code": "fakekms's CreateCryptoKey field allowlist leaves out crypto_key.labels, so it " +
		"answers UNIMPLEMENTED. labels is a documented CryptoKey field that Terraform sets on every key " +
		"(goog-terraform-provisioned), and CloudBurrow stores it (council report §4, CreateCryptoKey and UpdateCryptoKey; #405)",
	"update a DESTROY_SCHEDULED version code": "fakekms moves a DESTROY_SCHEDULED version to ENABLED through " +
		"UpdateCryptoKeyVersion; CloudBurrow refuses with FAILED_PRECONDITION, because UpdateCryptoKeyVersion may only " +
		"move a version between ENABLED and DISABLED (S:316-326; council report §4, UpdateCryptoKeyVersion) and leaving " +
		"DESTROY_SCHEDULED is RestoreCryptoKeyVersion's job (S:378-389). CloudBurrow's code is UNVERIFIED",
	"get v2 .response.state": "follows from the step above: fakekms re-enabled the version, CloudBurrow left it DESTROY_SCHEDULED",
	"CreateCryptoKey with a 24h schedule code": "fakekms refuses destroy_scheduled_duration as UNIMPLEMENTED, so every key " +
		"it has uses its fixed 30 days; CloudBurrow accepts 24h to 120d (R:198; council report §4, CreateCryptoKey, and §6, " +
		"\"fakekms destroy 'correct'\": correct only for default-duration keys). The 30-day destroy_time agrees above",
	"destroy on the 24h key code": "follows from the step above: fakekms never created the 24h key, so it answers NOT_FOUND",
}
