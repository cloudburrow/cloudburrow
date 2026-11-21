package resourcemanager

import (
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The devices.list and deviceUsers.list filter (#2103): "For a list of
// search fields, refer to Mobile device search fields
// (https://developers.google.com/admin-sdk/directory/v1/search-operators).
// Multiple search fields are separated by the space character"
// (cloudidentity v1 discovery document, read 2026-11-21). That page lists
// every field with the ":" operator, a colon "immediately after the field,
// with no spaces", terms separated by spaces (all of which must match),
// and a serial number searched "with or without the serial field":
//
//   - string fields: apps, bootloader_version, brand, build_number,
//     carrier, compromised_status (compromised, uncompromised, undetected),
//     default_language, email, encryption_status (encrypted, not_encrypted,
//     unsupported), endpoint_verification_cert_fingerprint,
//     endpoint_verification_cert_thumbprint, hardware, hid, id, imei,
//     kernel_version, mac_address, management_type (advanced, basic),
//     manufacturer, meid, model, name, os, owner (company, byod),
//     privilege (device_owner, profile_owner, device_admin), serial,
//     status, type;
//   - date fields register, security_patch_level and sync, as a
//     datetime (2016-04-26T14:23:05; a bare date is that whole day), an
//     inclusive range datetime..datetime, datetime.. or ..datetime.
//
// name is "Given words in usernames": name:joe matches "Joe Sabia" but not
// "Joel Ortiz"; name:Joe Sabia matches "Joe Sabia Daniels" but not "Joe
// The Pun Champ Sabia", so a value's words must appear together, in
// order. The page writes multi-word values unquoted (name:Joe Sabia,
// model:nexus s, os:ios 9); a bare word following a free-text field
// (apps, carrier, default_language, manufacturer, model, name, os)
// continues its value, and a value may also be quoted with " or '. The
// email example matches joe.sabia@example.com and joe.sciarrillo@example.org
// for email:joe@example.com: an address matches when it starts with the
// value or its local part's first word is the value's local part.
// Other string fields match a prefix of the attribute, ignoring case.
//
// Attributes the Devices API's resource has no field for are reported by
// the simulated device at sign-in (SignInInput): its apps, hardware name,
// hardware ID and management type. A device's default_language is its
// users' languageCode, matched by code or English name ("English").

// devTerm is one search term.
type devTerm struct {
	field, value string
	from, to     time.Time // date fields: an inclusive range; zero is open
}

// deviceFilter is a parsed filter: every term must match.
type deviceFilter []devTerm

var devStringFields = map[string]bool{"apps": true, "bootloader_version": true, "brand": true, "build_number": true, "carrier": true,
	"compromised_status": true, "default_language": true, "email": true, "encryption_status": true,
	"endpoint_verification_cert_fingerprint": true, "endpoint_verification_cert_thumbprint": true, "hardware": true, "hid": true,
	"id": true, "imei": true, "kernel_version": true, "mac_address": true, "management_type": true, "manufacturer": true, "meid": true,
	"model": true, "name": true, "os": true, "owner": true, "privilege": true, "serial": true, "status": true, "type": true}

var devDateFields = map[string]bool{"register": true, "security_patch_level": true, "sync": true}

// devMultiWord are the free-text fields whose unquoted value may run on
// over several words.
var devMultiWord = map[string]bool{"apps": true, "carrier": true, "default_language": true, "manufacturer": true, "model": true,
	"name": true, "os": true}

var devEnums = map[string]map[string][]string{
	"compromised_status": {"compromised": {"COMPROMISED"}, "uncompromised": {"UNCOMPROMISED"},
		"undetected": {"", "COMPROMISED_STATE_UNSPECIFIED"}},
	"encryption_status": {"encrypted": {"ENCRYPTED"}, "not_encrypted": {"NOT_ENCRYPTED"},
		"unsupported": {"UNSUPPORTED_BY_DEVICE"}},
	"management_type": {"advanced": {"advanced"}, "basic": {"basic"}},
	"owner":           {"company": {"COMPANY"}, "byod": {"BYOD"}},
	"privilege": {"device_owner": {"DEVICE_OWNER"}, "profile_owner": {"PROFILE_OWNER"},
		"device_admin": {"DEVICE_ADMINISTRATOR"}},
	"status": {"pending": {"PENDING", "PENDING_APPROVAL"}, "approved": {"APPROVED"}, "blocked": {"BLOCKED"},
		"account_wiped": {"WIPED"}, "wiped": {"WIPED"}, "wiping": {"WIPING"}, "unprovisioned": {"UNPROVISIONED"}},
}

// devTokens splits a filter on spaces, keeping quoted runs together.
func devTokens(s string) ([]string, error) {
	var out []string
	var b strings.Builder
	var quote rune
	in := false
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				b.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote, in = c, true
		case unicode.IsSpace(c):
			if in {
				out = append(out, b.String())
				b.Reset()
				in = false
			}
		default:
			b.WriteRune(c)
			in = true
		}
	}
	if quote != 0 {
		return nil, apierror.InvalidArgument("Invalid filter %q: unterminated quote.", s)
	}
	if in {
		out = append(out, b.String())
	}
	return out, nil
}

// parseDevTime reads a search datetime; a bare date covers its whole day.
func parseDevTime(s string, end bool) (time.Time, bool) {
	for _, l := range []string{"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if end {
			return t.Add(24*time.Hour - time.Nanosecond), true
		}
		return t, true
	}
	return time.Time{}, false
}

func parseDeviceFilter(s string) (deviceFilter, error) {
	toks, err := devTokens(s)
	if err != nil {
		return nil, err
	}
	var out deviceFilter
	for _, tok := range toks {
		k, v, ok := strings.Cut(tok, ":")
		if ok {
			k = strings.ToLower(k)
		}
		if !ok || (!devStringFields[k] && !devDateFields[k]) {
			if ok && looksLikeField(k) {
				return nil, apierror.InvalidArgument("Invalid filter %q: unknown search field %s.", s, k)
			}
			// A bare word: it continues a multi-word value, or is a
			// serial number.
			if n := len(out); n > 0 && devMultiWord[out[n-1].field] && out[n-1].value != "" {
				out[n-1].value += " " + tok
				continue
			}
			out = append(out, devTerm{field: "serial", value: tok})
			continue
		}
		if v == "" {
			return nil, apierror.InvalidArgument("Invalid filter %q: %s: has no value; put the value right after the colon.", s, k)
		}
		t := devTerm{field: k, value: v}
		if devDateFields[k] {
			a, b, rng := strings.Cut(v, "..")
			if !rng {
				b = a
			}
			if a != "" {
				if t.from, ok = parseDevTime(a, false); !ok {
					return nil, apierror.InvalidArgument("Invalid filter %q: %s: wants a date or datetime (2016-04-26T14:23:05), or a range of them (a..b).", s, k)
				}
			}
			if b != "" {
				if t.to, ok = parseDevTime(b, true); !ok {
					return nil, apierror.InvalidArgument("Invalid filter %q: %s: wants a date or datetime (2016-04-26T14:23:05), or a range of them (a..b).", s, k)
				}
			}
			if a == "" && b == "" {
				return nil, apierror.InvalidArgument("Invalid filter %q: %s: has an empty range.", s, k)
			}
		}
		if e := devEnums[k]; e != nil {
			if _, ok := e[strings.ToLower(v)]; !ok {
				return nil, apierror.InvalidArgument("Invalid filter %q: unknown value %s for %s:.", s, v, k)
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// looksLikeField is whether a word before a colon reads as a field name
// (letters and underscores), rather than a value holding a colon such as
// a MAC address.
func looksLikeField(k string) bool {
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c == '_') {
			return false
		}
	}
	return true
}

func prefixFold(s, p string) bool { return strings.HasPrefix(strings.ToLower(s), strings.ToLower(p)) }

// words are a string's lower-case words.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) })
}

// wordsMatch is whether q's words appear together, in order, in s's.
func wordsMatch(s, q string) bool {
	sw, qw := words(s), words(q)
	if len(qw) == 0 {
		return false
	}
	for i := 0; i+len(qw) <= len(sw); i++ {
		ok := true
		for j := range qw {
			if sw[i+j] != qw[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// emailMatch is the email: field's match.
func emailMatch(addr, v string) bool {
	if prefixFold(addr, v) {
		return true
	}
	lv, _, _ := strings.Cut(strings.ToLower(v), "@")
	la, _, _ := strings.Cut(strings.ToLower(addr), "@")
	w := strings.FieldsFunc(la, func(c rune) bool { return c == '.' || c == '_' || c == '-' || c == '+' })
	return lv != "" && len(w) > 0 && w[0] == lv
}

// languageMatch matches a language code by code or English name.
func languageMatch(code, v string) bool {
	if code == "" {
		return false
	}
	if prefixFold(code, v) {
		return true
	}
	tag, err := language.Parse(code)
	if err != nil {
		return false
	}
	base, _ := tag.Base()
	return prefixFold(display.English.Tags().Name(tag), v) || prefixFold(display.English.Languages().Name(base), v)
}

func inRange(ts string, t devTerm) bool {
	x, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return false
	}
	return (t.from.IsZero() || !x.Before(t.from)) && (t.to.IsZero() || !x.After(t.to))
}

// managementType is a device's management type: advanced when the device
// reported it, or when an Android work profile or device owner is set up
// (its ownershipPrivilege), else basic.
func (d *DeviceRecord) managementType() string {
	if d.ManagementType != "" {
		return d.ManagementType
	}
	if a := d.Device.AndroidSpecificAttributes; a != nil && a.OwnershipPrivilege != "" && a.OwnershipPrivilege != "OWNERSHIP_PRIVILEGE_UNSPECIFIED" {
		return "advanced"
	}
	return "basic"
}

// matches is whether the device (and, for a device user's listing, the
// user) passes every term. nameOf gives a user's full name by email.
func (f deviceFilter) matches(d *DeviceRecord, only *DeviceUser, nameOf func(email string) string) bool {
	dev := d.Device
	users := d.Users
	if only != nil {
		users = []*DeviceUser{only}
	}
	anyUser := func(ok func(*DeviceUser) bool) bool {
		for _, u := range users {
			if ok(u) {
				return true
			}
		}
		return false
	}
	enum := func(field, v, got string) bool {
		for _, w := range devEnums[field][strings.ToLower(v)] {
			if got == w {
				return true
			}
		}
		return false
	}
	for _, t := range f {
		v := t.value
		ok := false
		switch t.field {
		case "serial":
			ok = prefixFold(dev.SerialNumber, v)
		case "model":
			ok = prefixFold(dev.Model, v)
		case "os":
			ok = prefixFold(dev.OsVersion, v)
		case "type":
			ok = strings.EqualFold(strings.ReplaceAll(dev.DeviceType, "_", ""), strings.ReplaceAll(v, "_", ""))
		case "owner":
			ok = enum("owner", v, dev.OwnerType)
		case "imei":
			ok = prefixFold(dev.Imei, v)
		case "meid":
			ok = prefixFold(dev.Meid, v)
		case "manufacturer":
			ok = prefixFold(dev.Manufacturer, v)
		case "brand":
			ok = prefixFold(dev.Brand, v)
		case "id":
			ok = strings.TrimPrefix(dev.Name, "devices/") == v || dev.DeviceId == v
		case "bootloader_version":
			ok = prefixFold(dev.BootloaderVersion, v)
		case "build_number":
			ok = prefixFold(dev.BuildNumber, v)
		case "kernel_version":
			ok = prefixFold(dev.KernelVersion, v)
		case "carrier":
			ok = prefixFold(dev.NetworkOperator, v)
		case "hardware":
			ok = prefixFold(d.Hardware, v)
		case "hid":
			ok = prefixFold(d.HardwareID, v)
		case "apps":
			for _, a := range d.Apps {
				ok = ok || prefixFold(a, v)
			}
		case "mac_address":
			for _, m := range dev.WifiMacAddresses {
				ok = ok || strings.EqualFold(m, v) || prefixFold(m, v)
			}
		case "compromised_status":
			ok = enum(t.field, v, dev.CompromisedState)
		case "encryption_status":
			ok = enum(t.field, v, dev.EncryptionState)
		case "management_type":
			ok = enum(t.field, v, d.managementType())
		case "privilege":
			if a := dev.AndroidSpecificAttributes; a != nil {
				ok = enum(t.field, v, a.OwnershipPrivilege)
			}
		case "endpoint_verification_cert_fingerprint", "endpoint_verification_cert_thumbprint":
			if a := dev.EndpointVerificationSpecificAttributes; a != nil {
				for _, c := range a.CertificateAttributes {
					got := c.Fingerprint
					if t.field == "endpoint_verification_cert_thumbprint" {
						got = c.Thumbprint
					}
					ok = ok || (got != "" && strings.EqualFold(got, v))
				}
			}
		case "status":
			st := dev.ManagementState
			if only != nil {
				st = only.User.ManagementState
			}
			ok = enum(t.field, v, st)
		case "email":
			ok = anyUser(func(u *DeviceUser) bool { return emailMatch(u.User.UserEmail, v) })
		case "name":
			ok = anyUser(func(u *DeviceUser) bool { return nameOf != nil && wordsMatch(nameOf(u.User.UserEmail), v) })
		case "default_language":
			ok = anyUser(func(u *DeviceUser) bool { return languageMatch(u.User.LanguageCode, v) })
		case "register":
			ok = inRange(dev.CreateTime, t)
		case "security_patch_level":
			ok = inRange(dev.SecurityPatchTime, t)
		case "sync":
			ts := dev.LastSyncTime
			if only != nil {
				ts = only.User.LastSyncTime
			}
			ok = inRange(ts, t)
		}
		if !ok {
			return false
		}
	}
	return true
}

// userNames maps the customer's users' emails to their full names.
func (r *Registry) userNames() func(string) string {
	us, err := r.Users()
	m := map[string]string{}
	if err == nil {
		for _, u := range us {
			nm, _ := u.Object["name"].(map[string]any)
			full, _ := nm["fullName"].(string)
			if full == "" {
				g, _ := nm["givenName"].(string)
				f, _ := nm["familyName"].(string)
				full = strings.TrimSpace(g + " " + f)
			}
			m[u.Email()] = full
		}
	}
	return func(e string) string { return m[normEmail(e)] }
}

// SearchDevices are the devices a devices.list filter finds.
func (r *Registry) SearchDevices(filter string) ([]*DeviceRecord, error) {
	f, err := parseDeviceFilter(filter)
	if err != nil {
		return nil, err
	}
	all, err := r.Devices()
	if err != nil {
		return nil, err
	}
	names := r.userNames()
	var out []*DeviceRecord
	for _, d := range all {
		if f.matches(d, nil, names) {
			out = append(out, d)
		}
	}
	return out, nil
}
