// Package awsfake is a local stand-in for the AWS APIs CloudBurrow's AWS
// provider calls (#2209), for tests only: EC2 (the Query protocol, XML in
// the 2016-11-15 namespace), IAM and STS (awsquery XML), the Price List API
// (JSON 1.1), and IAM Identity Center's OIDC and portal APIs (REST JSON).
// Request and response shapes follow each API's reference and the SDK's
// recorded snapshots. It keeps its state in memory, implements only the
// calls the provider makes, and refuses anything else with 400
// InvalidAction, so a call it does not know is a test failure, never a
// plausible answer.
//
// It never contacts AWS. Credentials are not verified, but every call AWS
// signs must carry a SigV4 Authorization header.
package awsfake

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Account is an AWS account IAM Identity Center lists, with its roles.
type Account struct {
	ID    string
	Name  string
	Email string
	Roles []string
}

// Fixed identities the fake answers for.
const (
	DefaultAccount = "111122223333"
	DefaultRole    = "AdministratorAccess"
	// UserARN is the caller of an access key the fake did not issue.
	UserARN = "arn:aws:iam::111122223333:user/cloudburrow-test"
)

type fault struct{ code, message string }

// resource is one EC2 resource. Fields apply by kind.
type resource struct {
	kind    string // vpc, subnet, internet-gateway, route-table, security-group, instance, volume
	id      string
	region  string
	tags    map[string]string
	state   string
	cidr    string
	vpc     string
	subnet  string
	main    bool
	attach  []string // internet gateway: attached VPCs
	assocs  map[string]string
	routes  [][2]string
	ingress []perm
	// instance
	itype, image, userData, publicIP, privateIP, volume string
	groups                                              []string
	shutdown                                            string
	size                                                int
	encrypted                                           bool
	// profile is an instance's IAM instance profile; members are a role's
	// attached policies or a profile's roles.
	profile string
	members []string
}

type perm struct {
	proto, from, to string
	cidrs           []string
}

type device struct {
	client   string
	approved bool
	expires  time.Time
	used     bool
}

type token struct {
	expires time.Time
}

type roleCreds struct {
	account, role string
	expires       time.Time
}

// Server is the fake.
type Server struct {
	srv *httptest.Server
	now func() time.Time

	// Boot, when set, is called as RunInstances makes an instance, with its
	// decoded user data, before the call answers: a test boots a stand-in
	// host as the VM (test/tools/byocfakes boots the Docker-in-Docker sshd
	// stand-in). It is called with the fake's lock held.
	Boot func(userData string)

	mu       sync.Mutex
	seq      int
	calls    []string
	denied   map[string]bool
	failNext map[string]fault
	keep     map[string]bool
	rejected map[string]bool

	regions []string
	res     map[string]*resource
	prices  map[string]map[string]float64

	accounts   []Account
	clients    map[string]string // client ID -> secret
	devices    map[string]*device
	tokens     map[string]token
	refresh    map[string]bool
	creds      map[string]roleCreds
	tokenTTL   time.Duration
	roleTTL    time.Duration
	deviceTTL  time.Duration
	lastDevice string

	ssmTarget   string
	ssmAgent    string
	ssmSessions map[string]*ssmSession
	ssmOffline  map[string]bool
}

// New starts the fake.
func New() *Server {
	s := &Server{
		now:      time.Now,
		denied:   map[string]bool{},
		failNext: map[string]fault{},
		keep:     map[string]bool{},
		rejected: map[string]bool{},
		regions:  []string{"us-east-1", "us-west-2", "eu-west-1", "eu-central-1", "ap-southeast-1"},
		res:      map[string]*resource{},
		prices: map[string]map[string]float64{
			"us-east-1": {"m7i.xlarge": 0.2016, "m7i.4xlarge": 0.8064, "m7i.8xlarge": 1.6128, "g6.xlarge": 0.8048, "gp3": 0.08},
			"us-west-2": {"m7i.xlarge": 0.2016, "m7i.4xlarge": 0.8064, "m7i.8xlarge": 1.6128, "g6.xlarge": 0.8048, "gp3": 0.08},
			"eu-west-1": {"m7i.xlarge": 0.2226, "m7i.4xlarge": 0.8904, "m7i.8xlarge": 1.7808, "gp3": 0.088},
		},
		accounts:  []Account{{ID: DefaultAccount, Name: "Sandbox", Email: "sandbox@example.com", Roles: []string{DefaultRole}}},
		clients:   map[string]string{},
		devices:   map[string]*device{},
		tokens:    map[string]token{},
		refresh:   map[string]bool{},
		creds:     map[string]roleCreds{},
		tokenTTL:  8 * time.Hour,
		roleTTL:   time.Hour,
		deviceTTL: 10 * time.Minute,
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// URL is the base URL of every service.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the fake.
func (s *Server) Close() { s.srv.Close() }

// Deny refuses an IAM action ("ec2:CreateSecurityGroup"): the call answers
// UnauthorizedOperation (AccessDenied for IAM) and SimulatePrincipalPolicy
// reports implicitDeny.
func (s *Server) Deny(action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied[action] = true
}

// FailNext makes the next call of action ("ec2:RunInstances") answer the
// error code with message, as AWS would.
func (s *Server) FailNext(action, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[action] = fault{code, message}
}

// KeepOnDelete makes deletes of a kind of resource ("security-group",
// "vpc", ...) answer success and leave it, so a check after delete fails.
func (s *Server) KeepOnDelete(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keep[kind] = true
}

// RejectKey makes calls signed with an access key ID answer
// InvalidClientTokenId.
func (s *Server) RejectKey(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejected[id] = true
}

// SetAccounts replaces the accounts and roles IAM Identity Center lists.
func (s *Server) SetAccounts(a []Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = a
}

// SetTTLs sets how long access tokens, role credentials and device codes
// last; zero keeps the current value.
func (s *Server) SetTTLs(accessToken, roleCredentials, deviceCode time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if accessToken > 0 {
		s.tokenTTL = accessToken
	}
	if roleCredentials > 0 {
		s.roleTTL = roleCredentials
	}
	if deviceCode > 0 {
		s.deviceTTL = deviceCode
	}
}

// SetPrice sets an on-demand price: an instance type's per hour, or "gp3"
// per GB-month.
func (s *Server) SetPrice(region, item string, usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prices[region] == nil {
		s.prices[region] = map[string]float64{}
	}
	s.prices[region][item] = usd
}

// Approve approves the last device code, as the user does on the AWS page.
func (s *Server) Approve() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.devices[s.lastDevice]; d != nil {
		d.approved = true
	}
}

// ExpireDevice expires the last device code.
func (s *Server) ExpireDevice() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.devices[s.lastDevice]; d != nil {
		d.expires = s.now().Add(-time.Second)
	}
}

// RevokeTokens invalidates every access token, as signing out does; the
// refresh tokens still work.
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]token{}
}

// RevokeAll invalidates every access and refresh token.
func (s *Server) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]token{}
	s.refresh = map[string]bool{}
}

// Calls is every call served, as "ec2:RunInstances".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// Count is how many times action was called.
func (s *Server) Count(action string) int {
	n := 0
	for _, c := range s.Calls() {
		if c == action {
			n++
		}
	}
	return n
}

// Resource is one resource as a test reads it.
type Resource struct {
	Kind, ID, Region, State string
	Tags                    map[string]string
	// Ingress is a security group's rules, "tcp 22-22 203.0.113.7/32".
	Ingress []string
	// InstanceType, UserData (decoded) and ShutdownBehavior are an
	// instance's; VolumeSize and Encrypted a volume's.
	InstanceType, UserData, ShutdownBehavior string
	VolumeSize                               int
	Encrypted                                bool
	// Profile is an instance's instance profile; Members a role's policies
	// or a profile's roles.
	Profile string
	Members []string
}

// Resources is every resource that exists, terminated instances included.
func (s *Server) Resources() []Resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Resource
	for _, r := range s.res {
		x := Resource{Kind: r.kind, ID: r.id, Region: r.region, State: r.state, Tags: copyTags(r.tags),
			InstanceType: r.itype, UserData: r.userData, ShutdownBehavior: r.shutdown, VolumeSize: r.size, Encrypted: r.encrypted,
			Profile: r.profile, Members: append([]string(nil), r.members...)}
		for _, p := range r.ingress {
			for _, c := range p.cidrs {
				x.Ingress = append(x.Ingress, p.proto+" "+p.from+"-"+p.to+" "+c)
			}
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Live is every resource that exists, without terminated instances.
func (s *Server) Live() []Resource {
	var out []Resource
	for _, r := range s.Resources() {
		if r.Kind == "instance" && r.State == "terminated" {
			continue
		}
		out = append(out, r)
	}
	return out
}

func copyTags(t map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range t {
		out[k] = v
	}
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/client/register", "/device_authorization", "/token":
		s.oidc(w, r)
		return
	case "/assignment/accounts", "/assignment/roles", "/federation/credentials":
		s.portal(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/data-channel/") {
		s.dataChannel(w, r)
		return
	}
	if t := r.Header.Get("X-Amz-Target"); strings.HasPrefix(t, "AmazonSSM.") {
		s.ssm(w, r, strings.TrimPrefix(t, "AmazonSSM."))
		return
	}
	if t := r.Header.Get("X-Amz-Target"); strings.HasPrefix(t, "AWSPriceListService.") {
		s.pricing(w, r, strings.TrimPrefix(t, "AWSPriceListService."))
		return
	}
	body, _ := io.ReadAll(r.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil || r.Method != http.MethodPost {
		http.Error(w, "unsupported request", http.StatusBadRequest)
		return
	}
	action := form.Get("Action")
	var svc string
	switch form.Get("Version") {
	case "2016-11-15":
		svc = "ec2"
	case "2010-05-08":
		svc = "iam"
	case "2011-06-15":
		svc = "sts"
	default:
		queryError(w, "sts", http.StatusBadRequest, "InvalidAction", "unknown API version "+form.Get("Version"))
		return
	}
	key, ok := s.authorize(w, r, svc)
	if !ok {
		return
	}
	name := svc + ":" + action
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
	if f, ok := s.failNext[name]; ok {
		delete(s.failNext, name)
		status := http.StatusBadRequest
		if f.code == "UnauthorizedOperation" || f.code == "AccessDenied" {
			status = http.StatusForbidden
		}
		queryError(w, svc, status, f.code, f.message)
		return
	}
	if s.denied[name] {
		if svc == "ec2" {
			queryError(w, svc, http.StatusForbidden, "UnauthorizedOperation",
				"You are not authorized to perform this operation. User: "+s.callerARN(key)+" is not authorized to perform: "+name)
		} else {
			queryError(w, svc, http.StatusForbidden, "AccessDenied",
				"User: "+s.callerARN(key)+" is not authorized to perform: "+name)
		}
		return
	}
	switch svc {
	case "ec2":
		s.ec2(w, r, action, form)
	case "iam":
		s.iam(w, action, form)
	case "sts":
		s.sts(w, action, key)
	}
}

// authorize requires a SigV4 Authorization header and returns its access
// key ID; issued role credentials must not have expired.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, svc string) (string, bool) {
	h := r.Header.Get("Authorization")
	i := strings.Index(h, "Credential=")
	if !strings.HasPrefix(h, "AWS4-HMAC-SHA256 ") || i < 0 {
		queryError(w, svc, http.StatusForbidden, "MissingAuthenticationToken", "Request is missing Authentication Token")
		return "", false
	}
	key := strings.SplitN(h[i+len("Credential="):], "/", 2)[0]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejected[key] {
		queryError(w, svc, http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid.")
		return "", false
	}
	if c, ok := s.creds[key]; ok && !s.now().Before(c.expires) {
		queryError(w, svc, http.StatusBadRequest, "ExpiredToken", "The security token included in the request is expired")
		return "", false
	}
	return key, true
}

func (s *Server) callerARN(key string) string {
	if c, ok := s.creds[key]; ok {
		return "arn:aws:sts::" + c.account + ":assumed-role/AWSReservedSSO_" + c.role + "_0123456789abcdef/cloudburrow-test"
	}
	return UserARN
}

func (s *Server) id(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%017x", prefix, 0xc10b0000+s.seq)
}

func esc(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

const ec2NS = "http://ec2.amazonaws.com/doc/2016-11-15/"

func queryError(w http.ResponseWriter, svc string, status int, code, message string) {
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(status)
	if svc == "ec2" {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors><RequestID>fake-request</RequestID></Response>`,
			esc(code), esc(message))
		return
	}
	ns := "https://sts.amazonaws.com/doc/2011-06-15/"
	if svc == "iam" {
		ns = "https://iam.amazonaws.com/doc/2010-05-08/"
	}
	fmt.Fprintf(w, `<ErrorResponse xmlns="%s"><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>fake-request</RequestId></ErrorResponse>`,
		ns, esc(code), esc(message))
}

func ec2Reply(w http.ResponseWriter, action, inner string) {
	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><%sResponse xmlns="%s"><requestId>fake-request</requestId>%s</%sResponse>`,
		action, ec2NS, inner, action)
}

// list reads the members of a flattened list parameter: Prefix.1, Prefix.2.
func list(form url.Values, prefix string) []string {
	var out []string
	for i := 1; ; i++ {
		v, ok := form[prefix+"."+strconv.Itoa(i)]
		if !ok {
			return out
		}
		out = append(out, v[0])
	}
}

// tagSpecs is the tags a create's TagSpecification parameters give kind.
func tagSpecs(form url.Values, kind string) map[string]string {
	tags := map[string]string{}
	for i := 1; ; i++ {
		p := "TagSpecification." + strconv.Itoa(i)
		rt, ok := form[p+".ResourceType"]
		if !ok {
			return tags
		}
		if rt[0] != kind {
			continue
		}
		for j := 1; ; j++ {
			k, ok := form[p+".Tag."+strconv.Itoa(j)+".Key"]
			if !ok {
				break
			}
			tags[k[0]] = form.Get(p + ".Tag." + strconv.Itoa(j) + ".Value")
		}
	}
}

type filter struct {
	name   string
	values []string
}

func filters(form url.Values) []filter {
	var out []filter
	for i := 1; ; i++ {
		p := "Filter." + strconv.Itoa(i)
		n, ok := form[p+".Name"]
		if !ok {
			return out
		}
		out = append(out, filter{n[0], list(form, p+".Value")})
	}
}

func (f filter) match(v string) bool {
	for _, want := range f.values {
		if ok, _ := path.Match(want, v); ok || want == v {
			return true
		}
	}
	return false
}

// matches applies the tag:, vpc-id and instance-state-name filters.
func (r *resource) matches(fs []filter) bool {
	for _, f := range fs {
		switch {
		case strings.HasPrefix(f.name, "tag:"):
			v, ok := r.tags[strings.TrimPrefix(f.name, "tag:")]
			if !ok || !f.match(v) {
				return false
			}
		case f.name == "vpc-id":
			if !f.match(r.vpc) {
				return false
			}
		case f.name == "instance-state-name":
			if !f.match(r.state) {
				return false
			}
		}
	}
	return true
}

func tagSet(t map[string]string) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("<tagSet>")
	for _, k := range keys {
		fmt.Fprintf(&b, "<item><key>%s</key><value>%s</value></item>", esc(k), esc(t[k]))
	}
	b.WriteString("</tagSet>")
	return b.String()
}

func (s *Server) add(r *resource) *resource {
	s.res[r.id] = r
	return r
}

// get is the resource id of kind in region, or answers the not-found error.
func (s *Server) get(w http.ResponseWriter, kind, id, region string) *resource {
	r := s.res[id]
	if r == nil || r.kind != kind || r.region != region {
		codes := map[string]string{"vpc": "InvalidVpcID.NotFound", "subnet": "InvalidSubnetID.NotFound",
			"internet-gateway": "InvalidInternetGatewayID.NotFound", "route-table": "InvalidRouteTableID.NotFound",
			"security-group": "InvalidGroup.NotFound", "instance": "InvalidInstanceID.NotFound", "volume": "InvalidVolume.NotFound"}
		queryError(w, "ec2", http.StatusBadRequest, codes[kind], fmt.Sprintf("The %s ID '%s' does not exist", kind, id))
		return nil
	}
	return r
}

func (s *Server) remove(r *resource) {
	if s.keep[r.kind] {
		return
	}
	delete(s.res, r.id)
}

// regionOf is the region in the request's SigV4 credential scope.
func regionOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	i := strings.Index(h, "Credential=")
	if i < 0 {
		return ""
	}
	parts := strings.Split(strings.SplitN(h[i+len("Credential="):], ",", 2)[0], "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func returnTrue(w http.ResponseWriter, action string) { ec2Reply(w, action, "<return>true</return>") }

func (s *Server) ec2(w http.ResponseWriter, r *http.Request, action string, form url.Values) {
	region := regionOf(r)
	switch action {
	case "DescribeRegions":
		var b strings.Builder
		b.WriteString("<regionInfo>")
		for _, rg := range s.regions {
			fmt.Fprintf(&b, "<item><regionName>%s</regionName><regionEndpoint>ec2.%s.amazonaws.com</regionEndpoint><optInStatus>opt-in-not-required</optInStatus></item>", rg, rg)
		}
		b.WriteString("</regionInfo>")
		ec2Reply(w, action, b.String())

	case "CreateVpc":
		v := s.add(&resource{kind: "vpc", id: s.id("vpc"), region: region, state: "available", cidr: form.Get("CidrBlock"), tags: tagSpecs(form, "vpc")})
		// A VPC comes with its main route table, which deleting it removes.
		s.add(&resource{kind: "route-table", id: s.id("rtb"), region: region, vpc: v.id, main: true, tags: map[string]string{}, assocs: map[string]string{}})
		ec2Reply(w, action, "<vpc>"+s.vpcXML(v)+"</vpc>")
	case "DescribeVpcs":
		ec2Reply(w, action, "<vpcSet>"+s.each("vpc", region, form, s.vpcXML)+"</vpcSet>")
	case "DeleteVpc":
		v := s.get(w, "vpc", form.Get("VpcId"), region)
		if v == nil {
			return
		}
		for _, o := range s.res {
			if o.vpc == v.id && !(o.kind == "route-table" && o.main) && !(o.kind == "instance" && o.state == "terminated") {
				queryError(w, "ec2", http.StatusBadRequest, "DependencyViolation",
					fmt.Sprintf("The vpc '%s' has dependencies and cannot be deleted.", v.id))
				return
			}
			for _, a := range o.attach {
				if a == v.id {
					queryError(w, "ec2", http.StatusBadRequest, "DependencyViolation",
						fmt.Sprintf("The vpc '%s' has dependencies and cannot be deleted.", v.id))
					return
				}
			}
		}
		if !s.keep["vpc"] {
			for _, o := range s.res {
				if o.vpc == v.id && o.kind == "route-table" && o.main {
					delete(s.res, o.id)
				}
			}
		}
		s.remove(v)
		returnTrue(w, action)

	case "CreateSubnet":
		v := s.get(w, "vpc", form.Get("VpcId"), region)
		if v == nil {
			return
		}
		sn := s.add(&resource{kind: "subnet", id: s.id("subnet"), region: region, vpc: v.id, state: "available", cidr: form.Get("CidrBlock"), tags: tagSpecs(form, "subnet")})
		ec2Reply(w, action, "<subnet>"+s.subnetXML(sn)+"</subnet>")
	case "DescribeSubnets":
		ec2Reply(w, action, "<subnetSet>"+s.each("subnet", region, form, s.subnetXML)+"</subnetSet>")
	case "DeleteSubnet":
		sn := s.get(w, "subnet", form.Get("SubnetId"), region)
		if sn == nil {
			return
		}
		for _, o := range s.res {
			if o.kind == "instance" && o.subnet == sn.id && o.state != "terminated" {
				queryError(w, "ec2", http.StatusBadRequest, "DependencyViolation", fmt.Sprintf("The subnet '%s' has dependencies and cannot be deleted.", sn.id))
				return
			}
		}
		for _, o := range s.res {
			if o.kind == "route-table" {
				for a, sub := range o.assocs {
					if sub == sn.id {
						delete(o.assocs, a)
					}
				}
			}
		}
		s.remove(sn)
		returnTrue(w, action)

	case "CreateInternetGateway":
		g := s.add(&resource{kind: "internet-gateway", id: s.id("igw"), region: region, tags: tagSpecs(form, "internet-gateway")})
		ec2Reply(w, action, "<internetGateway>"+s.igwXML(g)+"</internetGateway>")
	case "AttachInternetGateway":
		g := s.get(w, "internet-gateway", form.Get("InternetGatewayId"), region)
		if g == nil || s.get(w, "vpc", form.Get("VpcId"), region) == nil {
			return
		}
		g.attach = append(g.attach, form.Get("VpcId"))
		returnTrue(w, action)
	case "DetachInternetGateway":
		g := s.get(w, "internet-gateway", form.Get("InternetGatewayId"), region)
		if g == nil {
			return
		}
		var keep []string
		found := false
		for _, a := range g.attach {
			if a == form.Get("VpcId") {
				found = true
				continue
			}
			keep = append(keep, a)
		}
		if !found {
			queryError(w, "ec2", http.StatusBadRequest, "Gateway.NotAttached", "resource "+g.id+" is not attached to network "+form.Get("VpcId"))
			return
		}
		g.attach = keep
		returnTrue(w, action)
	case "DeleteInternetGateway":
		g := s.get(w, "internet-gateway", form.Get("InternetGatewayId"), region)
		if g == nil {
			return
		}
		if len(g.attach) > 0 {
			queryError(w, "ec2", http.StatusBadRequest, "DependencyViolation", "The internetGateway '"+g.id+"' has dependencies and cannot be deleted.")
			return
		}
		s.remove(g)
		returnTrue(w, action)
	case "DescribeInternetGateways":
		ec2Reply(w, action, "<internetGatewaySet>"+s.each("internet-gateway", region, form, s.igwXML)+"</internetGatewaySet>")

	case "CreateRouteTable":
		if s.get(w, "vpc", form.Get("VpcId"), region) == nil {
			return
		}
		t := s.add(&resource{kind: "route-table", id: s.id("rtb"), region: region, vpc: form.Get("VpcId"), tags: tagSpecs(form, "route-table"), assocs: map[string]string{}})
		ec2Reply(w, action, "<routeTable>"+s.rtbXML(t)+"</routeTable>")
	case "CreateRoute":
		t := s.get(w, "route-table", form.Get("RouteTableId"), region)
		if t == nil || s.get(w, "internet-gateway", form.Get("GatewayId"), region) == nil {
			return
		}
		t.routes = append(t.routes, [2]string{form.Get("DestinationCidrBlock"), form.Get("GatewayId")})
		returnTrue(w, action)
	case "AssociateRouteTable":
		t := s.get(w, "route-table", form.Get("RouteTableId"), region)
		if t == nil || s.get(w, "subnet", form.Get("SubnetId"), region) == nil {
			return
		}
		a := s.id("rtbassoc")
		t.assocs[a] = form.Get("SubnetId")
		ec2Reply(w, action, "<associationId>"+a+"</associationId><associationState><state>associated</state></associationState>")
	case "DisassociateRouteTable":
		for _, o := range s.res {
			if o.kind == "route-table" {
				if _, ok := o.assocs[form.Get("AssociationId")]; ok {
					delete(o.assocs, form.Get("AssociationId"))
					returnTrue(w, action)
					return
				}
			}
		}
		queryError(w, "ec2", http.StatusBadRequest, "InvalidAssociationID.NotFound", "The association ID '"+form.Get("AssociationId")+"' does not exist")
	case "DeleteRouteTable":
		t := s.get(w, "route-table", form.Get("RouteTableId"), region)
		if t == nil {
			return
		}
		if len(t.assocs) > 0 {
			queryError(w, "ec2", http.StatusBadRequest, "DependencyViolation", "The routeTable '"+t.id+"' has dependencies and cannot be deleted.")
			return
		}
		s.remove(t)
		returnTrue(w, action)
	case "DescribeRouteTables":
		ec2Reply(w, action, "<routeTableSet>"+s.each("route-table", region, form, s.rtbXML)+"</routeTableSet>")

	case "CreateSecurityGroup":
		if s.get(w, "vpc", form.Get("VpcId"), region) == nil {
			return
		}
		for _, o := range s.res {
			if o.kind == "security-group" && o.vpc == form.Get("VpcId") && o.cidr == form.Get("GroupName") {
				queryError(w, "ec2", http.StatusBadRequest, "InvalidGroup.Duplicate", "The security group '"+o.cidr+"' already exists for VPC '"+o.vpc+"'")
				return
			}
		}
		// cidr holds the group name.
		g := s.add(&resource{kind: "security-group", id: s.id("sg"), region: region, vpc: form.Get("VpcId"), cidr: form.Get("GroupName"), tags: tagSpecs(form, "security-group")})
		ec2Reply(w, action, "<return>true</return><groupId>"+g.id+"</groupId>"+tagSet(g.tags))
	case "AuthorizeSecurityGroupIngress":
		g := s.get(w, "security-group", form.Get("GroupId"), region)
		if g == nil {
			return
		}
		for i := 1; ; i++ {
			p := "IpPermissions." + strconv.Itoa(i)
			proto, ok := form[p+".IpProtocol"]
			if !ok {
				break
			}
			var cidrs []string
			for j := 1; ; j++ {
				c, ok := form[p+".IpRanges."+strconv.Itoa(j)+".CidrIp"]
				if !ok {
					break
				}
				cidrs = append(cidrs, c[0])
			}
			g.ingress = append(g.ingress, perm{proto[0], form.Get(p + ".FromPort"), form.Get(p + ".ToPort"), cidrs})
		}
		ec2Reply(w, action, "<return>true</return>")
	case "RevokeSecurityGroupIngress":
		g := s.get(w, "security-group", form.Get("GroupId"), region)
		if g == nil {
			return
		}
		if s.revokeIngress(w, g, form) {
			returnTrue(w, action)
		}
	case "DeleteSecurityGroup":
		g := s.get(w, "security-group", form.Get("GroupId"), region)
		if g == nil {
			return
		}
		for _, o := range s.res {
			if o.kind == "instance" && o.state != "terminated" {
				for _, sg := range o.groups {
					if sg == g.id {
						queryError(w, "ec2", http.StatusBadRequest, "DependencyViolation", "resource "+g.id+" has a dependent object")
						return
					}
				}
			}
		}
		s.remove(g)
		returnTrue(w, action)
	case "DescribeSecurityGroups":
		ec2Reply(w, action, "<securityGroupInfo>"+s.each("security-group", region, form, s.sgXML)+"</securityGroupInfo>")

	case "DescribeImages":
		owners := list(form, "Owner")
		var b strings.Builder
		b.WriteString("<imagesSet>")
		if len(owners) == 1 && owners[0] == "099720109477" {
			for _, img := range []struct{ id, name, date string }{
				{"ami-0aa00000000000001", "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20260801", "2026-08-01T00:00:00.000Z"},
				{"ami-0aa00000000000002", "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20261101", "2026-11-01T00:00:00.000Z"},
			} {
				ok := true
				for _, f := range filters(form) {
					if f.name == "name" && !f.match(img.name) {
						ok = false
					}
				}
				if ok {
					fmt.Fprintf(&b, "<item><imageId>%s</imageId><imageOwnerId>099720109477</imageOwnerId><name>%s</name><creationDate>%s</creationDate><imageState>available</imageState><architecture>x86_64</architecture><rootDeviceType>ebs</rootDeviceType><rootDeviceName>/dev/sda1</rootDeviceName></item>",
						img.id, img.name, img.date)
				}
			}
		}
		b.WriteString("</imagesSet>")
		ec2Reply(w, action, b.String())

	case "RunInstances":
		s.runInstances(w, region, form)
	case "DescribeInstances":
		ids := list(form, "InstanceId")
		var items []*resource
		for _, id := range ids {
			i := s.res[id]
			if i == nil || i.kind != "instance" || i.region != region {
				queryError(w, "ec2", http.StatusBadRequest, "InvalidInstanceID.NotFound", "The instance ID '"+id+"' does not exist")
				return
			}
			items = append(items, i)
		}
		if len(ids) == 0 {
			fs := filters(form)
			for _, i := range s.sorted("instance", region) {
				if i.matches(fs) {
					items = append(items, i)
				}
			}
		}
		var b strings.Builder
		b.WriteString("<reservationSet>")
		for _, i := range items {
			// Each read moves an instance on through its transition.
			switch i.state {
			case "pending":
				i.state = "running"
				i.publicIP = fmt.Sprintf("198.51.100.%d", 10+s.seq%200)
			case "stopping":
				i.state = "stopped"
				i.publicIP = ""
			case "shutting-down":
				i.state = "terminated"
				i.publicIP = ""
				if v := s.res[i.volume]; v != nil {
					delete(s.res, v.id)
				}
			}
			fmt.Fprintf(&b, "<item><reservationId>r-%s</reservationId><ownerId>%s</ownerId><instancesSet>%s</instancesSet></item>",
				strings.TrimPrefix(i.id, "i-"), DefaultAccount, s.instanceXML(i))
		}
		b.WriteString("</reservationSet>")
		ec2Reply(w, action, b.String())
	case "StartInstances", "StopInstances", "TerminateInstances":
		var b strings.Builder
		b.WriteString("<instancesSet>")
		for _, id := range list(form, "InstanceId") {
			i := s.get(w, "instance", id, region)
			if i == nil {
				return
			}
			prev := i.state
			switch action {
			case "StartInstances":
				if i.state == "stopped" {
					i.state = "pending"
				}
			case "StopInstances":
				if i.state == "running" || i.state == "pending" {
					i.state = "stopping"
				}
			case "TerminateInstances":
				if s.keep["instance"] {
					break
				}
				if i.state != "terminated" {
					i.state = "shutting-down"
				}
			}
			fmt.Fprintf(&b, "<item><instanceId>%s</instanceId><currentState><code>%d</code><name>%s</name></currentState><previousState><code>%d</code><name>%s</name></previousState></item>",
				i.id, stateCode(i.state), i.state, stateCode(prev), prev)
		}
		b.WriteString("</instancesSet>")
		ec2Reply(w, action, b.String())
	case "ModifyInstanceAttribute":
		i := s.get(w, "instance", form.Get("InstanceId"), region)
		if i == nil {
			return
		}
		if t := form.Get("InstanceType.Value"); t != "" {
			if i.state != "stopped" {
				queryError(w, "ec2", http.StatusBadRequest, "IncorrectInstanceState",
					"The instance '"+i.id+"' is not in the 'stopped' state.")
				return
			}
			i.itype = t
		}
		returnTrue(w, action)
	case "DescribeVolumes":
		ec2Reply(w, action, "<volumeSet>"+s.each("volume", region, form, s.volumeXML)+"</volumeSet>")
	case "DeleteVolume":
		v := s.get(w, "volume", form.Get("VolumeId"), region)
		if v == nil {
			return
		}
		if v.state == "in-use" {
			queryError(w, "ec2", http.StatusBadRequest, "VolumeInUse", "Volume "+v.id+" is currently attached")
			return
		}
		s.remove(v)
		returnTrue(w, action)
	default:
		queryError(w, "ec2", http.StatusBadRequest, "InvalidAction", "The action "+action+" is not valid for this web service (awsfake implements only the calls CloudBurrow makes).")
	}
}

func stateCode(st string) int {
	return map[string]int{"pending": 0, "running": 16, "shutting-down": 32, "terminated": 48, "stopping": 64, "stopped": 80}[st]
}

func (s *Server) runInstances(w http.ResponseWriter, region string, form url.Values) {
	if form.Get("MinCount") != "1" || form.Get("MaxCount") != "1" {
		queryError(w, "ec2", http.StatusBadRequest, "InvalidParameterValue", "awsfake runs one instance at a time")
		return
	}
	subnet := form.Get("NetworkInterface.1.SubnetId")
	sn := s.get(w, "subnet", subnet, region)
	if sn == nil {
		return
	}
	groups := list(form, "NetworkInterface.1.SecurityGroupId")
	for _, g := range groups {
		if s.get(w, "security-group", g, region) == nil {
			return
		}
	}
	if !strings.HasPrefix(form.Get("ImageId"), "ami-") {
		queryError(w, "ec2", http.StatusBadRequest, "InvalidAMIID.Malformed", "Invalid id: \""+form.Get("ImageId")+"\"")
		return
	}
	ud := ""
	if raw := form.Get("UserData"); raw != "" {
		if dec, err := decodeB64(raw); err == nil {
			ud = dec
		} else {
			queryError(w, "ec2", http.StatusBadRequest, "InvalidParameterValue", "Invalid BASE64 encoding of user data.")
			return
		}
	}
	profile := form.Get("IamInstanceProfile.Name")
	if profile != "" && s.res["profile:"+profile] == nil {
		queryError(w, "ec2", http.StatusBadRequest, "InvalidParameterValue", "Value ("+profile+") for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name")
		return
	}
	size, _ := strconv.Atoi(form.Get("BlockDeviceMapping.1.Ebs.VolumeSize"))
	vol := s.add(&resource{kind: "volume", id: s.id("vol"), region: region, state: "in-use", size: size,
		encrypted: form.Get("BlockDeviceMapping.1.Ebs.Encrypted") == "true", tags: tagSpecs(form, "volume")})
	n := s.seq
	inst := s.add(&resource{kind: "instance", id: s.id("i"), region: region, state: "pending", vpc: sn.vpc, subnet: sn.id,
		itype: form.Get("InstanceType"), image: form.Get("ImageId"), userData: ud, groups: groups, volume: vol.id,
		shutdown: form.Get("InstanceInitiatedShutdownBehavior"), profile: profile, privateIP: fmt.Sprintf("10.209.1.%d", 10+n%200),
		tags: tagSpecs(form, "instance")})
	if s.Boot != nil {
		s.Boot(ud)
	}
	ec2Reply(w, "RunInstances", fmt.Sprintf("<reservationId>r-%s</reservationId><ownerId>%s</ownerId><groupSet/><instancesSet>%s</instancesSet>",
		strings.TrimPrefix(inst.id, "i-"), DefaultAccount, s.instanceXML(inst)))
}

func (s *Server) sorted(kind, region string) []*resource {
	var out []*resource
	for _, r := range s.res {
		if r.kind == kind && r.region == region {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// each is the items of kind in region the request's filters match.
func (s *Server) each(kind, region string, form url.Values, render func(*resource) string) string {
	fs := filters(form)
	var b strings.Builder
	for _, r := range s.sorted(kind, region) {
		if r.matches(fs) {
			b.WriteString("<item>" + render(r) + "</item>")
		}
	}
	return b.String()
}

func (s *Server) vpcXML(v *resource) string {
	return fmt.Sprintf("<vpcId>%s</vpcId><ownerId>%s</ownerId><state>%s</state><cidrBlock>%s</cidrBlock><instanceTenancy>default</instanceTenancy><isDefault>false</isDefault>%s",
		v.id, DefaultAccount, v.state, esc(v.cidr), tagSet(v.tags))
}

func (s *Server) subnetXML(sn *resource) string {
	return fmt.Sprintf("<subnetId>%s</subnetId><state>%s</state><vpcId>%s</vpcId><cidrBlock>%s</cidrBlock><availabilityZone>%sa</availabilityZone><availableIpAddressCount>251</availableIpAddressCount><mapPublicIpOnLaunch>false</mapPublicIpOnLaunch>%s",
		sn.id, sn.state, sn.vpc, esc(sn.cidr), sn.region, tagSet(sn.tags))
}

func (s *Server) igwXML(g *resource) string {
	var b strings.Builder
	b.WriteString("<internetGatewayId>" + g.id + "</internetGatewayId><ownerId>" + DefaultAccount + "</ownerId><attachmentSet>")
	for _, a := range g.attach {
		b.WriteString("<item><vpcId>" + a + "</vpcId><state>available</state></item>")
	}
	b.WriteString("</attachmentSet>" + tagSet(g.tags))
	return b.String()
}

func (s *Server) rtbXML(t *resource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<routeTableId>%s</routeTableId><vpcId>%s</vpcId><ownerId>%s</ownerId><routeSet><item><destinationCidrBlock>10.209.0.0/16</destinationCidrBlock><gatewayId>local</gatewayId><state>active</state><origin>CreateRouteTable</origin></item>",
		t.id, t.vpc, DefaultAccount)
	for _, r := range t.routes {
		fmt.Fprintf(&b, "<item><destinationCidrBlock>%s</destinationCidrBlock><gatewayId>%s</gatewayId><state>active</state><origin>CreateRoute</origin></item>", esc(r[0]), r[1])
	}
	b.WriteString("</routeSet><associationSet>")
	if t.main {
		fmt.Fprintf(&b, "<item><routeTableAssociationId>rtbassoc-main%s</routeTableAssociationId><routeTableId>%s</routeTableId><main>true</main><associationState><state>associated</state></associationState></item>",
			strings.TrimPrefix(t.id, "rtb-"), t.id)
	}
	keys := make([]string, 0, len(t.assocs))
	for a := range t.assocs {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	for _, a := range keys {
		fmt.Fprintf(&b, "<item><routeTableAssociationId>%s</routeTableAssociationId><routeTableId>%s</routeTableId><subnetId>%s</subnetId><main>false</main><associationState><state>associated</state></associationState></item>",
			a, t.id, t.assocs[a])
	}
	b.WriteString("</associationSet>" + tagSet(t.tags))
	return b.String()
}

func (s *Server) sgXML(g *resource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<ownerId>%s</ownerId><groupId>%s</groupId><groupName>%s</groupName><groupDescription>CloudBurrow</groupDescription><vpcId>%s</vpcId><ipPermissions>",
		DefaultAccount, g.id, esc(g.cidr), g.vpc)
	for _, p := range g.ingress {
		fmt.Fprintf(&b, "<item><ipProtocol>%s</ipProtocol><fromPort>%s</fromPort><toPort>%s</toPort><groups/><ipRanges>", esc(p.proto), esc(p.from), esc(p.to))
		for _, c := range p.cidrs {
			b.WriteString("<item><cidrIp>" + esc(c) + "</cidrIp></item>")
		}
		b.WriteString("</ipRanges><ipv6Ranges/><prefixListIds/></item>")
	}
	b.WriteString("</ipPermissions><ipPermissionsEgress><item><ipProtocol>-1</ipProtocol><groups/><ipRanges><item><cidrIp>0.0.0.0/0</cidrIp></item></ipRanges><ipv6Ranges/><prefixListIds/></item></ipPermissionsEgress>")
	b.WriteString(tagSet(g.tags))
	return b.String()
}

func (s *Server) volumeXML(v *resource) string {
	return fmt.Sprintf("<volumeId>%s</volumeId><size>%d</size><availabilityZone>%sa</availabilityZone><status>%s</status><volumeType>gp3</volumeType><encrypted>%t</encrypted>%s",
		v.id, v.size, v.region, v.state, v.encrypted, tagSet(v.tags))
}

func (s *Server) instanceXML(i *resource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<item><instanceId>%s</instanceId><imageId>%s</imageId><instanceState><code>%d</code><name>%s</name></instanceState><privateIpAddress>%s</privateIpAddress>",
		i.id, i.image, stateCode(i.state), i.state, i.privateIP)
	if i.publicIP != "" {
		b.WriteString("<ipAddress>" + i.publicIP + "</ipAddress>")
	}
	fmt.Fprintf(&b, "<instanceType>%s</instanceType><placement><availabilityZone>%sa</availabilityZone><tenancy>default</tenancy></placement><subnetId>%s</subnetId><vpcId>%s</vpcId><architecture>x86_64</architecture><rootDeviceType>ebs</rootDeviceType><rootDeviceName>/dev/sda1</rootDeviceName><groupSet>",
		esc(i.itype), i.region, i.subnet, i.vpc)
	for _, g := range i.groups {
		b.WriteString("<item><groupId>" + g + "</groupId></item>")
	}
	b.WriteString("</groupSet><blockDeviceMapping>")
	if i.volume != "" && i.state != "terminated" {
		fmt.Fprintf(&b, "<item><deviceName>/dev/sda1</deviceName><ebs><volumeId>%s</volumeId><status>attached</status><deleteOnTermination>true</deleteOnTermination></ebs></item>", i.volume)
	}
	b.WriteString("</blockDeviceMapping>" + tagSet(i.tags) + "</item>")
	return b.String()
}

// iam serves GetRole and SimulatePrincipalPolicy.
func (s *Server) iam(w http.ResponseWriter, action string, form url.Values) {
	const ns = "https://iam.amazonaws.com/doc/2010-05-08/"
	w.Header().Set("Content-Type", "text/xml")
	if s.iamRoles(w, action, form) {
		return
	}
	switch action {
	case "GetRole":
		name := form.Get("RoleName")
		if !strings.HasPrefix(name, "AWSReservedSSO_") {
			queryError(w, "iam", http.StatusNotFound, "NoSuchEntity", "The role with name "+name+" cannot be found.")
			return
		}
		p := "/aws-reserved/sso.amazonaws.com/"
		fmt.Fprintf(w, `<GetRoleResponse xmlns="%s"><GetRoleResult><Role><Path>%s</Path><RoleName>%s</RoleName><RoleId>AROAFAKE0000000000001</RoleId><Arn>arn:aws:iam::%s:role%s%s</Arn><CreateDate>2026-01-01T00:00:00Z</CreateDate></Role></GetRoleResult><ResponseMetadata><RequestId>fake-request</RequestId></ResponseMetadata></GetRoleResponse>`,
			ns, p, esc(name), DefaultAccount, p, esc(name))
	case "SimulatePrincipalPolicy":
		var b strings.Builder
		for _, a := range list(form, "ActionNames.member") {
			d := "allowed"
			if s.denied[a] {
				d = "implicitDeny"
			}
			fmt.Fprintf(&b, "<member><EvalActionName>%s</EvalActionName><EvalResourceName>*</EvalResourceName><EvalDecision>%s</EvalDecision><MatchedStatements/><MissingContextValues/></member>", esc(a), d)
		}
		fmt.Fprintf(w, `<SimulatePrincipalPolicyResponse xmlns="%s"><SimulatePrincipalPolicyResult><IsTruncated>false</IsTruncated><EvaluationResults>%s</EvaluationResults></SimulatePrincipalPolicyResult><ResponseMetadata><RequestId>fake-request</RequestId></ResponseMetadata></SimulatePrincipalPolicyResponse>`,
			ns, b.String())
	default:
		queryError(w, "iam", http.StatusBadRequest, "InvalidAction", "Could not find operation "+action+" (awsfake implements only the calls CloudBurrow makes).")
	}
}

func (s *Server) sts(w http.ResponseWriter, action, key string) {
	if action != "GetCallerIdentity" {
		queryError(w, "sts", http.StatusBadRequest, "InvalidAction", "Could not find operation "+action+" (awsfake implements only the calls CloudBurrow makes).")
		return
	}
	account := DefaultAccount
	if c, ok := s.creds[key]; ok {
		account = c.account
	}
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>%s</Arn><UserId>AIDAFAKE00000000001</UserId><Account>%s</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>fake-request</RequestId></ResponseMetadata></GetCallerIdentityResponse>`,
		esc(s.callerARN(key)), account)
}

func jsonError(w http.ResponseWriter, status int, typ, message string, extra map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-ErrorType", typ)
	w.WriteHeader(status)
	body := map[string]string{"__type": typ, "message": message}
	for k, v := range extra {
		body[k] = v
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// pricing serves GetProducts, AWS Price List product JSON as strings.
func (s *Server) pricing(w http.ResponseWriter, r *http.Request, op string) {
	if _, ok := s.authorize(w, r, "pricing"); !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "pricing:"+op)
	if f, ok := s.failNext["pricing:"+op]; ok {
		delete(s.failNext, "pricing:"+op)
		jsonError(w, http.StatusBadRequest, f.code, f.message, nil)
		return
	}
	if s.denied["pricing:"+op] {
		jsonError(w, http.StatusBadRequest, "AccessDeniedException", "User is not authorized to perform: pricing:"+op, nil)
		return
	}
	if op != "GetProducts" {
		jsonError(w, http.StatusBadRequest, "InvalidAction", "awsfake implements only GetProducts", nil)
		return
	}
	var in struct {
		ServiceCode string
		Filters     []struct{ Type, Field, Value string }
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ServiceCode != "AmazonEC2" {
		jsonError(w, http.StatusBadRequest, "InvalidParameterException", "ServiceCode AmazonEC2 is required", nil)
		return
	}
	f := map[string]string{}
	for _, x := range in.Filters {
		if x.Type != "TERM_MATCH" {
			jsonError(w, http.StatusBadRequest, "InvalidParameterException", "unsupported filter type "+x.Type, nil)
			return
		}
		f[x.Field] = x.Value
	}
	table := s.prices[f["regionCode"]]
	item, unit, family := f["instanceType"], "Hrs", "Compute Instance"
	if f["volumeApiName"] != "" {
		item, unit, family = f["volumeApiName"], "GB-Mo", "Storage"
	}
	var list []string
	if usd, ok := table[item]; ok && (family == "Storage" || (f["operatingSystem"] == "Linux" && f["tenancy"] == "Shared")) {
		sku := strings.ToUpper(strings.NewReplacer(".", "", "-", "").Replace(f["regionCode"] + item))
		attrs := map[string]string{"regionCode": f["regionCode"], "servicecode": "AmazonEC2"}
		for k, v := range f {
			attrs[k] = v
		}
		product := map[string]any{
			"product":     map[string]any{"productFamily": family, "attributes": attrs, "sku": sku},
			"serviceCode": "AmazonEC2",
			"terms": map[string]any{"OnDemand": map[string]any{sku + ".JRTCKXETXF": map[string]any{
				"priceDimensions": map[string]any{sku + ".JRTCKXETXF.6YS6EN2CT7": map[string]any{
					"unit": unit, "pricePerUnit": map[string]string{"USD": strconv.FormatFloat(usd, 'f', 10, 64)},
					"description": fmt.Sprintf("$%.4f per On Demand %s %s", usd, item, unit), "rateCode": sku + ".JRTCKXETXF.6YS6EN2CT7",
				}},
				"sku": sku, "effectiveDate": "2026-11-01T00:00:00Z", "offerTermCode": "JRTCKXETXF",
			}}},
			"version": "20261101000000", "publicationDate": "2026-11-01T00:00:00Z",
		}
		raw, _ := json.Marshal(product)
		list = append(list, string(raw))
	}
	writeJSON(w, map[string]any{"FormatVersion": "aws_v1", "PriceList": list})
}

// oidc serves IAM Identity Center's OIDC API: RegisterClient,
// StartDeviceAuthorization and CreateToken.
func (s *Server) oidc(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	switch r.URL.Path {
	case "/client/register":
		s.calls = append(s.calls, "sso-oauth:RegisterClient")
		if str("clientType") != "public" || str("clientName") == "" {
			jsonError(w, http.StatusBadRequest, "InvalidClientMetadataException", "clientName and clientType public are required", nil)
			return
		}
		id, sec := s.id("client"), s.id("secret")
		s.clients[id] = sec
		writeJSON(w, map[string]any{"clientId": id, "clientSecret": sec, "clientIdIssuedAt": now.Unix(), "clientSecretExpiresAt": now.Add(90 * 24 * time.Hour).Unix()})
	case "/device_authorization":
		s.calls = append(s.calls, "sso-oauth:StartDeviceAuthorization")
		if s.clients[str("clientId")] == "" || s.clients[str("clientId")] != str("clientSecret") {
			jsonError(w, http.StatusBadRequest, "InvalidClientException", "invalid client", nil)
			return
		}
		if !strings.HasPrefix(str("startUrl"), "https://") {
			jsonError(w, http.StatusBadRequest, "InvalidRequestException", "invalid startUrl", nil)
			return
		}
		code := s.id("device")
		s.devices[code] = &device{client: str("clientId"), expires: now.Add(s.deviceTTL)}
		s.lastDevice = code
		user := fmt.Sprintf("FAKE-%04d", s.seq)
		writeJSON(w, map[string]any{"deviceCode": code, "userCode": user, "verificationUri": s.srv.URL + "/start/#/device",
			"verificationUriComplete": s.srv.URL + "/start/#/device?user_code=" + user, "expiresIn": int(s.deviceTTL / time.Second), "interval": 1})
	case "/token":
		s.calls = append(s.calls, "sso-oauth:CreateToken")
		if s.clients[str("clientId")] == "" || s.clients[str("clientId")] != str("clientSecret") {
			jsonError(w, http.StatusBadRequest, "InvalidClientException", "invalid client", map[string]string{"error": "invalid_client"})
			return
		}
		switch str("grantType") {
		case "urn:ietf:params:oauth:grant-type:device_code":
			d := s.devices[str("deviceCode")]
			switch {
			case d == nil || d.used:
				jsonError(w, http.StatusBadRequest, "InvalidGrantException", "invalid device code", map[string]string{"error": "invalid_grant"})
				return
			case !now.Before(d.expires):
				jsonError(w, http.StatusBadRequest, "ExpiredTokenException", "device code expired", map[string]string{"error": "expired_token"})
				return
			case !d.approved:
				jsonError(w, http.StatusBadRequest, "AuthorizationPendingException", "authorization pending", map[string]string{"error": "authorization_pending"})
				return
			}
			d.used = true
		case "refresh_token":
			if !s.refresh[str("refreshToken")] {
				jsonError(w, http.StatusBadRequest, "InvalidGrantException", "invalid refresh token", map[string]string{"error": "invalid_grant"})
				return
			}
			delete(s.refresh, str("refreshToken"))
		default:
			jsonError(w, http.StatusBadRequest, "UnsupportedGrantTypeException", "unsupported grant type", map[string]string{"error": "unsupported_grant_type"})
			return
		}
		at, rt := s.id("access"), s.id("refresh")
		s.tokens[at] = token{expires: now.Add(s.tokenTTL)}
		s.refresh[rt] = true
		writeJSON(w, map[string]any{"accessToken": at, "tokenType": "Bearer", "expiresIn": int(s.tokenTTL / time.Second), "refreshToken": rt})
	}
}

// portal serves the IAM Identity Center portal: ListAccounts,
// ListAccountRoles and GetRoleCredentials, with the bearer token in
// x-amz-sso_bearer_token.
func (s *Server) portal(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := map[string]string{"/assignment/accounts": "sso:ListAccounts", "/assignment/roles": "sso:ListAccountRoles", "/federation/credentials": "sso:GetRoleCredentials"}
	s.calls = append(s.calls, names[r.URL.Path])
	t, ok := s.tokens[r.Header.Get("x-amz-sso_bearer_token")]
	if !ok || !s.now().Before(t.expires) {
		jsonError(w, http.StatusUnauthorized, "UnauthorizedException", "Session token not found or invalid", nil)
		return
	}
	q := r.URL.Query()
	switch r.URL.Path {
	case "/assignment/accounts":
		var out []map[string]string
		for _, a := range s.accounts {
			out = append(out, map[string]string{"accountId": a.ID, "accountName": a.Name, "emailAddress": a.Email})
		}
		writeJSON(w, map[string]any{"accountList": out})
	case "/assignment/roles":
		var out []map[string]string
		for _, a := range s.accounts {
			if a.ID == q.Get("account_id") {
				for _, role := range a.Roles {
					out = append(out, map[string]string{"accountId": a.ID, "roleName": role})
				}
			}
		}
		writeJSON(w, map[string]any{"roleList": out})
	case "/federation/credentials":
		found := false
		for _, a := range s.accounts {
			if a.ID == q.Get("account_id") {
				for _, role := range a.Roles {
					found = found || role == q.Get("role_name")
				}
			}
		}
		if !found {
			jsonError(w, http.StatusForbidden, "ForbiddenException", "No access", nil)
			return
		}
		key := "ASIA" + strings.ToUpper(strings.TrimPrefix(s.id("key"), "key-"))
		exp := s.now().Add(s.roleTTL)
		s.creds[key] = roleCreds{account: q.Get("account_id"), role: q.Get("role_name"), expires: exp}
		writeJSON(w, map[string]any{"roleCredentials": map[string]any{
			"accessKeyId": key, "secretAccessKey": "fake-secret-" + key, "sessionToken": "fake-session-" + key, "expiration": exp.UnixMilli()}})
	}
}
