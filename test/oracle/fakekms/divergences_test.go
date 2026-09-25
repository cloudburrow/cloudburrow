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
}
