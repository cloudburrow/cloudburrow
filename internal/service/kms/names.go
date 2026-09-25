package kms

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Resource names are parsed before any store lookup (#394): a malformed name
// is INVALID_ARGUMENT naming the field, and only a well-formed name that does
// not exist is NOT_FOUND. Formats: resources.proto keyRings, cryptoKeys and
// cryptoKeyVersions patterns; IDs per service.proto key_ring_id and
// crypto_key_id. Google's code for a malformed name is UNVERIFIED.

var (
	// idRE is a key ring or crypto key ID.
	idRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)
	// versionIDRE is a version number: positive, decimal, no leading zero.
	versionIDRE = regexp.MustCompile(`^[1-9][0-9]*$`)
)

func badName(field, name, kind, why string) error {
	return apierror.InvalidArgument("%s %q is not a valid %s name: %s", field, name, kind, why)
}

// parseLocation checks projects/{p}/locations/{l}, with the project and
// location rules every other service uses. A project number is refused, as
// it is elsewhere; whether Google accepts one here is UNVERIFIED.
func parseLocation(field, name string) error {
	if _, _, err := resource.ParseLocation(name); err != nil {
		return apierror.InvalidArgument("%s %q is not a valid location name: %v", field, name, err)
	}
	return nil
}

// parseKeyRing checks projects/{p}/locations/{l}/keyRings/{kr}.
func parseKeyRing(field, name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[4] != "keyRings" {
		return badName(field, name, "KeyRing", "want projects/{project}/locations/{location}/keyRings/{key_ring}")
	}
	if err := parseLocation(field, strings.Join(parts[:4], "/")); err != nil {
		return err
	}
	if !idRE.MatchString(parts[5]) {
		return badName(field, name, "KeyRing", "the key ring ID must be 1-63 letters, digits, - or _")
	}
	return nil
}

// parseCryptoKey checks .../keyRings/{kr}/cryptoKeys/{ck}.
func parseCryptoKey(field, name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 8 || parts[6] != "cryptoKeys" {
		return badName(field, name, "CryptoKey", "want projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}")
	}
	if err := parseKeyRing(field, strings.Join(parts[:6], "/")); err != nil {
		return err
	}
	if !idRE.MatchString(parts[7]) {
		return badName(field, name, "CryptoKey", "the crypto key ID must be 1-63 letters, digits, - or _")
	}
	return nil
}

// parseCryptoKeyVersion checks .../cryptoKeys/{ck}/cryptoKeyVersions/{n} and
// returns the key's name and the version number.
func parseCryptoKeyVersion(field, name string) (key string, n int, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 10 || parts[8] != "cryptoKeyVersions" {
		return "", 0, badName(field, name, "CryptoKeyVersion",
			"want projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}/cryptoKeyVersions/{version}")
	}
	key = strings.Join(parts[:8], "/")
	if err := parseCryptoKey(field, key); err != nil {
		return "", 0, err
	}
	if n, err = versionNumber(parts[9]); err != nil {
		return "", 0, badName(field, name, "CryptoKeyVersion", err.Error())
	}
	return key, n, nil
}

// versionNumber parses a version ID: a positive decimal with no leading zero.
func versionNumber(id string) (int, error) {
	if !versionIDRE.MatchString(id) {
		return 0, apierror.InvalidArgument("the version ID %q must be a positive number with no leading zero", id)
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return 0, apierror.InvalidArgument("the version ID %q is out of range", id)
	}
	return n, nil
}
