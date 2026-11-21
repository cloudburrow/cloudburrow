package resourcemanager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Cloud Identity's Devices API (#2103): the customer's devices, the users
// signed in to each (deviceUsers) and the client states partners and the
// customer keep on a device user, as an admin manages them
// (https://docs.cloud.google.com/identity/docs/reference/rest/v1/devices,
// .../devices.deviceUsers and .../devices.deviceUsers.clientStates, read
// 2026-11-21):
//
//   - create imports a company-owned device ("Only company-owned device
//     may be created"), UNPROVISIONED ("Device will start from this state
//     until some action is taken (i.e. a user starts using the device)");
//   - a user starts using a device when it signs in on it. No device signs
//     in here, so the instance serves the device's side at
//     /cloudburrow/v1/devices:signIn (a managed user signs in on a device:
//     the company-owned device of that serial number, or a new BYOD
//     device) and /cloudburrow/v1/devices/{device}:sync (the device checks
//     in, carrying out a pending wipe). A new device user is
//     PENDING_APPROVAL, and its device PENDING;
//   - deviceUsers approve ("Approves device to access user data":
//     APPROVED), block ("Blocks device from accessing user data":
//     BLOCKED), wipe (WIPING, then WIPED when the device syncs) and
//     cancelWipe ("in the gap between the wipe operation returning success
//     and the device being wiped": back to the state before the wipe);
//     devices wipe and cancelWipe likewise for the whole device ("possible
//     when the device is in a 'pending wipe' state");
//   - the device's managementState follows its users' while it is not
//     being wiped: APPROVED when one is approved, else PENDING when one
//     awaits approval, else BLOCKED when one is blocked;
//   - list's view (COMPANY_INVENTORY, USER_ASSIGNED_DEVICES), orderBy
//     (createTime, lastSyncTime, model, osVersion, deviceType,
//     serialNumber, optionally desc), page sizes (devices 20 up to 100,
//     device users 5 up to 20, larger coerced), and the "Mobile device
//     search fields" filter terms serial:, model:, os:, type:, status:,
//     email:, owner:, imei:, meid:, manufacturer:, brand:, id: and a bare
//     word (a serial number), separated by spaces;
//   - lookup ("Must be set to 'me'"), for the caller's own device users,
//     matched by the device's deviceId (rawResourceId, androidId or
//     iosDeviceId);
//   - a client state's ID is the customer's ID without its C followed by
//     a suffix ("{customer}-suffix"), keyValuePairs at most 10 KB
//     serialized, its etag checked when sent; patch creates it.
//
// Every mutation answers a done Operation, as Google's do.

const (
	devicePrefix          = "resourcemanager/cloudidentity/devices/"
	deviceType            = "type.googleapis.com/google.apps.cloudidentity.devices.v1."
	maxClientStateKVBytes = 10 * 1024
)

// DeviceRecord is a stored device with its users.
type DeviceRecord struct {
	Device *ci.GoogleAppsCloudidentityDevicesV1Device `json:"device"`
	// BeforeWipe is the device's state before a pending wipe.
	BeforeWipe string        `json:"beforeWipe,omitempty"`
	Users      []*DeviceUser `json:"users,omitempty"`
}

// DeviceUser is a stored device user with its client states.
type DeviceUser struct {
	User         *ci.GoogleAppsCloudidentityDevicesV1DeviceUser    `json:"user"`
	BeforeWipe   string                                            `json:"beforeWipe,omitempty"`
	ClientStates []*ci.GoogleAppsCloudidentityDevicesV1ClientState `json:"clientStates,omitempty"`
}

// ID is the device's ID.
func (d *DeviceRecord) ID() string { return strings.TrimPrefix(d.Device.Name, "devices/") }

func deviceNotFound(name string) error {
	return apierror.NotFound("Device %s not found.", name)
}

// checkCustomer checks a request's optional customer.
func checkCustomer(c string) error {
	switch c {
	case "", "customers/my_customer", CustomerName:
		return nil
	}
	return permissionDenied(c)
}

func (r *Registry) devicesLocked() ([]*DeviceRecord, error) {
	keys, err := r.st.List(devicePrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list devices")
	}
	var out []*DeviceRecord
	for _, k := range keys {
		b, err := r.st.Get(k)
		if err != nil {
			continue
		}
		var d DeviceRecord
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, apierror.Internal(err, "decode device")
		}
		out = append(out, &d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device.Name < out[j].Device.Name })
	return out, nil
}

func (r *Registry) deviceLocked(name string) (*DeviceRecord, error) {
	id, ok := strings.CutPrefix(name, "devices/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return nil, apierror.InvalidArgument("Invalid device name %q: it must be devices/{device}.", name)
	}
	b, err := r.st.Get(devicePrefix + id)
	if err != nil {
		return nil, deviceNotFound(name)
	}
	var d DeviceRecord
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, apierror.Internal(err, "decode device")
	}
	return &d, nil
}

func (r *Registry) putDeviceLocked(d *DeviceRecord) error {
	d.settle()
	b, err := json.Marshal(d)
	if err != nil {
		return apierror.Internal(err, "encode device")
	}
	if err := r.st.Put(devicePrefix+d.ID(), b); err != nil {
		return apierror.Internal(err, "store device")
	}
	return nil
}

// settle sets the device's managementState from its users', unless it is
// being wiped.
func (d *DeviceRecord) settle() {
	switch d.Device.ManagementState {
	case "WIPING", "WIPED":
		return
	}
	has := map[string]bool{}
	for _, u := range d.Users {
		has[u.User.ManagementState] = true
	}
	switch {
	case has["APPROVED"]:
		d.Device.ManagementState = "APPROVED"
	case has["PENDING_APPROVAL"]:
		d.Device.ManagementState = "PENDING"
	case has["BLOCKED"]:
		d.Device.ManagementState = "BLOCKED"
	case len(d.Users) == 0:
		d.Device.ManagementState = "UNPROVISIONED"
	}
}

var deviceTypes = map[string]bool{"DEVICE_TYPE_UNSPECIFIED": true, "ANDROID": true, "IOS": true, "GOOGLE_SYNC": true,
	"WINDOWS": true, "MAC_OS": true, "LINUX": true, "CHROME_OS": true, "GOOGLEBOOK": true}

// CreateDevice imports a company-owned device.
func (r *Registry) CreateDevice(in *ci.GoogleAppsCloudidentityDevicesV1Device) (*DeviceRecord, error) {
	if in == nil || strings.TrimSpace(in.SerialNumber) == "" {
		return nil, apierror.InvalidArgument("Request contains an invalid argument: serialNumber is required to import a company-owned device.")
	}
	if in.OwnerType != "" && in.OwnerType != "COMPANY" {
		return nil, apierror.InvalidArgument("Only company-owned device may be created.")
	}
	if in.DeviceType == "" {
		in.DeviceType = "DEVICE_TYPE_UNSPECIFIED"
	}
	if !deviceTypes[in.DeviceType] {
		return nil, apierror.InvalidArgument("Invalid value at 'device.device_type', %q", in.DeviceType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.devicesLocked()
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Device.OwnerType == "COMPANY" && strings.EqualFold(d.Device.SerialNumber, in.SerialNumber) && d.Device.DeviceType == in.DeviceType {
			return nil, apierror.AlreadyExists("A company-owned device with serial number %s already exists: %s.", in.SerialNumber, d.Device.Name)
		}
	}
	now := ciTime(r.now())
	dev := &ci.GoogleAppsCloudidentityDevicesV1Device{Name: "devices/" + newDeviceID(), SerialNumber: strings.TrimSpace(in.SerialNumber),
		AssetTag: in.AssetTag, DeviceType: in.DeviceType, OwnerType: "COMPANY", CreateTime: now, ManagementState: "UNPROVISIONED",
		WifiMacAddresses: in.WifiMacAddresses, DeviceId: in.DeviceId, Hostname: in.Hostname, LastSyncTime: in.LastSyncTime}
	d := &DeviceRecord{Device: dev}
	return d, r.putDeviceLocked(d)
}

func newDeviceID() string {
	b := newIdentityID()
	sum := sha256.Sum256([]byte(b + time.Now().String()))
	return "D" + hex.EncodeToString(sum[:9])
}

// Devices are every device.
func (r *Registry) Devices() ([]*DeviceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.devicesLocked()
}

// Device is one device.
func (r *Registry) Device(name string) (*DeviceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deviceLocked(name)
}

// DeleteDevice deletes a device and its users.
func (r *Registry) DeleteDevice(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, err := r.deviceLocked(name)
	if err != nil {
		return err
	}
	if err := r.st.Delete(devicePrefix + d.ID()); err != nil {
		return apierror.Internal(err, "delete device")
	}
	return nil
}

// WipeDevice starts a wipe of the whole device; CancelWipeDevice cancels
// a pending one.
func (r *Registry) WipeDevice(name string, cancel bool) (*DeviceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, err := r.deviceLocked(name)
	if err != nil {
		return nil, err
	}
	st := d.Device.ManagementState
	switch {
	case cancel && st != "WIPING":
		if st == "WIPED" {
			return nil, apierror.FailedPrecondition("The wipe command has already been issued to the device %s; it cannot be cancelled.", name)
		}
		return nil, apierror.FailedPrecondition("The device %s is not in a pending wipe state.", name)
	case cancel:
		// Back to the state its users give it (settle).
		d.Device.ManagementState, d.BeforeWipe = "UNPROVISIONED", ""
	case st == "WIPING" || st == "WIPED":
		return nil, apierror.FailedPrecondition("The device %s is already %s.", name, strings.ToLower(st))
	default:
		d.BeforeWipe = st
		d.Device.ManagementState = "WIPING"
	}
	return d, r.putDeviceLocked(d)
}

// deviceUserLocked reads devices/{d}/deviceUsers/{u}.
func (r *Registry) deviceUserLocked(name string) (*DeviceRecord, *DeviceUser, error) {
	dname, uid, ok := strings.Cut(name, "/deviceUsers/")
	if !ok || uid == "" || strings.Contains(uid, "/") {
		return nil, nil, apierror.InvalidArgument("Invalid device user name %q: it must be devices/{device}/deviceUsers/{device_user}.", name)
	}
	if dname == "devices/-" {
		all, err := r.devicesLocked()
		if err != nil {
			return nil, nil, err
		}
		for _, d := range all {
			for _, u := range d.Users {
				if strings.HasSuffix(u.User.Name, "/deviceUsers/"+uid) {
					return d, u, nil
				}
			}
		}
		return nil, nil, apierror.NotFound("Device user %s not found.", name)
	}
	d, err := r.deviceLocked(dname)
	if err != nil {
		return nil, nil, err
	}
	for _, u := range d.Users {
		if u.User.Name == name {
			return d, u, nil
		}
	}
	return nil, nil, apierror.NotFound("Device user %s not found.", name)
}

// DeviceUserByName is a device user and its device.
func (r *Registry) DeviceUserByName(name string) (*DeviceRecord, *DeviceUser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deviceUserLocked(name)
}

// ActOnDeviceUser approves, blocks, wipes or cancels the wipe of a device
// user.
func (r *Registry) ActOnDeviceUser(name, action string) (*DeviceRecord, *DeviceUser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, u, err := r.deviceUserLocked(name)
	if err != nil {
		return nil, nil, err
	}
	st := u.User.ManagementState
	if d.Device.ManagementState == "WIPED" && action != "cancelWipe" {
		return nil, nil, apierror.FailedPrecondition("The device %s has been wiped.", d.Device.Name)
	}
	switch action {
	case "approve", "block":
		if st == "WIPING" || st == "WIPED" {
			return nil, nil, apierror.FailedPrecondition("The device user %s is %s; it cannot be %s.", name, strings.ToLower(st), map[string]string{"approve": "approved", "block": "blocked"}[action])
		}
		u.User.ManagementState = map[string]string{"approve": "APPROVED", "block": "BLOCKED"}[action]
	case "wipe":
		if st == "WIPING" || st == "WIPED" {
			return nil, nil, apierror.FailedPrecondition("The device user %s is already %s.", name, strings.ToLower(st))
		}
		u.BeforeWipe, u.User.ManagementState = st, "WIPING"
	case "cancelWipe":
		if st == "WIPED" {
			return nil, nil, apierror.FailedPrecondition("The device user %s has been wiped; the wipe cannot be cancelled.", name)
		}
		if st != "WIPING" {
			return nil, nil, apierror.FailedPrecondition("The device user %s has no unfinished wipe.", name)
		}
		u.User.ManagementState, u.BeforeWipe = orDefault(u.BeforeWipe, "APPROVED"), ""
	default:
		return nil, nil, apierror.NotFound("no such method: %s", action)
	}
	return d, u, r.putDeviceLocked(d)
}

// DeleteDeviceUser removes a user from its device.
func (r *Registry) DeleteDeviceUser(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, u, err := r.deviceUserLocked(name)
	if err != nil {
		return err
	}
	for i, x := range d.Users {
		if x == u {
			d.Users = append(d.Users[:i], d.Users[i+1:]...)
			break
		}
	}
	return r.putDeviceLocked(d)
}

// SignInInput is a device's sign-in, the device's side of the API.
type SignInInput struct {
	UserEmail    string `json:"userEmail"`
	SerialNumber string `json:"serialNumber"`
	DeviceType   string `json:"deviceType"`
	DeviceID     string `json:"deviceId"`
	Model        string `json:"model"`
	OSVersion    string `json:"osVersion"`
	Manufacturer string `json:"manufacturer"`
	UserAgent    string `json:"userAgent"`
}

// SignIn is a managed user signing in on a device: the company-owned
// device of its serial number and type, or a new BYOD device.
func (r *Registry) SignIn(in SignInInput) (*DeviceRecord, *DeviceUser, error) {
	email := normEmail(in.UserEmail)
	if in.DeviceType == "" {
		in.DeviceType = "ANDROID"
	}
	if !deviceTypes[in.DeviceType] {
		return nil, nil, apierror.InvalidArgument("Invalid deviceType %q.", in.DeviceType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.userLocked(email); err != nil {
		return nil, nil, apierror.InvalidArgument("userEmail %q is not a user of the customer (Directory API users).", in.UserEmail)
	}
	all, err := r.devicesLocked()
	if err != nil {
		return nil, nil, err
	}
	var d *DeviceRecord
	for _, x := range all {
		if in.SerialNumber != "" && strings.EqualFold(x.Device.SerialNumber, in.SerialNumber) && x.Device.DeviceType == in.DeviceType {
			d = x
			break
		}
	}
	now := ciTime(r.now())
	if d == nil {
		d = &DeviceRecord{Device: &ci.GoogleAppsCloudidentityDevicesV1Device{Name: "devices/" + newDeviceID(), OwnerType: "BYOD",
			SerialNumber: in.SerialNumber, DeviceType: in.DeviceType}}
	}
	if d.Device.ManagementState == "WIPED" {
		return nil, nil, apierror.FailedPrecondition("The device %s has been wiped.", d.Device.Name)
	}
	dev := d.Device
	dev.LastSyncTime = now
	for _, kv := range [][2]*string{{&dev.DeviceId, &in.DeviceID}, {&dev.Model, &in.Model}, {&dev.OsVersion, &in.OSVersion}, {&dev.Manufacturer, &in.Manufacturer}} {
		if *kv[1] != "" {
			*kv[0] = *kv[1]
		}
	}
	for _, u := range d.Users {
		if u.User.UserEmail == email {
			u.User.LastSyncTime = now
			return d, u, r.putDeviceLocked(d)
		}
	}
	u := &DeviceUser{User: &ci.GoogleAppsCloudidentityDevicesV1DeviceUser{Name: dev.Name + "/deviceUsers/" + newDeviceID(),
		UserEmail: email, ManagementState: "PENDING_APPROVAL", CreateTime: now, FirstSyncTime: now, LastSyncTime: now,
		UserAgent: in.UserAgent, CompromisedState: "NOT_COMPROMISED", PasswordState: "PASSWORD_SET", LanguageCode: "en-US"}}
	d.Users = append(d.Users, u)
	return d, u, r.putDeviceLocked(d)
}

// SyncDevice is the device checking in: a pending wipe is carried out.
func (r *Registry) SyncDevice(name string) (*DeviceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, err := r.deviceLocked(name)
	if err != nil {
		return nil, err
	}
	now := ciTime(r.now())
	d.Device.LastSyncTime = now
	if d.Device.ManagementState == "WIPING" {
		d.Device.ManagementState, d.BeforeWipe = "WIPED", ""
	}
	for _, u := range d.Users {
		u.User.LastSyncTime = now
		if u.User.ManagementState == "WIPING" || d.Device.ManagementState == "WIPED" {
			u.User.ManagementState, u.BeforeWipe = "WIPED", ""
		}
	}
	return d, r.putDeviceLocked(d)
}

// deviceFilter is a devices.list filter: field:value terms, all of
// which must match.
type deviceFilter [][2]string

var deviceFilterFields = map[string]bool{"serial": true, "model": true, "os": true, "type": true, "status": true, "email": true,
	"owner": true, "imei": true, "meid": true, "manufacturer": true, "brand": true, "id": true}

var documentedDeviceFields = []string{"apps", "bootloader_version", "build_number", "carrier", "compromised_status", "default_language",
	"encryption_status", "endpoint_verification_cert_fingerprint", "endpoint_verification_cert_thumbprint", "hardware", "hid",
	"kernel_version", "mac_address", "management_type", "name", "privilege", "register", "security_patch_level", "sync"}

func parseDeviceFilter(s string) (deviceFilter, error) {
	var out deviceFilter
	for _, term := range strings.Fields(s) {
		k, v, ok := strings.Cut(term, ":")
		if !ok {
			out = append(out, [2]string{"serial", term})
			continue
		}
		k = strings.ToLower(k)
		if !deviceFilterFields[k] {
			for _, f := range documentedDeviceFields {
				if f == k {
					return nil, apierror.Unimplemented("Not implemented here: the %s: search field of devices.list's filter; serial:, model:, os:, type:, status:, email:, owner:, imei:, meid:, manufacturer:, brand: and id: are evaluated.", k)
				}
			}
			return nil, apierror.InvalidArgument("Invalid filter %q: unknown search field %s.", s, k)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

var statusWords = map[string][]string{"pending": {"PENDING", "PENDING_APPROVAL"}, "approved": {"APPROVED"}, "blocked": {"BLOCKED"},
	"account_wiped": {"WIPED"}, "wiped": {"WIPED"}, "wiping": {"WIPING"}, "unprovisioned": {"UNPROVISIONED"}}

func prefixFold(s, p string) bool { return strings.HasPrefix(strings.ToLower(s), strings.ToLower(p)) }

// matches is whether the device (and, for a device user's listing, the
// user) passes every term.
func (f deviceFilter) matches(d *DeviceRecord, only *DeviceUser) bool {
	dev := d.Device
	users := d.Users
	if only != nil {
		users = []*DeviceUser{only}
	}
	for _, t := range f {
		v := t[1]
		ok := false
		switch t[0] {
		case "serial":
			ok = prefixFold(dev.SerialNumber, v)
		case "model":
			ok = prefixFold(dev.Model, v)
		case "os":
			ok = prefixFold(dev.OsVersion, v)
		case "type":
			ok = strings.EqualFold(strings.ReplaceAll(dev.DeviceType, "_", ""), strings.ReplaceAll(v, "_", ""))
		case "owner":
			ok = strings.EqualFold(dev.OwnerType, v) || (strings.EqualFold(v, "company") && dev.OwnerType == "COMPANY")
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
		case "status":
			for _, st := range statusWords[strings.ToLower(v)] {
				if only != nil && only.User.ManagementState == st || only == nil && dev.ManagementState == st {
					ok = true
				}
			}
		case "email":
			for _, u := range users {
				if prefixFold(u.User.UserEmail, v) {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func listSize(req *http.Request, def, max int) (int, error) {
	n, err := rest.QueryInt(req, "pageSize", 0)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, apierror.InvalidArgument("pageSize must not be negative")
	}
	if n == 0 {
		n = def
	}
	if n > max {
		// "values above N will be coerced to N".
		n = max
	}
	return n, nil
}

func deviceOrder(ob string) (func(a, b *ci.GoogleAppsCloudidentityDevicesV1Device) bool, error) {
	fields := strings.Fields(ob)
	if len(fields) == 0 {
		return nil, nil
	}
	if len(fields) > 2 || (len(fields) == 2 && strings.ToLower(fields[1]) != "desc" && strings.ToLower(fields[1]) != "asc") {
		return nil, apierror.InvalidArgument("Invalid orderBy %q.", ob)
	}
	key := map[string]func(*ci.GoogleAppsCloudidentityDevicesV1Device) string{
		"createTime":   func(d *ci.GoogleAppsCloudidentityDevicesV1Device) string { return d.CreateTime },
		"lastSyncTime": func(d *ci.GoogleAppsCloudidentityDevicesV1Device) string { return d.LastSyncTime },
		"model":        func(d *ci.GoogleAppsCloudidentityDevicesV1Device) string { return d.Model },
		"osVersion":    func(d *ci.GoogleAppsCloudidentityDevicesV1Device) string { return d.OsVersion },
		"deviceType":   func(d *ci.GoogleAppsCloudidentityDevicesV1Device) string { return d.DeviceType },
		"serialNumber": func(d *ci.GoogleAppsCloudidentityDevicesV1Device) string { return d.SerialNumber },
	}[fields[0]]
	if key == nil {
		return nil, apierror.InvalidArgument("Invalid orderBy %q: it must be one of createTime, lastSyncTime, model, osVersion, deviceType and serialNumber, optionally followed by desc.", ob)
	}
	desc := len(fields) == 2 && strings.ToLower(fields[1]) == "desc"
	return func(a, b *ci.GoogleAppsCloudidentityDevicesV1Device) bool {
		if desc {
			return key(a) > key(b)
		}
		return key(a) < key(b)
	}, nil
}

func deviceOp(w http.ResponseWriter, typ, field string, v any) error {
	if field == "" {
		return doneOp(w, deviceType+typ, v)
	}
	return doneOp(w, deviceType+typ, map[string]any{field: v})
}

// customerOf reads a request's customer, from its query or body.
func customerOf(req *http.Request) (string, error) {
	c := req.URL.Query().Get("customer")
	if req.Method == http.MethodPost && req.ContentLength != 0 {
		var body map[string]any
		if err := rest.DecodeJSON(req, &body); err != nil {
			return "", err
		}
		if s, _ := body["customer"].(string); s != "" {
			c = s
		}
	}
	return c, checkCustomer(c)
}

func clientStateID(name string) (string, bool) {
	i := strings.LastIndex(name, "/clientStates/")
	if i < 0 {
		return "", false
	}
	return name[i+len("/clientStates/"):], true
}

// PatchClientState creates or updates a client state.
func (r *Registry) PatchClientState(name string, in *ci.GoogleAppsCloudidentityDevicesV1ClientState, mask []string) (*ci.GoogleAppsCloudidentityDevicesV1ClientState, error) {
	uname, cid, ok := strings.Cut(name, "/clientStates/")
	if !ok || cid == "" || strings.Contains(cid, "/") {
		return nil, apierror.InvalidArgument("Invalid client state name %q: it must be devices/{device}/deviceUsers/{device_user}/clientStates/{partner}.", name)
	}
	prefix := strings.TrimPrefix(OrganizationCustomerID, "C") + "-"
	if !strings.HasPrefix(cid, prefix) || len(cid) == len(prefix) {
		return nil, apierror.InvalidArgument("Invalid client state ID %q: it must be the customer's ID without its C, a hyphen and a suffix (%ssuffix); no BeyondCorp Alliance partner is known here.", cid, prefix)
	}
	if in.KeyValuePairs != nil {
		b, _ := json.Marshal(in.KeyValuePairs)
		if len(b) > maxClientStateKVBytes {
			return nil, apierror.InvalidArgument("The total serialized length of keyValuePairs may not exceed 10KB; it is %d bytes.", len(b))
		}
		for k, v := range in.KeyValuePairs {
			n := 0
			if v.BoolValue {
				n++
			}
			if v.NumberValue != 0 {
				n++
			}
			if v.StringValue != "" {
				n++
			}
			if n > 1 {
				return nil, apierror.InvalidArgument("keyValuePairs[%q] sets more than one of boolValue, numberValue and stringValue.", k)
			}
		}
	}
	for _, e := range []struct {
		v, f string
		ok   map[string]bool
	}{
		{in.HealthScore, "health_score", map[string]bool{"": true, "HEALTH_SCORE_UNSPECIFIED": true, "VERY_POOR": true, "POOR": true, "NEUTRAL": true, "GOOD": true, "VERY_GOOD": true}},
		{in.Managed, "managed", map[string]bool{"": true, "MANAGED_STATE_UNSPECIFIED": true, "MANAGED": true, "UNMANAGED": true}},
		{in.ComplianceState, "compliance_state", map[string]bool{"": true, "COMPLIANCE_STATE_UNSPECIFIED": true, "COMPLIANT": true, "NON_COMPLIANT": true}},
	} {
		if !e.ok[e.v] {
			return nil, apierror.InvalidArgument("Invalid value at 'client_state.%s', %q", e.f, e.v)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, u, err := r.deviceUserLocked(uname)
	if err != nil {
		return nil, err
	}
	var cs *ci.GoogleAppsCloudidentityDevicesV1ClientState
	for _, x := range u.ClientStates {
		if x.Name == u.User.Name+"/clientStates/"+cid {
			cs = x
		}
	}
	now := ciTime(r.now())
	if cs == nil {
		cs = &ci.GoogleAppsCloudidentityDevicesV1ClientState{Name: u.User.Name + "/clientStates/" + cid, CreateTime: now, OwnerType: "OWNER_TYPE_CUSTOMER"}
		u.ClientStates = append(u.ClientStates, cs)
	} else if in.Etag != "" && in.Etag != cs.Etag {
		return nil, apierror.Aborted("The client state %s has changed: its etag is not %s.", cs.Name, in.Etag)
	}
	if len(mask) == 0 {
		mask = []string{"custom_id", "asset_tags", "health_score", "score_reason", "managed", "compliance_state", "key_value_pairs"}
	}
	for _, f := range mask {
		switch snake(f) {
		case "custom_id":
			cs.CustomId = in.CustomId
		case "asset_tags":
			cs.AssetTags = in.AssetTags
		case "health_score":
			cs.HealthScore = in.HealthScore
		case "score_reason":
			cs.ScoreReason = in.ScoreReason
		case "managed":
			cs.Managed = in.Managed
		case "compliance_state":
			cs.ComplianceState = in.ComplianceState
		case "key_value_pairs":
			cs.KeyValuePairs = in.KeyValuePairs
		default:
			return nil, apierror.InvalidArgument("Invalid updateMask field %q.", f)
		}
	}
	cs.LastUpdateTime = now
	sum := sha256.Sum256([]byte(cs.Name + now))
	cs.Etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	return cs, r.putDeviceLocked(d)
}

// deviceRoutes registers the Devices API and the device's side.
func (r *Registry) deviceRoutes(rt *rest.Router) {
	rt.Handle("POST /v1/devices", func(w http.ResponseWriter, req *http.Request) error {
		if err := checkCustomer(req.URL.Query().Get("customer")); err != nil {
			return err
		}
		var in ci.GoogleAppsCloudidentityDevicesV1Device
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		d, err := r.CreateDevice(&in)
		if err != nil {
			return err
		}
		return deviceOp(w, "Device", "", d.Device)
	})
	rt.Handle("GET /v1/devices", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		if err := checkCustomer(q.Get("customer")); err != nil {
			return err
		}
		f, err := parseDeviceFilter(q.Get("filter"))
		if err != nil {
			return err
		}
		order, err := deviceOrder(q.Get("orderBy"))
		if err != nil {
			return err
		}
		vw := q.Get("view")
		switch vw {
		case "", "VIEW_UNSPECIFIED", "COMPANY_INVENTORY", "USER_ASSIGNED_DEVICES":
		default:
			return apierror.InvalidArgument("Invalid value at 'view', %q", vw)
		}
		size, err := listSize(req, 20, 100)
		if err != nil {
			return err
		}
		all, err := r.Devices()
		if err != nil {
			return err
		}
		var devs []*ci.GoogleAppsCloudidentityDevicesV1Device
		for _, d := range all {
			if vw == "COMPANY_INVENTORY" && d.Device.OwnerType != "COMPANY" {
				continue
			}
			if vw == "USER_ASSIGNED_DEVICES" && len(d.Users) == 0 {
				continue
			}
			if !f.matches(d, nil) {
				continue
			}
			dev := *d.Device
			if vw == "USER_ASSIGNED_DEVICES" {
				// "no asset tags".
				dev.AssetTag = ""
			}
			devs = append(devs, &dev)
		}
		if order != nil {
			sort.SliceStable(devs, func(i, j int) bool { return order(devs[i], devs[j]) })
		}
		keys := make([]string, len(devs))
		byKey := map[string]*ci.GoogleAppsCloudidentityDevicesV1Device{}
		for i, d := range devs {
			keys[i] = d.Name
			byKey[d.Name] = d
		}
		page, next, err := pageOfSize(req, "devices:"+q.Get("filter")+q.Get("orderBy")+vw, keys, size)
		if err != nil {
			return err
		}
		resp := ci.GoogleAppsCloudidentityDevicesV1ListDevicesResponse{NextPageToken: next, Devices: []*ci.GoogleAppsCloudidentityDevicesV1Device{}}
		for _, k := range page {
			resp.Devices = append(resp.Devices, byKey[k])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/devices/{device}", func(w http.ResponseWriter, req *http.Request) error {
		if err := checkCustomer(req.URL.Query().Get("customer")); err != nil {
			return err
		}
		d, err := r.Device("devices/" + req.PathValue("device"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, d.Device)
	})
	rt.Handle("DELETE /v1/devices/{device}", func(w http.ResponseWriter, req *http.Request) error {
		if err := checkCustomer(req.URL.Query().Get("customer")); err != nil {
			return err
		}
		if err := r.DeleteDevice("devices/" + req.PathValue("device")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})
	rt.Handle("POST /v1/devices/{device}", func(w http.ResponseWriter, req *http.Request) error {
		id, verb, _ := strings.Cut(req.PathValue("device"), ":")
		if verb != "wipe" && verb != "cancelWipe" {
			return apierror.NotFound("no such method: POST %s", req.URL.Path)
		}
		if _, err := customerOf(req); err != nil {
			return err
		}
		d, err := r.WipeDevice("devices/"+id, verb == "cancelWipe")
		if err != nil {
			return err
		}
		return deviceOp(w, map[string]string{"wipe": "WipeDeviceResponse", "cancelWipe": "CancelWipeDeviceResponse"}[verb], "device", d.Device)
	})
	rt.Handle("GET /v1/devices/{device}/{verb}", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		switch req.PathValue("verb") {
		case "deviceUsers":
		case "deviceUsers:lookup":
			return r.lookupDeviceUsers(w, req)
		default:
			return apierror.NotFound("no such method: GET %s", req.URL.Path)
		}
		if err := checkCustomer(q.Get("customer")); err != nil {
			return err
		}
		f, err := parseDeviceFilter(q.Get("filter"))
		if err != nil {
			return err
		}
		size, err := listSize(req, 5, 20)
		if err != nil {
			return err
		}
		var devs []*DeviceRecord
		if dv := req.PathValue("device"); dv == "-" {
			if devs, err = r.Devices(); err != nil {
				return err
			}
		} else {
			d, err := r.Device("devices/" + dv)
			if err != nil {
				return err
			}
			devs = []*DeviceRecord{d}
		}
		byKey := map[string]*ci.GoogleAppsCloudidentityDevicesV1DeviceUser{}
		var keys []string
		for _, d := range devs {
			for _, u := range d.Users {
				if f.matches(d, u) {
					keys = append(keys, u.User.Name)
					byKey[u.User.Name] = u.User
				}
			}
		}
		page, next, err := pageOfSize(req, "deviceUsers:"+req.PathValue("device")+q.Get("filter"), keys, size)
		if err != nil {
			return err
		}
		resp := ci.GoogleAppsCloudidentityDevicesV1ListDeviceUsersResponse{NextPageToken: next, DeviceUsers: []*ci.GoogleAppsCloudidentityDevicesV1DeviceUser{}}
		for _, k := range page {
			resp.DeviceUsers = append(resp.DeviceUsers, byKey[k])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/devices/{device}/deviceUsers/{user}", func(w http.ResponseWriter, req *http.Request) error {
		if err := checkCustomer(req.URL.Query().Get("customer")); err != nil {
			return err
		}
		_, u, err := r.DeviceUserByName("devices/" + req.PathValue("device") + "/deviceUsers/" + req.PathValue("user"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, u.User)
	})
	rt.Handle("DELETE /v1/devices/{device}/deviceUsers/{user}", func(w http.ResponseWriter, req *http.Request) error {
		if err := checkCustomer(req.URL.Query().Get("customer")); err != nil {
			return err
		}
		if err := r.DeleteDeviceUser("devices/" + req.PathValue("device") + "/deviceUsers/" + req.PathValue("user")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})
	rt.Handle("POST /v1/devices/{device}/deviceUsers/{user}", func(w http.ResponseWriter, req *http.Request) error {
		id, verb, _ := strings.Cut(req.PathValue("user"), ":")
		typ := map[string]string{"approve": "ApproveDeviceUserResponse", "block": "BlockDeviceUserResponse",
			"wipe": "WipeDeviceUserResponse", "cancelWipe": "CancelWipeDeviceUserResponse"}[verb]
		if typ == "" {
			return apierror.NotFound("no such method: POST %s", req.URL.Path)
		}
		if _, err := customerOf(req); err != nil {
			return err
		}
		_, u, err := r.ActOnDeviceUser("devices/"+req.PathValue("device")+"/deviceUsers/"+id, verb)
		if err != nil {
			return err
		}
		return deviceOp(w, typ, "deviceUser", u.User)
	})
	rt.Handle("GET /v1/devices/{device}/deviceUsers/{user}/clientStates", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		if err := checkCustomer(q.Get("customer")); err != nil {
			return err
		}
		if q.Get("filter") != "" {
			return apierror.Unimplemented("Not implemented here: clientStates.list's filter, whose syntax Google does not document.")
		}
		dv, us := req.PathValue("device"), req.PathValue("user")
		var states []*ci.GoogleAppsCloudidentityDevicesV1ClientState
		if dv == "-" && us == "-" {
			all, err := r.Devices()
			if err != nil {
				return err
			}
			for _, d := range all {
				for _, u := range d.Users {
					states = append(states, u.ClientStates...)
				}
			}
		} else {
			_, u, err := r.DeviceUserByName("devices/" + dv + "/deviceUsers/" + us)
			if err != nil {
				return err
			}
			states = u.ClientStates
		}
		if states == nil {
			states = []*ci.GoogleAppsCloudidentityDevicesV1ClientState{}
		}
		keys := make([]string, len(states))
		byKey := map[string]*ci.GoogleAppsCloudidentityDevicesV1ClientState{}
		for i, s := range states {
			keys[i] = s.Name
			byKey[s.Name] = s
		}
		page, next, err := pageOfSize(req, "clientStates:"+dv+"/"+us, keys, 100)
		if err != nil {
			return err
		}
		resp := ci.GoogleAppsCloudidentityDevicesV1ListClientStatesResponse{NextPageToken: next, ClientStates: []*ci.GoogleAppsCloudidentityDevicesV1ClientState{}}
		for _, k := range page {
			resp.ClientStates = append(resp.ClientStates, byKey[k])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/devices/{device}/deviceUsers/{user}/clientStates/{state}", func(w http.ResponseWriter, req *http.Request) error {
		if err := checkCustomer(req.URL.Query().Get("customer")); err != nil {
			return err
		}
		_, u, err := r.DeviceUserByName("devices/" + req.PathValue("device") + "/deviceUsers/" + req.PathValue("user"))
		if err != nil {
			return err
		}
		for _, s := range u.ClientStates {
			if id, _ := clientStateID(s.Name); id == req.PathValue("state") {
				return rest.WriteJSON(w, http.StatusOK, s)
			}
		}
		return apierror.NotFound("Client state %s not found.", req.PathValue("state"))
	})
	rt.Handle("PATCH /v1/devices/{device}/deviceUsers/{user}/clientStates/{state}", func(w http.ResponseWriter, req *http.Request) error {
		if err := checkCustomer(req.URL.Query().Get("customer")); err != nil {
			return err
		}
		var in ci.GoogleAppsCloudidentityDevicesV1ClientState
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		cs, err := r.PatchClientState("devices/"+req.PathValue("device")+"/deviceUsers/"+req.PathValue("user")+"/clientStates/"+req.PathValue("state"),
			&in, ciMask(req.URL.Query().Get("updateMask")))
		if err != nil {
			return err
		}
		return deviceOp(w, "ClientState", "", cs)
	})
	// The device's side (no Google API): a sign-in and a sync.
	rt.Handle("POST /cloudburrow/v1/devices:signIn", func(w http.ResponseWriter, req *http.Request) error {
		var in SignInInput
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		_, u, err := r.SignIn(in)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, u.User)
	})
	rt.Handle("POST /cloudburrow/v1/devices/{device}", func(w http.ResponseWriter, req *http.Request) error {
		id, verb, _ := strings.Cut(req.PathValue("device"), ":")
		if verb != "sync" {
			return apierror.NotFound("no such method: POST %s", req.URL.Path)
		}
		d, err := r.SyncDevice("devices/" + id)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, d.Device)
	})
}

func (r *Registry) lookupDeviceUsers(w http.ResponseWriter, req *http.Request) error {
	q := req.URL.Query()
	if req.PathValue("device") != "-" {
		return apierror.InvalidArgument("Invalid parent %q: it must be devices/-/deviceUsers.", "devices/"+req.PathValue("device")+"/deviceUsers")
	}
	if q.Get("userId") != "me" {
		return apierror.InvalidArgument("userId must be set to 'me'.")
	}
	if (q.Get("partner") != "") != (q.Get("iosDeviceId") != "") {
		return apierror.InvalidArgument("If either the partner or iosDeviceId field is provided, then both fields are required.")
	}
	bearer, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	caller, ok := r.Caller(strings.TrimSpace(bearer))
	if !ok {
		return &apierror.Error{Code: codes.PermissionDenied, Message: "The caller does not have permission: lookup needs end-user credentials."}
	}
	size, err := listSize(req, 20, 20)
	if err != nil {
		return err
	}
	all, err := r.Devices()
	if err != nil {
		return err
	}
	var keys []string
	for _, d := range all {
		for _, id := range []string{q.Get("rawResourceId"), q.Get("androidId"), q.Get("iosDeviceId")} {
			if id != "" && d.Device.DeviceId != id {
				goto next
			}
		}
		for _, u := range d.Users {
			if u.User.UserEmail == caller {
				keys = append(keys, u.User.Name)
			}
		}
	next:
	}
	page, nextTok, err := pageOfSize(req, "lookup:"+caller+q.Get("rawResourceId")+q.Get("androidId")+q.Get("iosDeviceId"), keys, size)
	if err != nil {
		return err
	}
	if page == nil {
		page = []string{}
	}
	return rest.WriteJSON(w, http.StatusOK, ci.GoogleAppsCloudidentityDevicesV1LookupSelfDeviceUsersResponse{Customer: CustomerName, Names: page, NextPageToken: nextTok})
}
