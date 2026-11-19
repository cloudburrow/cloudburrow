package orgpolicy

// Cloud KMS's resource types and documented fields ("Create custom
// organization policy constraints for Cloud KMS", Supported Cloud KMS
// resources, https://docs.cloud.google.com/kms/docs/custom-org-policies,
// read 2026-11-19).
const (
	TypeKMSAutokeyConfig    = "cloudkms.googleapis.com/AutokeyConfig"
	TypeKMSCryptoKey        = "cloudkms.googleapis.com/CryptoKey"
	TypeKMSCryptoKeyVersion = "cloudkms.googleapis.com/CryptoKeyVersion"
	TypeKMSEkmConfig        = "cloudkms.googleapis.com/EkmConfig"
	TypeKMSEkmConnection    = "cloudkms.googleapis.com/EkmConnection"
	TypeKMSImportJob        = "cloudkms.googleapis.com/ImportJob"
	TypeKMSKeyHandle        = "cloudkms.googleapis.com/KeyHandle"
)

func init() {
	RegisterFields(TypeKMSAutokeyConfig, "keyProject", "keyProjectResolutionMode", "name")
	RegisterFields(TypeKMSCryptoKey, "cryptoKeyBackend", "destroyScheduledDuration", "importOnly",
		"keyAccessJustificationsPolicy.allowedAccessReasons", "nextRotationTime", "purpose", "rotationPeriod",
		"versionTemplate.algorithm", "versionTemplate.protectionLevel")
	RegisterTyped(TypeKMSCryptoKey, map[string]string{"destroyScheduledDuration": "duration",
		"nextRotationTime": "timestamp", "rotationPeriod": "duration"})
	RegisterFields(TypeKMSCryptoKeyVersion, "externalProtectionLevelOptions.ekmConnectionKeyPath",
		"externalProtectionLevelOptions.externalKeyUri", "state")
	RegisterFields(TypeKMSEkmConfig, "defaultEkmConnection")
	RegisterFields(TypeKMSEkmConnection, "cryptoSpacePath", "keyManagementMode", "serviceResolvers.endpointFilter",
		"serviceResolvers.hostname", "serviceResolvers.serverCertificates.rawDer", "serviceResolvers.serviceDirectoryService")
	RegisterFields(TypeKMSImportJob, "cryptoKeyBackend", "importMethod", "protectionLevel")
	RegisterFields(TypeKMSKeyHandle, "name", "resourceTypeSelector")
}
