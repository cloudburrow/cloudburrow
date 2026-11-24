package awsfake

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov/ssmtunnel"
)

// The SSM parts of the fake: an instance launched with an instance profile
// whose role has AmazonSSMManagedInstanceCore registers with Systems
// Manager once it runs (DescribeInstanceInformation says Online), and
// StartSession on it with AWS-StartPortForwardingSession opens a data
// channel at the fake's /v1/data-channel/<session>, whose agent side
// (ssmtunnel.ServeAgent) connects to SetSSMTarget's address, as the SSM
// agent on the VM connects to its own port.

// SSMCorePolicy is the managed policy the instance role needs.
const SSMCorePolicy = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"

type ssmSession struct {
	id, target, token, port string
	ended                   bool
}

// SetSSMTarget is the TCP address every port session reaches (the stand-in
// sshd, or an echo server); empty refuses the connection as an agent whose
// port is closed does.
func (s *Server) SetSSMTarget(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ssmTarget = addr
}

// SetSSMAgentVersion is the agent version the handshake names.
func (s *Server) SetSSMAgentVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ssmAgent = v
}

// SSMSessions is the sessions started, and whether each was terminated.
func (s *Server) SSMSessions() (started, terminated int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.ssmSessions {
		started++
		if x.ended {
			terminated++
		}
	}
	return started, terminated
}

// SetOffline makes the instance's agent stop answering (DescribeInstance
// Information says ConnectionLost and StartSession fails), or answer again.
func (s *Server) SetOffline(instanceID string, offline bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ssmOffline == nil {
		s.ssmOffline = map[string]bool{}
	}
	s.ssmOffline[instanceID] = offline
}

// online is whether an instance's agent is registered: it runs, with a
// profile whose role carries the core policy.
func (s *Server) online(i *resource) bool {
	if i == nil || i.kind != "instance" || i.state != "running" || s.ssmOffline[i.id] {
		return false
	}
	p := s.res["profile:"+i.profile]
	if p == nil {
		return false
	}
	for _, role := range p.members {
		if r := s.res["role:"+role]; r != nil {
			for _, pol := range r.members {
				if pol == SSMCorePolicy {
					return true
				}
			}
		}
	}
	return false
}

// ssm serves the JSON API's StartSession, TerminateSession and
// DescribeInstanceInformation.
func (s *Server) ssm(w http.ResponseWriter, r *http.Request, op string) {
	if _, ok := s.authorize(w, r, "ssm"); !ok {
		return
	}
	region := regionOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	name := "ssm:" + op
	s.calls = append(s.calls, name)
	if f, ok := s.failNext[name]; ok {
		delete(s.failNext, name)
		jsonError(w, http.StatusBadRequest, f.code, f.message, nil)
		return
	}
	if s.denied[name] {
		jsonError(w, http.StatusBadRequest, "AccessDeniedException", "User: "+UserARN+" is not authorized to perform: "+name, nil)
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch op {
	case "StartSession":
		target := str("Target")
		i := s.res[target]
		if i == nil || i.kind != "instance" || i.region != region || !s.online(i) {
			jsonError(w, http.StatusBadRequest, "TargetNotConnected", target+" is not connected.", nil)
			return
		}
		if doc := str("DocumentName"); doc != "AWS-StartPortForwardingSession" {
			jsonError(w, http.StatusBadRequest, "InvalidDocument", "awsfake serves only AWS-StartPortForwardingSession, not "+doc, nil)
			return
		}
		port := "22"
		if p, ok := in["Parameters"].(map[string]any); ok {
			if v, ok := p["portNumber"].([]any); ok && len(v) == 1 {
				port, _ = v[0].(string)
			}
		}
		s.seq++
		id := fmt.Sprintf("cloudburrow-%012x", 0x55000000+s.seq)
		tok := fmt.Sprintf("fake-token-%d-%d", s.seq, time.Now().UnixNano())
		if s.ssmSessions == nil {
			s.ssmSessions = map[string]*ssmSession{}
		}
		s.ssmSessions[id] = &ssmSession{id: id, target: target, token: tok, port: port}
		u, _ := url.Parse(s.srv.URL)
		writeJSON(w, map[string]string{"SessionId": id, "TokenValue": tok,
			"StreamUrl": "ws://" + u.Host + "/v1/data-channel/" + id + "?role=publish_subscribe&cell-number=AAEAAQ"})
	case "TerminateSession":
		if x := s.ssmSessions[str("SessionId")]; x != nil {
			x.ended = true
		}
		writeJSON(w, map[string]string{"SessionId": str("SessionId")})
	case "DescribeInstanceInformation":
		var ids []string
		if fs, ok := in["Filters"].([]any); ok {
			for _, f := range fs {
				m, _ := f.(map[string]any)
				if m["Key"] == "InstanceIds" {
					for _, v := range m["Values"].([]any) {
						ids = append(ids, v.(string))
					}
				}
			}
		}
		sort.Strings(ids)
		var list []map[string]any
		for _, id := range ids {
			i := s.res[id]
			if i == nil || i.kind != "instance" || i.region != region || s.res["profile:"+i.profile] == nil {
				continue
			}
			ping := "Online"
			if !s.online(i) {
				ping = "ConnectionLost"
			}
			list = append(list, map[string]any{"InstanceId": id, "PingStatus": ping, "AgentVersion": s.agentVersion(),
				"PlatformType": "Linux", "PlatformName": "Ubuntu", "ResourceType": "EC2Instance"})
		}
		writeJSON(w, map[string]any{"InstanceInformationList": list})
	default:
		jsonError(w, http.StatusBadRequest, "InvalidAction", "awsfake implements only StartSession, TerminateSession and DescribeInstanceInformation", nil)
	}
}

func (s *Server) agentVersion() string {
	if s.ssmAgent != "" {
		return s.ssmAgent
	}
	return ssmtunnel.AgentDefaultVersion
}

var upgrader = websocket.Upgrader{}

// dataChannel is the session's stream URL. Like the real service it needs
// the SigV4 header (service ssmmessages) unless the URL is presigned.
func (s *Server) dataChannel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/data-channel/")
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "AWS4-HMAC-SHA256 ") || !strings.Contains(h, "/ssmmessages/aws4_request") {
		http.Error(w, "missing the ssmmessages SigV4 signature", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	x := s.ssmSessions[id]
	target, version := s.ssmTarget, s.agentVersion()
	s.calls = append(s.calls, "ssmmessages:OpenDataChannel")
	s.mu.Unlock()
	if x == nil || x.ended {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	_ = ssmtunnel.ServeAgent(ws, ssmtunnel.AgentOptions{Token: x.token, Version: version, Port: x.port,
		Dial: func() (net.Conn, error) {
			if target == "" {
				return nil, fmt.Errorf("connection refused")
			}
			return net.DialTimeout("tcp", target, 10*time.Second)
		}})
}

// iamRoles serves the role and instance-profile calls: CreateRole,
// GetRole (for a role it made), AttachRolePolicy, DetachRolePolicy,
// ListAttachedRolePolicies, DeleteRole, CreateInstanceProfile,
// GetInstanceProfile, AddRoleToInstanceProfile,
// RemoveRoleFromInstanceProfile and DeleteInstanceProfile.
func (s *Server) iamRoles(w http.ResponseWriter, action string, form url.Values) bool {
	const ns = "https://iam.amazonaws.com/doc/2010-05-08/"
	reply := func(inner string) {
		fmt.Fprintf(w, `<%sResponse xmlns="%s"><%sResult>%s</%sResult><ResponseMetadata><RequestId>fake-request</RequestId></ResponseMetadata></%sResponse>`,
			action, ns, action, inner, action, action)
	}
	tags := func() map[string]string {
		t := map[string]string{}
		for i := 1; ; i++ {
			k, ok := form["Tags.member."+fmt.Sprint(i)+".Key"]
			if !ok {
				return t
			}
			t[k[0]] = form.Get("Tags.member." + fmt.Sprint(i) + ".Value")
		}
	}
	noSuch := func(kind, name string) {
		queryError(w, "iam", http.StatusNotFound, "NoSuchEntity", "The "+kind+" with name "+name+" cannot be found.")
	}
	tagXML := func(t map[string]string) string {
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("<Tags>")
		for _, k := range keys {
			fmt.Fprintf(&b, "<member><Key>%s</Key><Value>%s</Value></member>", esc(k), esc(t[k]))
		}
		b.WriteString("</Tags>")
		return b.String()
	}
	roleXML := func(r *resource) string {
		return fmt.Sprintf("<Role><Path>/</Path><RoleName>%s</RoleName><RoleId>AROAFAKE%012d</RoleId><Arn>arn:aws:iam::%s:role/%s</Arn><CreateDate>2026-11-24T00:00:00Z</CreateDate>%s</Role>",
			esc(r.cidr), len(r.cidr), DefaultAccount, esc(r.cidr), tagXML(r.tags))
	}
	profileXML := func(p *resource) string {
		var roles strings.Builder
		for _, n := range p.members {
			if r := s.res["role:"+n]; r != nil {
				roles.WriteString("<member>" + strings.TrimPrefix(strings.TrimSuffix(roleXML(r), "</Role>"), "<Role>") + "</member>")
			}
		}
		return fmt.Sprintf("<InstanceProfile><Path>/</Path><InstanceProfileName>%s</InstanceProfileName><InstanceProfileId>AIPAFAKE%012d</InstanceProfileId><Arn>arn:aws:iam::%s:instance-profile/%s</Arn><CreateDate>2026-11-24T00:00:00Z</CreateDate><Roles>%s</Roles>%s</InstanceProfile>",
			esc(p.cidr), len(p.cidr), DefaultAccount, esc(p.cidr), roles.String(), tagXML(p.tags))
	}
	role := form.Get("RoleName")
	profile := form.Get("InstanceProfileName")
	switch action {
	case "CreateRole":
		if s.res["role:"+role] != nil {
			queryError(w, "iam", http.StatusConflict, "EntityAlreadyExists", "Role with name "+role+" already exists.")
			return true
		}
		var doc struct {
			Statement []struct {
				Principal struct{ Service any }
				Action    any
			}
		}
		if err := json.Unmarshal([]byte(form.Get("AssumeRolePolicyDocument")), &doc); err != nil || len(doc.Statement) == 0 ||
			fmt.Sprint(doc.Statement[0].Principal.Service) != "ec2.amazonaws.com" {
			queryError(w, "iam", http.StatusBadRequest, "MalformedPolicyDocument", "the trust policy must let ec2.amazonaws.com assume the role")
			return true
		}
		r := s.add(&resource{kind: "iam-role", id: "role:" + role, cidr: role, tags: tags()})
		reply(roleXML(r))
	case "GetRole":
		r := s.res["role:"+role]
		if r == nil {
			return false // the caller's own role (iam in fake.go)
		}
		reply(roleXML(r))
	case "AttachRolePolicy", "DetachRolePolicy":
		r := s.res["role:"+role]
		if r == nil {
			noSuch("role", role)
			return true
		}
		arn := form.Get("PolicyArn")
		if action == "AttachRolePolicy" {
			if arn != SSMCorePolicy {
				queryError(w, "iam", http.StatusBadRequest, "InvalidInput", "awsfake knows only "+SSMCorePolicy)
				return true
			}
			r.members = append(r.members, arn)
		} else {
			var keep []string
			for _, m := range r.members {
				if m != arn {
					keep = append(keep, m)
				}
			}
			if len(keep) == len(r.members) {
				noSuch("policy", arn)
				return true
			}
			r.members = keep
		}
		reply("")
	case "ListAttachedRolePolicies":
		r := s.res["role:"+role]
		if r == nil {
			noSuch("role", role)
			return true
		}
		var b strings.Builder
		for _, m := range r.members {
			fmt.Fprintf(&b, "<member><PolicyName>%s</PolicyName><PolicyArn>%s</PolicyArn></member>", esc(m[strings.LastIndex(m, "/")+1:]), esc(m))
		}
		reply("<IsTruncated>false</IsTruncated><AttachedPolicies>" + b.String() + "</AttachedPolicies>")
	case "DeleteRole":
		r := s.res["role:"+role]
		if r == nil {
			noSuch("role", role)
			return true
		}
		if len(r.members) > 0 {
			queryError(w, "iam", http.StatusConflict, "DeleteConflict", "Cannot delete entity, must detach all policies first.")
			return true
		}
		for _, o := range s.res {
			if o.kind == "instance-profile" {
				for _, m := range o.members {
					if m == role {
						queryError(w, "iam", http.StatusConflict, "DeleteConflict", "Cannot delete entity, must remove roles from instance profile first.")
						return true
					}
				}
			}
		}
		s.remove(r)
		reply("")
	case "CreateInstanceProfile":
		if s.res["profile:"+profile] != nil {
			queryError(w, "iam", http.StatusConflict, "EntityAlreadyExists", "Instance Profile "+profile+" already exists.")
			return true
		}
		p := s.add(&resource{kind: "instance-profile", id: "profile:" + profile, cidr: profile, tags: tags()})
		reply(profileXML(p))
	case "GetInstanceProfile":
		p := s.res["profile:"+profile]
		if p == nil {
			noSuch("instance profile", profile)
			return true
		}
		reply(profileXML(p))
	case "AddRoleToInstanceProfile", "RemoveRoleFromInstanceProfile":
		p := s.res["profile:"+profile]
		if p == nil {
			noSuch("instance profile", profile)
			return true
		}
		if s.res["role:"+role] == nil {
			noSuch("role", role)
			return true
		}
		if action == "AddRoleToInstanceProfile" {
			if len(p.members) > 0 {
				queryError(w, "iam", http.StatusConflict, "LimitExceeded", "Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1")
				return true
			}
			p.members = []string{role}
		} else {
			p.members = nil
		}
		reply("")
	case "DeleteInstanceProfile":
		p := s.res["profile:"+profile]
		if p == nil {
			noSuch("instance profile", profile)
			return true
		}
		if len(p.members) > 0 {
			queryError(w, "iam", http.StatusConflict, "DeleteConflict", "Cannot delete entity, must remove roles from instance profile first.")
			return true
		}
		for _, o := range s.res {
			if o.kind == "instance" && o.state != "terminated" && o.profile == profile {
				queryError(w, "iam", http.StatusConflict, "DeleteConflict", "Cannot delete entity, it is in use by an instance.")
				return true
			}
		}
		s.remove(p)
		reply("")
	default:
		return false
	}
	return true
}

// revokeIngress removes the rules RevokeSecurityGroupIngress names.
func (s *Server) revokeIngress(w http.ResponseWriter, g *resource, form url.Values) bool {
	for i := 1; ; i++ {
		p := "IpPermissions." + fmt.Sprint(i)
		proto, ok := form[p+".IpProtocol"]
		if !ok {
			return true
		}
		for j := 1; ; j++ {
			c, ok := form[p+".IpRanges."+fmt.Sprint(j)+".CidrIp"]
			if !ok {
				break
			}
			found := false
			for k := range g.ingress {
				q := &g.ingress[k]
				if q.proto != proto[0] || q.from != form.Get(p+".FromPort") || q.to != form.Get(p+".ToPort") {
					continue
				}
				for n, x := range q.cidrs {
					if x == c[0] {
						q.cidrs = append(q.cidrs[:n], q.cidrs[n+1:]...)
						found = true
						break
					}
				}
			}
			if !found {
				queryError(w, "ec2", http.StatusBadRequest, "InvalidPermission.NotFound", "The specified rule does not exist in this security group.")
				return false
			}
		}
	}
}
