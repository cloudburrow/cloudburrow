package compute

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// A VM's metadata server (#2177), as Google documents it
// (https://docs.cloud.google.com/compute/docs/metadata/querying-metadata,
// .../predefined-metadata-keys, .../manage-guest-attributes, read
// 2026-11-22). The guest reaches it at metadata.google.internal,
// 169.254.169.254 and (IPv6-only) fd20:ce::254, where its relay
// (guest/root/usr/lib/cloudburrow/metadata-server) carries every request
// to GuestPrefix + its instance's token; the tree is the instance's own:
//
//   - every key Google lists under instance/ and project/, a directory
//     listed one key a line (a subdirectory with its slash), a leaf as
//     text, ?recursive=true as JSON in Google's camelCase keys, and
//     ?alt=json / ?alt=text over the default;
//   - instance/service-accounts/ the instance's own account (none when it
//     has none), whose token and identity are minted by CloudBurrow's
//     local IAM (Options.Tokens);
//   - instance/guest-attributes/ written (PUT) and deleted from the guest
//     when enable-guest-attributes is TRUE, and read by
//     instances.getGuestAttributes;
//   - an ETag on every answer, and ?wait_for_change=true (with last_etag
//     and timeout_sec) held until the value's ETag changes;
//   - Metadata-Flavor: Google required on every request but the root
//     probe, and every request with X-Forwarded-For refused.

// GuestTokens mints the tokens a VM's metadata server hands out for its
// service account; *metadata.Credentials is one.
type GuestTokens interface {
	MintAccessTokenFor(email string, ttl time.Duration) string
	// MintAccessTokenScoped mints a token limited to scopes (#2179).
	MintAccessTokenScoped(email string, scopes []string, ttl time.Duration) string
	MintIDTokenWithClaims(email, audience string, extra map[string]any, now time.Time, ttl time.Duration) (string, error)
}

// guestTokenTTL is the life of a token the metadata server hands out.
const guestTokenTTL = time.Hour

// mdDir is a metadata directory, its keys in Google's (kebab-case) form;
// raw keeps them as they are in JSON too (attributes, which are the user's).
type mdDir struct {
	kids map[string]any
	raw  bool
}

func newDir() *mdDir { return &mdDir{kids: map[string]any{}} }

func (d *mdDir) set(k string, v any) *mdDir {
	d.kids[k] = v
	return d
}

func (d *mdDir) keys() []string {
	out := make([]string, 0, len(d.kids))
	for k := range d.kids {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mdList is a directory of numbered entries (disks/, network-interfaces/,
// access-configs/), an array in JSON.
type mdList []any

// mdLines is a leaf holding a list: one value a line in text, an array in
// JSON (dns-servers, ipv6s, scopes).
type mdLines []string

// mdJSONList is a leaf whose text is its JSON array too (tags).
type mdJSONList []string

// mdSecret is a leaf listed in its directory but never in a recursive
// answer, and answered on its own (a service account's token, identity).
type mdSecret string

// isDir is whether a node lists as a directory.
func isDir(v any) bool {
	switch v.(type) {
	case *mdDir, mdList:
		return true
	}
	return false
}

// child is a directory's or list's entry k.
func child(v any, k string) (any, bool) {
	switch n := v.(type) {
	case *mdDir:
		c, ok := n.kids[k]
		return c, ok
	case mdList:
		if !isIndex(k) {
			return nil, false
		}
		i, _ := strconv.Atoi(k)
		if i >= len(n) {
			return nil, false
		}
		return n[i], true
	}
	return nil, false
}

// listing is a directory's text: one key a line, a directory's with its
// slash.
func listing(v any) string {
	var b strings.Builder
	switch n := v.(type) {
	case *mdDir:
		for _, k := range n.keys() {
			b.WriteString(k)
			if isDir(n.kids[k]) {
				b.WriteString("/")
			}
			b.WriteString("\n")
		}
	case mdList:
		for i := range n {
			fmt.Fprintf(&b, "%d/\n", i)
		}
	}
	return b.String()
}

// camel is a metadata key in JSON: network-interfaces is networkInterfaces.
func camel(k string) string {
	parts := strings.Split(k, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			r := []rune(parts[i])
			r[0] = unicode.ToUpper(r[0])
			parts[i] = string(r)
		}
	}
	return strings.Join(parts, "")
}

// toJSON is a node as Google's recursive answer writes it.
func toJSON(v any) any {
	switch n := v.(type) {
	case *mdDir:
		out := map[string]any{}
		for k, c := range n.kids {
			if _, secret := c.(mdSecret); secret {
				continue
			}
			key := k
			if !n.raw {
				key = camel(k)
			}
			out[key] = toJSON(c)
		}
		return out
	case mdList:
		out := make([]any, len(n))
		for i, c := range n {
			out[i] = toJSON(c)
		}
		return out
	case mdLines:
		return append([]string{}, n...)
	case mdJSONList:
		return append([]string{}, n...)
	}
	return v
}

// leafText is a leaf as text.
func leafText(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case int64:
		return strconv.FormatInt(n, 10)
	case json.Number:
		return n.String()
	case mdLines:
		if len(n) == 0 {
			return ""
		}
		return strings.Join(n, "\n") + "\n"
	case mdJSONList:
		b, _ := json.Marshal(append([]string{}, n...))
		return string(b)
	}
	return ""
}

// flatText is ?alt=text on a directory with ?recursive=true: one "PATH
// VALUE" line a leaf, as Google flattens it (0/device-name boot).
func flatText(v any, prefix string, b *strings.Builder) {
	switch n := v.(type) {
	case *mdDir:
		for _, k := range n.keys() {
			if _, secret := n.kids[k].(mdSecret); secret {
				continue
			}
			sub := prefix + k
			if isDir(n.kids[k]) {
				sub += "/"
			}
			flatText(n.kids[k], sub, b)
		}
	case mdList:
		for i, c := range n {
			flatText(c, prefix+strconv.Itoa(i)+"/", b)
		}
	case mdLines:
		for _, l := range n {
			b.WriteString(prefix + " " + l + "\n")
		}
	case mdJSONList:
		for _, l := range n {
			b.WriteString(prefix + " " + l + "\n")
		}
	default:
		b.WriteString(prefix + " " + leafText(v) + "\n")
	}
}

func boolKey(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

// guestAttributesOn is whether the instance's (else the project's)
// enable-guest-attributes is TRUE.
func guestAttributesOn(imeta, pmeta map[string]string) bool {
	v, ok := imeta["enable-guest-attributes"]
	if !ok {
		v = pmeta["enable-guest-attributes"]
	}
	return strings.EqualFold(v, "true")
}

// guestTreeLocked is the instance's whole metadata tree: the root holding
// instance/ and project/.
func (s *Server) guestTreeLocked(inst obj, project string) *mdDir {
	zone := lastSeg(str(inst, "zone"))
	number := strconv.FormatInt(resource.ProjectNumber(project), 10)
	pmeta := map[string]string{}
	if po, ok := s.res[projectRel(project)]; ok {
		pmeta = metadataItems(po["commonInstanceMetadata"])
	}
	imeta := metadataItems(inst["metadata"])
	host := str(inst, "hostname")
	if host == "" {
		host = str(inst, "name") + "." + zone + ".c." + project + ".internal"
	}
	in := newDir()
	attrs := &mdDir{kids: map[string]any{}, raw: true}
	for k, v := range imeta {
		attrs.kids[k] = v
	}
	in.set("attributes", attrs)
	id, _ := strconv.ParseInt(str(inst, "id"), 10, 64)
	in.set("id", json.Number(strconv.FormatInt(id, 10)))
	if id == 0 {
		in.set("id", json.Number(str(inst, "id")))
	}
	in.set("name", str(inst, "name"))
	in.set("hostname", host)
	in.set("description", str(inst, "description"))
	in.set("zone", "projects/"+number+"/zones/"+zone)
	in.set("machine-type", "projects/"+number+"/machineTypes/"+lastSeg(str(inst, "machineType")))
	in.set("cpu-platform", str(inst, "cpuPlatform"))
	tags := mdJSONList{}
	if t, ok := inst["tags"].(map[string]any); ok {
		tags = append(tags, strsAny(t["items"])...)
	}
	in.set("tags", tags)
	in.set("maintenance-event", "NONE")
	in.set("preempted", "FALSE")
	in.set("remaining-cpu-time", json.Number("-1"))
	in.set("virtual-clock", newDir().set("drift-token", "0"))
	sched := objOf(inst["scheduling"])
	ohm := str(sched, "onHostMaintenance")
	if ohm == "" {
		ohm = "MIGRATE"
	}
	restart, set := sched["automaticRestart"].(bool)
	if !set {
		restart = true
	}
	pre, _ := sched["preemptible"].(bool)
	in.set("scheduling", newDir().set("on-host-maintenance", ohm).set("automatic-restart", boolKey(restart)).
		set("preemptible", boolKey(pre)))
	// Disks, the boot disk's image and its licenses.
	var disks mdList
	var licenses mdList
	image := ""
	for _, e := range listOf(inst["disks"]) {
		ad := objOf(e)
		mode := str(ad, "mode")
		if mode == "" {
			mode = "READ_WRITE"
		}
		typ := str(ad, "type")
		if typ == "" {
			typ = "PERSISTENT"
		}
		idx, _ := ad["index"].(float64)
		disks = append(disks, newDir().set("device-name", str(ad, "deviceName")).set("index", json.Number(strconv.Itoa(int(idx)))).
			set("interface", str(ad, "interface")).set("mode", mode).set("type", typ))
		if b, _ := ad["boot"].(bool); b {
			if d, ok := s.res[str(ad, "source")]; ok {
				image = Rel(str(d, "sourceImage"), project)
				for _, l := range strsAny(d["licenses"]) {
					licenses = append(licenses, newDir().set("id", stableID(Rel(l, project))))
				}
			}
		}
	}
	if disks == nil {
		disks = mdList{}
	}
	if licenses == nil {
		licenses = mdList{}
	}
	in.set("disks", disks)
	in.set("image", image)
	in.set("licenses", licenses)
	// Network interfaces.
	var nics mdList
	for _, e := range listOf(inst["networkInterfaces"]) {
		nics = append(nics, s.guestNICLocked(objOf(e), project))
	}
	if nics == nil {
		nics = mdList{}
	}
	in.set("network-interfaces", nics)
	// The instance's service account, by its email and as default.
	sas := newDir()
	for i, e := range listOf(inst["serviceAccounts"]) {
		sa := objOf(e)
		email := str(sa, "email")
		if email == "" {
			continue
		}
		scopes := mdLines(strsAny(sa["scopes"]))
		if scopes == nil {
			scopes = mdLines{}
		}
		mk := func() *mdDir {
			return newDir().set("aliases", mdLines{"default"}).set("email", email).set("scopes", scopes).
				set("token", mdSecret("token")).set("identity", mdSecret("identity"))
		}
		sas.set(email, mk())
		if i == 0 {
			sas.set("default", mk())
		}
	}
	in.set("service-accounts", sas)
	// Guest attributes, NAMESPACE/KEY.
	ga := &mdDir{kids: map[string]any{}, raw: true}
	for ns, kv := range objOf(inst["_guestAttributes"]) {
		d := &mdDir{kids: map[string]any{}, raw: true}
		for k, v := range objOf(kv) {
			d.kids[k], _ = v.(string)
		}
		ga.kids[ns] = d
	}
	in.set("guest-attributes", ga)

	pr := newDir()
	pattrs := &mdDir{kids: map[string]any{}, raw: true}
	for k, v := range pmeta {
		pattrs.kids[k] = v
	}
	pr.set("attributes", pattrs)
	pr.set("project-id", project)
	pr.set("numeric-project-id", json.Number(number))
	return newDir().set("instance", in).set("project", pr)
}

// guestNICLocked is one network interface's directory.
func (s *Server) guestNICLocked(nic obj, project string) *mdDir {
	acs := listOf(nic["accessConfigs"])
	// An interface without an external IP still shows access config 0 with an
	// empty external-ip, as on Compute Engine: the Ops Agent's resource
	// detector (metadata.ExternalIP) treats a missing key as fatal.
	if len(acs) == 0 {
		acs = []any{obj{}}
	}
	var ac mdList
	for _, a := range acs {
		t := str(objOf(a), "type")
		if t == "" {
			t = "ONE_TO_ONE_NAT"
		}
		ac = append(ac, newDir().set("external-ip", str(objOf(a), "natIP")).set("type", t))
	}
	v6 := str(nic, "ipv6Address")
	for _, a := range listOf(nic["ipv6AccessConfigs"]) {
		if v6 == "" {
			v6 = str(objOf(a), "externalIpv6")
		}
	}
	ip := str(nic, "networkIP")
	d := newDir().set("access-configs", ac).set("ip", ip).set("mac", vmNet{ip: ip, ip6: v6}.mac()).
		set("dns-servers", mdLines{"169.254.169.254"}).set("forwarded-ips", mdList{}).set("ip-aliases", mdList{}).
		set("target-instance-ips", mdList{})
	if parts := strings.Split(Rel(str(nic, "network"), project), "/"); len(parts) >= 5 && parts[0] == "projects" {
		d.set("network", "projects/"+strconv.FormatInt(resource.ProjectNumber(parts[1]), 10)+"/networks/"+parts[len(parts)-1])
		mtu := int64(1460)
		if nw, ok := s.res[strings.Join(parts, "/")]; ok {
			if m, ok := nw["mtu"].(float64); ok && m > 0 {
				mtu = int64(m)
			}
		}
		d.set("mtu", json.Number(strconv.FormatInt(mtu, 10)))
	}
	var aliases mdList
	for _, r := range listOf(nic["aliasIpRanges"]) {
		aliases = append(aliases, str(objOf(r), "ipCidrRange"))
	}
	if aliases != nil {
		d.set("ip-aliases", aliases)
	}
	if sub, ok := s.res[Rel(str(nic, "subnetwork"), project)]; ok {
		if ip != "" {
			if _, n, err := net.ParseCIDR(str(sub, "ipCidrRange")); err == nil {
				d.set("gateway", str(sub, "gatewayAddress"))
				d.set("subnetmask", net.IP(n.Mask).String())
			}
		}
		if v6 != "" {
			if r := str(sub, "ipv6CidrRange"); r != "" {
				d.set("ipv6", mdLines{r})
			} else if r := str(sub, "externalIpv6Prefix"); r != "" {
				d.set("ipv6", mdLines{r})
			}
			d.set("gateway-ipv6", "fe80::1")
		}
	}
	if v6 != "" {
		d.set("ipv6s", mdLines{v6})
		d.set("forwarded-ipv6s", mdList{})
	}
	return d
}

// guestLookup walks path (instance/..., without a leading slash) from root.
func guestLookup(root *mdDir, path string) (any, bool) {
	var node any = root
	p := strings.Trim(path, "/")
	if p == "" {
		return node, true
	}
	for _, seg := range strings.Split(p, "/") {
		c, ok := child(node, seg)
		if !ok {
			return nil, false
		}
		node = c
	}
	return node, true
}

// guestBody is the answer to path: its body, content type and whether it
// was found.
func guestBody(root *mdDir, path string, q map[string][]string) (string, string, bool) {
	node, ok := guestLookup(root, path)
	if !ok {
		return "", "", false
	}
	if _, secret := node.(mdSecret); secret {
		return "", "", false
	}
	recursive := isTrue(first(q, "recursive"))
	alt := first(q, "alt")
	asJSON := false
	switch {
	case alt == "json":
		asJSON = true
	case alt == "text":
	case isDir(node) && recursive:
		asJSON = true
	}
	if isDir(node) && !recursive {
		if asJSON {
			var keys []string
			for _, l := range strings.Split(strings.TrimSuffix(listing(node), "\n"), "\n") {
				if l != "" {
					keys = append(keys, l)
				}
			}
			if keys == nil {
				keys = []string{}
			}
			b, _ := json.Marshal(keys)
			return string(b), "application/json", true
		}
		return listing(node), "application/text", true
	}
	if asJSON {
		b, _ := json.Marshal(toJSON(node))
		return string(b), "application/json", true
	}
	if isDir(node) {
		var b strings.Builder
		flatText(node, "", &b)
		return b.String(), "application/text", true
	}
	return leafText(node), "application/text", true
}

func isTrue(v string) bool {
	switch strings.ToLower(v) {
	case "true", "1", "yes", "y":
		return true
	}
	return false
}

func etagOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

// guestAttrName is a guest attribute's namespace or key: letters,
// numerals, underscores and hyphens.
var guestAttrName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// guestErr writes an error as the metadata server does: a plain text line.
func guestErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n"))
}

// serveInstanceMetadata answers a VM's metadata request; path is what
// follows the token (computeMetadata/v1/..., or "" for the root).
func (s *Server) serveInstanceMetadata(w http.ResponseWriter, r *http.Request, rel, path string) {
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Server", "Metadata Server for VM")
	if r.Header.Get("X-Forwarded-For") != "" {
		guestErr(w, http.StatusForbidden, "Request had X-Forwarded-For header and was rejected.")
		return
	}
	switch path {
	case "", "/":
		// The root probe: clients ask it without the header and check only
		// the answer's Metadata-Flavor (cloud.google.com/go/compute/metadata.OnGCE).
		w.Header().Set("Content-Type", "application/text")
		_, _ = w.Write([]byte("computeMetadata/\n"))
		return
	}
	if r.Header.Get("Metadata-Flavor") != "Google" {
		guestErr(w, http.StatusForbidden, "Missing Metadata-Flavor:Google header.")
		return
	}
	if path == "computeMetadata" || path == "computeMetadata/" {
		w.Header().Set("Content-Type", "application/text")
		_, _ = w.Write([]byte("v1/\n"))
		return
	}
	key, ok := strings.CutPrefix(path, "computeMetadata/v1")
	if !ok || (key != "" && key[0] != '/') {
		guestErr(w, http.StatusNotFound, "Not Found")
		return
	}
	key = strings.TrimPrefix(key, "/")
	q := r.URL.Query()
	if strings.HasPrefix(key, "instance/guest-attributes") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.writeGuestAttribute(w, r, rel, strings.TrimPrefix(key, "instance/guest-attributes"))
			return
		}
		if !s.guestAttributesOnFor(rel) {
			guestErr(w, http.StatusForbidden, "Guest attributes endpoint access is disabled.")
			return
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		guestErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	// The HTTPS metadata server's client credentials (#2179).
	if key == "instance/credentials/mds-client-certificate" {
		s.serveMDSCredentials(w, r, rel)
		return
	}
	// A service account's token and identity.
	if rest, ok := strings.CutPrefix(key, "instance/service-accounts/"); ok {
		if acct, field, _ := strings.Cut(rest, "/"); field == "token" || field == "identity" {
			if isTrue(first(q, "wait_for_change")) {
				guestErr(w, http.StatusBadRequest, "Invalid request: wait_for_change is not supported on service account tokens.")
				return
			}
			s.serveGuestToken(w, r, rel, acct, field)
			return
		}
	}
	read := func() (string, string, bool, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		inst, ok := s.res[rel]
		if !ok {
			return "", "", false, false
		}
		project := strings.Split(rel, "/")[1]
		root := s.guestTreeLocked(inst, project)
		node, found := guestLookup(root, key)
		dirNoSlash := found && isDir(node) && key != "" && !strings.HasSuffix(key, "/")
		if dirNoSlash {
			return "", "", true, true
		}
		body, ct, found := guestBody(root, key, q)
		return body, ct, found, false
	}
	body, ct, found, redirect := read()
	if redirect {
		// A directory named without its slash: Google's metadata server
		// answers 301 to the directory (#2139).
		loc := r.URL.Path + "/"
		if r.URL.RawQuery != "" {
			loc += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, loc, http.StatusMovedPermanently)
		return
	}
	if !found {
		guestErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if isTrue(first(q, "wait_for_change")) {
		// The whole tree may be watched recursively, as Google's guest
		// agent watches it (computeMetadata/v1/?recursive=true).
		if (strings.HasSuffix(key, "/") || key == "") && !isTrue(first(q, "recursive")) {
			guestErr(w, http.StatusBadRequest, "Invalid request: wait_for_change is not supported on directory listings; use recursive=true.")
			return
		}
		last := first(q, "last_etag")
		if last == "" {
			last = etagOf(body)
		}
		timeout := 3600 * time.Second
		if t := first(q, "timeout_sec"); t != "" {
			n, err := strconv.Atoi(t)
			if err != nil || n < 0 {
				guestErr(w, http.StatusBadRequest, "Invalid request: timeout_sec must be a non-negative integer.")
				return
			}
			timeout = time.Duration(n) * time.Second
		}
		deadline := time.NewTimer(timeout)
		defer deadline.Stop()
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
	wait:
		for etagOf(body) == last {
			select {
			case <-r.Context().Done():
				return
			case <-deadline.C:
				break wait
			case <-tick.C:
				b, c, f, _ := read()
				if !f {
					guestErr(w, http.StatusNotFound, "Not Found")
					return
				}
				body, ct = b, c
			}
		}
	}
	w.Header().Set("ETag", etagOf(body))
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write([]byte(body))
}

func (s *Server) guestAttributesOnFor(rel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.res[rel]
	if !ok {
		return false
	}
	project := strings.Split(rel, "/")[1]
	pmeta := map[string]string{}
	if po, ok := s.res[projectRel(project)]; ok {
		pmeta = metadataItems(po["commonInstanceMetadata"])
	}
	return guestAttributesOn(metadataItems(inst["metadata"]), pmeta)
}

// Guest attribute limits (manage-guest-attributes): a value of at most
// 256 KiB, a key of at most 128 bytes, and at most 10 writes a minute per
// instance, in bursts of at most 3 a second (#2179).
const (
	guestAttrMaxValue  = 256 << 10
	guestAttrMaxKey    = 128
	guestAttrPerMinute = 10
	guestAttrPerSecond = 3
)

// guestAttrAllowLocked records a guest attribute write of rel at now and
// reports whether it is within the instance's rate limits.
func (s *Server) guestAttrAllowLocked(rel string, now time.Time) bool {
	if s.gaRate == nil {
		s.gaRate = map[string][]time.Time{}
	}
	var keep []time.Time
	sec := 0
	for _, t := range s.gaRate[rel] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
			if now.Sub(t) < time.Second {
				sec++
			}
		}
	}
	if len(keep) >= guestAttrPerMinute || sec >= guestAttrPerSecond {
		s.gaRate[rel] = keep
		return false
	}
	s.gaRate[rel] = append(keep, now)
	return true
}

// writeGuestAttribute is a PUT or DELETE of instance/guest-attributes/NS/KEY.
func (s *Server) writeGuestAttribute(w http.ResponseWriter, r *http.Request, rel, rest string) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		guestErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !s.guestAttributesOnFor(rel) {
		guestErr(w, http.StatusForbidden, "Guest attributes endpoint access is disabled.")
		return
	}
	ns, key, _ := strings.Cut(strings.Trim(rest, "/"), "/")
	if !guestAttrName.MatchString(ns) || !guestAttrName.MatchString(key) {
		guestErr(w, http.StatusBadRequest, "Bad Request: a guest attribute is NAMESPACE/KEY, each of letters, numerals, underscores and hyphens.")
		return
	}
	if len(key) > guestAttrMaxKey {
		guestErr(w, http.StatusBadRequest, "Bad Request: the key is longer than 128 bytes.")
		return
	}
	var value []byte
	if r.Method == http.MethodPut {
		b := make([]byte, guestAttrMaxValue+1)
		n := 0
		for n < len(b) {
			m, err := r.Body.Read(b[n:])
			n += m
			if err != nil {
				break
			}
		}
		if n > guestAttrMaxValue {
			guestErr(w, http.StatusBadRequest, "Bad Request: the value is larger than 256 KiB.")
			return
		}
		value = b[:n]
	}
	s.mu.Lock()
	inst, ok := s.res[rel]
	if !ok {
		s.mu.Unlock()
		guestErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if !s.guestAttrAllowLocked(rel, s.opts.Now()) {
		s.mu.Unlock()
		guestErr(w, http.StatusTooManyRequests, "Too Many Requests: guest attributes are limited to 10 queries a minute per instance, in bursts of at most 3 a second.")
		return
	}
	ga := objOf(inst["_guestAttributes"])
	if ga == nil {
		ga = obj{}
	}
	kv := objOf(ga[ns])
	if kv == nil {
		kv = obj{}
	}
	if r.Method == http.MethodPut {
		kv[key] = string(value)
		ga[ns] = kv
	} else {
		delete(kv, key)
		if len(kv) == 0 {
			delete(ga, ns)
		} else {
			ga[ns] = kv
		}
	}
	inst["_guestAttributes"] = ga
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		guestErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// serveGuestToken answers instance/service-accounts/ACCOUNT/token and
// .../identity for the instance's own account.
func (s *Server) serveGuestToken(w http.ResponseWriter, r *http.Request, rel, acct, field string) {
	s.mu.Lock()
	inst, ok := s.res[rel]
	var email string
	var scopes []string
	var claims map[string]any
	if ok {
		for i, e := range listOf(inst["serviceAccounts"]) {
			sa := objOf(e)
			if (acct == "default" && i == 0) || acct == str(sa, "email") {
				email, scopes = str(sa, "email"), strsAny(sa["scopes"])
				break
			}
		}
		project := strings.Split(rel, "/")[1]
		created, _ := time.Parse("2006-01-02T15:04:05.000-07:00", str(inst, "creationTimestamp"))
		claims = map[string]any{"project_id": project, "project_number": resource.ProjectNumber(project),
			"zone": lastSeg(str(inst, "zone")), "instance_id": str(inst, "id"), "instance_name": str(inst, "name"),
			"instance_creation_timestamp": created.Unix()}
	}
	s.mu.Unlock()
	if email == "" {
		guestErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if s.opts.Tokens == nil {
		guestErr(w, http.StatusServiceUnavailable, "Service account tokens are not available: CloudBurrow's metadata server is not running.")
		return
	}
	q := r.URL.Query()
	switch field {
	case "token":
		if v := first(q, "enforce_scopes"); v != "" && !isTrue(v) {
			guestErr(w, http.StatusBadRequest, "Invalid request: enforce_scopes must be true when set.")
			return
		}
		if want := first(q, "scopes"); want != "" && isTrue(first(q, "enforce_scopes")) {
			have := map[string]bool{}
			for _, sc := range scopes {
				have[sc] = true
			}
			for _, sc := range strings.Split(want, ",") {
				if sc = strings.TrimSpace(sc); sc != "" && !have[sc] && !have["https://www.googleapis.com/auth/cloud-platform"] {
					guestErr(w, http.StatusBadRequest, "Invalid request: the requested scopes are not a subset of the instance's scopes.")
					return
				}
			}
		}
		// The token carries the instance's access scopes, or those asked
		// for (#2179): with IAM enforcement on, they limit what it may do.
		tokScopes := scopes
		if want := first(q, "scopes"); want != "" {
			tokScopes = nil
			for _, sc := range strings.Split(want, ",") {
				if sc = strings.TrimSpace(sc); sc != "" {
					tokScopes = append(tokScopes, sc)
				}
			}
		}
		tok := s.opts.Tokens.MintAccessTokenScoped(email, tokScopes, guestTokenTTL)
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"access_token": tok, "expires_in": int(guestTokenTTL.Seconds()), "token_type": "Bearer"})
		_, _ = w.Write(b)
	case "identity":
		aud := first(q, "audience")
		if aud == "" {
			guestErr(w, http.StatusBadRequest, "non-empty audience parameter required")
			return
		}
		var extra map[string]any
		if first(q, "format") == "full" {
			ce := claims
			if !isTrue(first(q, "licenses")) {
				ce = map[string]any{}
				for k, v := range claims {
					ce[k] = v
				}
			}
			extra = map[string]any{"google": map[string]any{"compute_engine": ce}}
		}
		tok, err := s.opts.Tokens.MintIDTokenWithClaims(email, aud, extra, s.opts.Now(), guestTokenTTL)
		if err != nil {
			guestErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/text")
		_, _ = w.Write([]byte(tok))
	}
}

// getGuestAttributes is instances.getGuestAttributes: queryPath
// NAMESPACE/ or NAMESPACE/KEY (or "" for every attribute) lists them in
// queryValue, variableKey reads one in variableValue.
func (s *Server) getGuestAttributes(c call, rel string, o obj) (obj, error) {
	ga := objOf(o["_guestAttributes"])
	out := obj{"kind": "compute#guestAttributes", "selfLink": rel + "/guestAttributes"}
	if vk := first(c.query, "variableKey"); vk != "" {
		ns, key, _ := strings.Cut(strings.Trim(vk, "/"), "/")
		v, ok := objOf(ga[ns])[key].(string)
		if !ok {
			return nil, notFound(rel + "/guestAttributes/" + vk)
		}
		out["variableKey"], out["variableValue"] = vk, v
		return out, nil
	}
	qp := first(c.query, "queryPath")
	ns, key, _ := strings.Cut(strings.Trim(qp, "/"), "/")
	var items []any
	nss := make([]string, 0, len(ga))
	for n := range ga {
		nss = append(nss, n)
	}
	sort.Strings(nss)
	for _, n := range nss {
		if ns != "" && n != ns {
			continue
		}
		kv := objOf(ga[n])
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if key != "" && k != key {
				continue
			}
			v, _ := kv[k].(string)
			items = append(items, obj{"namespace": n, "key": k, "value": v})
		}
	}
	if qp != "" && len(items) == 0 {
		return nil, notFound(rel + "/guestAttributes/" + qp)
	}
	out["queryPath"] = qp
	out["queryValue"] = obj{"items": toAny2(items)}
	return out, nil
}

func toAny2(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func isIndex(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && strconv.Itoa(n) == s
}
