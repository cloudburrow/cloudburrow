package main

import "strings"

// The Memorystore pages' values in the create pages' words (#1862), not
// the API's enums: a cluster's or instance's page says "IAM Auth", as
// Create cluster's Security offers it, where the API returns
// AUTH_MODE_IAM_AUTH. Google's docs show no instance page, so the words are
// those of Google's create pages, which they do show
// (https://docs.cloud.google.com/memorystore/docs/cluster/create-instances,
// https://docs.cloud.google.com/memorystore/docs/valkey/create-instances,
// https://docs.cloud.google.com/memorystore/docs/redis/create-manage-instances,
// read 2026-11-20). The enums of the three APIs differ in prefix only
// (AUTH_MODE_IAM_AUTH, IAM_AUTH), so each is read by its suffix.

// memorystoreAuthText is an authorization mode as Security names it.
func memorystoreAuthText(mode string) string {
	switch {
	case strings.HasSuffix(mode, "IAM_AUTH"):
		return "IAM Auth"
	case strings.HasSuffix(mode, "TOKEN_AUTH"):
		return "Token Auth"
	}
	return "Disabled"
}

// memorystoreTLSText is an in-transit encryption mode, as Enable
// In-Transit Encryption (TLS) has it.
func memorystoreTLSText(mode string) string {
	if strings.HasSuffix(mode, "SERVER_AUTHENTICATION") {
		return "Enabled (TLS)"
	}
	return "Disabled"
}

// memorystorePersistenceText is a persistence mode as Data persistence
// strategy names it; snapshot is RDB's name, "Redis Database (RDB)" on a
// cluster and "Memory Snapshot" on Valkey.
func memorystorePersistenceText(mode, snapshot string) string {
	switch {
	case strings.HasSuffix(mode, "AOF"):
		return "Append Only File (AOF)"
	case strings.HasSuffix(mode, "RDB"):
		return snapshot
	}
	return "No persistence"
}

// memorystoreEncryptionText is an encryption type as Encryption names it.
func memorystoreEncryptionText(kind string) string {
	if strings.HasPrefix(kind, "CUSTOMER_MANAGED") {
		return "Cloud KMS key"
	}
	return "Google-managed encryption key"
}

// memorystoreConnectText is a Redis instance's connect mode: Private
// services access, which Advanced network options sets up, or the direct
// peering the API defaults to.
func memorystoreConnectText(mode string) string {
	if mode == "PRIVATE_SERVICE_ACCESS" {
		return "Private services access"
	}
	return "Direct peering"
}

// memorystoreOnOff is a mode as Enabled or Disabled.
func memorystoreOnOff(on bool) string {
	if on {
		return "Enabled"
	}
	return "Disabled"
}
