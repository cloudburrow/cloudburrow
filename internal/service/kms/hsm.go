package kms

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Single-tenant Cloud HSM (google.cloud.kms.v1.HsmManagement, #1469).
//
// A SingleTenantHsmInstance is administered through proposals that a quorum
// of its administrators approves by signing challenges with their
// two-factor (2FA) keys, as hsm_management.proto and
// https://docs.cloud.google.com/kms/docs/create-manage-single-tenant-hsm
// define:
//
//   - CreateSingleTenantHsmInstance takes quorum_auth.total_approver_count
//     (N, 3 to 16) and leaves the instance PENDING_TWO_FACTOR_AUTH_REGISTRATION.
//   - A proposal carries one operation. register_2fa_keys (the instance must
//     be PENDING_TWO_FACTOR_AUTH_REGISTRATION) sets the N keys and M
//     (required_approver_count, 2 to N-1), and needs every one of the N new
//     keys to sign; disable, refresh, add_quorum_member and
//     remove_quorum_member need an ACTIVE instance, enable a DISABLED one,
//     and delete a DISABLED or PENDING_TWO_FACTOR_AUTH_REGISTRATION one (the
//     operation's field comment; its message comment says only "not DELETING
//     or DELETED", and the stricter reading is served). Each needs M of the
//     N registered keys; add_quorum_member also needs the new key's own
//     signature (required_action_quorum_parameters).
//   - Every challenge is 32 random bytes; a reply is that challenge signed
//     RSASSA-PKCS1 v1.5 over its SHA-256 digest (ChallengeReply.signed_challenge,
//     which is what the guide's `openssl dgst -sign` produces) by the RSA
//     2048 key whose public PEM the reply names. A reply that does not
//     verify, names a key the proposal has no challenge for, or is empty is
//     INVALID_ARGUMENT, and nothing in that request is recorded: an
//     unsigned approval is never accepted.
//   - A proposal expires after ttl or at expire_time (default 1 day, 5
//     minutes to 7 days); only one unexpired PENDING or APPROVED proposal
//     may exist per instance ("delete the active proposal or wait for it to
//     expire"). Deleting one is soft: it is DELETED until its purge_time,
//     the expiry, and then gone.
//   - An instance not refreshed by disable_time (730 days after creation,
//     the last refresh or enable) reads as DISABLED.
//
// What differs: there is no hardware, so every operation completes at
// once. CreateSingleTenantHsmInstance and ExecuteSingleTenantHsmInstanceProposal
// return done operations, and the transitional states (CREATING, DISABLING,
// DELETING, proposal CREATING and RUNNING) are never observed. Keys whose
// crypto_key_backend is an instance are SOFTWARE keys underneath, as HSM
// ones are (protection.go). upgrade_key_trust (#1521) promotes an AES_WRAPPING
// version of a key in the instance to hsm_trusted, with M of the N keys and
// a required signature from the 2FA key it names (trusted.go). The error codes Google returns for a refused
// transition are not documented, and are UNVERIFIED: a request made in the
// wrong state is FAILED_PRECONDITION, a malformed one INVALID_ARGUMENT.

const (
	hsmInstancePrefix = "kms/hsminstance/"
	hsmProposalPrefix = "kms/hsmproposal/"

	// hsmUnrefreshed is unrefreshed_duration_until_disable: "This will have
	// a value of 730 days."
	hsmUnrefreshed = 730 * 24 * time.Hour

	hsmMinApprovers = 3
	hsmMaxApprovers = 16

	proposalDefaultTTL = 24 * time.Hour
	proposalMinTTL     = 5 * time.Minute
	proposalMaxTTL     = 7 * 24 * time.Hour

	challengeBytes = 32
)

// Proposal operations, by the names gcloud's --operation-type uses.
const (
	opRegister = "register_2fa_keys"
	opDisable  = "disable_sthi"
	opEnable   = "enable_sthi"
	opDelete   = "delete_sthi"
	opAdd      = "add_quorum_member"
	opRemove   = "remove_quorum_member"
	opRefresh  = "refresh_sthi"
	opUpgrade  = "upgrade_key_trust"
)

type hsmInstance struct {
	Name        string    `json:"name"`
	Created     time.Time `json:"created"`
	State       string    `json:"state"`
	Total       int       `json:"totalApproverCount"`
	Required    int       `json:"requiredApproverCount,omitempty"`
	Pems        []string  `json:"twoFactorPublicKeyPems,omitempty"`
	Deleted     time.Time `json:"deleteTime,omitempty"`
	DisableAt   time.Time `json:"disableTime,omitempty"`
	Portability bool      `json:"keyPortabilityEnabled,omitempty"`
}

func (i hsmInstance) state() kmspb.SingleTenantHsmInstance_State {
	return kmspb.SingleTenantHsmInstance_State(kmspb.SingleTenantHsmInstance_State_value[i.State])
}

// effective is the instance as it is at now: an ACTIVE instance whose
// disable_time has passed is DISABLED.
func (i hsmInstance) effective(now time.Time) hsmInstance {
	if i.state() == kmspb.SingleTenantHsmInstance_ACTIVE && !i.DisableAt.IsZero() && !now.Before(i.DisableAt) {
		i.State = kmspb.SingleTenantHsmInstance_DISABLED.String()
	}
	return i
}

func (i hsmInstance) proto() *kmspb.SingleTenantHsmInstance {
	out := &kmspb.SingleTenantHsmInstance{
		Name: i.Name, CreateTime: timestamppb.New(i.Created), State: i.state(),
		QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: int32(i.Total),
			RequiredApproverCount: int32(i.Required), TwoFactorPublicKeyPems: i.Pems},
		UnrefreshedDurationUntilDisable: durationpb.New(hsmUnrefreshed),
		KeyPortabilityEnabled:           i.Portability,
	}
	if !i.Deleted.IsZero() {
		out.DeleteTime = timestamppb.New(i.Deleted)
	}
	if !i.DisableAt.IsZero() {
		out.DisableTime = timestamppb.New(i.DisableAt)
	}
	return out
}

type hsmChallenge struct {
	Challenge []byte `json:"challenge"`
	Pem       string `json:"publicKeyPem"`
	Approved  bool   `json:"approved,omitempty"`
}

type hsmProposal struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	State   string    `json:"state"`
	Failure string    `json:"failureReason,omitempty"`
	Op      string    `json:"operation"`
	// RegisterRequired and RegisterPems are register_2fa_keys's; MemberPem
	// is add_quorum_member's or remove_quorum_member's key.
	RegisterRequired int      `json:"registerRequiredApproverCount,omitempty"`
	RegisterPems     []string `json:"registerTwoFactorPublicKeyPems,omitempty"`
	MemberPem        string   `json:"memberPublicKeyPem,omitempty"`
	// Version is upgrade_key_trust's CryptoKeyVersion; its 2FA key is
	// MemberPem.
	Version string `json:"cryptoKeyVersion,omitempty"`
	// Required is M; Quorum the quorum challenges, and Must the challenges
	// that must all be signed (required_action_quorum_parameters, which a
	// proposal has exactly when Must is not empty).
	Required int            `json:"requiredApproverCount"`
	Quorum   []hsmChallenge `json:"challenges"`
	Must     []hsmChallenge `json:"requiredChallenges,omitempty"`
	Expire   time.Time      `json:"expireTime"`
	Deleted  time.Time      `json:"deleteTime,omitempty"`
	Purge    time.Time      `json:"purgeTime,omitempty"`
}

func (p hsmProposal) state() kmspb.SingleTenantHsmInstanceProposal_State {
	return kmspb.SingleTenantHsmInstanceProposal_State(kmspb.SingleTenantHsmInstanceProposal_State_value[p.State])
}

func (p hsmProposal) instance() string { return p.Name[:strings.Index(p.Name, "/proposals/")] }

// active is whether p still blocks another proposal on its instance.
func (p hsmProposal) active(now time.Time) bool {
	st := p.state()
	return (st == kmspb.SingleTenantHsmInstanceProposal_PENDING || st == kmspb.SingleTenantHsmInstanceProposal_APPROVED) && now.Before(p.Expire)
}

func challengesProto(cs []hsmChallenge) (all []*kmspb.Challenge, approved []string) {
	for _, c := range cs {
		all = append(all, &kmspb.Challenge{Challenge: c.Challenge, PublicKeyPem: c.Pem})
		if c.Approved {
			approved = append(approved, c.Pem)
		}
	}
	return all, approved
}

func (p hsmProposal) proto() *kmspb.SingleTenantHsmInstanceProposal {
	out := &kmspb.SingleTenantHsmInstanceProposal{Name: p.Name, CreateTime: timestamppb.New(p.Created), State: p.state(),
		FailureReason: p.Failure, Expiration: &kmspb.SingleTenantHsmInstanceProposal_ExpireTime{ExpireTime: timestamppb.New(p.Expire)}}
	if !p.Deleted.IsZero() {
		out.DeleteTime = timestamppb.New(p.Deleted)
	}
	if !p.Purge.IsZero() {
		out.PurgeTime = timestamppb.New(p.Purge)
	}
	quorum, approved := challengesProto(p.Quorum)
	if len(p.Must) > 0 {
		must, mustApproved := challengesProto(p.Must)
		out.ApprovalParameters = &kmspb.SingleTenantHsmInstanceProposal_RequiredActionQuorumParameters_{RequiredActionQuorumParameters: &kmspb.SingleTenantHsmInstanceProposal_RequiredActionQuorumParameters{
			RequiredChallenges: must, RequiredApproverCount: int32(p.Required), QuorumChallenges: quorum,
			ApprovedTwoFactorPublicKeyPems: append(mustApproved, approved...)}}
	} else {
		out.ApprovalParameters = &kmspb.SingleTenantHsmInstanceProposal_QuorumParameters_{QuorumParameters: &kmspb.SingleTenantHsmInstanceProposal_QuorumParameters{
			RequiredApproverCount: int32(p.Required), Challenges: quorum, ApprovedTwoFactorPublicKeyPems: approved}}
	}
	switch p.Op {
	case opRegister:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_{RegisterTwoFactorAuthKeys: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys{
			RequiredApproverCount: int32(p.RegisterRequired), TwoFactorPublicKeyPems: p.RegisterPems}}
	case opDisable:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_{DisableSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance{}}
	case opEnable:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance_{EnableSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance{}}
	case opDelete:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_{DeleteSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance{}}
	case opAdd:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember_{AddQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember{TwoFactorPublicKeyPem: p.MemberPem}}
	case opRemove:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember_{RemoveQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember{TwoFactorPublicKeyPem: p.MemberPem}}
	case opRefresh:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_{RefreshSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance{}}
	case opUpgrade:
		out.Operation = &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust_{UpgradeKeyTrust: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust{Name: p.Version, TwoFactorPublicKeyPem: p.MemberPem}}
	}
	return out
}

// HsmManagementServer serves google.cloud.kms.v1.HsmManagement.
type HsmManagementServer struct {
	kmspb.UnimplementedHsmManagementServer
	s *Server
}

// HsmManagement returns the HsmManagement service.
func (s *Server) HsmManagement() *HsmManagementServer { return &HsmManagementServer{s: s} }

// parseHsmInstance checks projects/{p}/locations/{l}/singleTenantHsmInstances/{i}
// and returns its location.
func parseHsmInstance(field, name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[4] != "singleTenantHsmInstances" || !idRE.MatchString(parts[5]) {
		return "", badName(field, name, "SingleTenantHsmInstance", "want projects/{project}/locations/{location}/singleTenantHsmInstances/{instance}")
	}
	loc := strings.Join(parts[:4], "/")
	if err := parseLocation(field, loc); err != nil {
		return "", err
	}
	return loc, nil
}

func parseHsmProposal(field, name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 8 || parts[6] != "proposals" || !idRE.MatchString(parts[7]) {
		return "", badName(field, name, "SingleTenantHsmInstanceProposal", "want projects/{project}/locations/{location}/singleTenantHsmInstances/{instance}/proposals/{proposal}")
	}
	return parseHsmInstance(field, strings.Join(parts[:6], "/"))
}

// twoFactorKey parses a 2FA public key PEM: "Public keys must be associated
// with RSA 2048 keys." It returns the key and its canonical DER, which is
// how two PEMs are compared.
func twoFactorKey(field, p string) (*rsa.PublicKey, string, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, "", apierror.InvalidArgument("%s is not a PEM-encoded public key", field)
	}
	var pub any
	var err error
	switch block.Type {
	case "PUBLIC KEY":
		pub, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		pub, err = x509.ParsePKCS1PublicKey(block.Bytes)
	default:
		return nil, "", apierror.InvalidArgument("%s is a PEM %q block: want PUBLIC KEY", field, block.Type)
	}
	if err != nil {
		return nil, "", apierror.InvalidArgument("%s does not parse: %v", field, err)
	}
	rk, ok := pub.(*rsa.PublicKey)
	if !ok || rk.N.BitLen() != 2048 {
		return nil, "", apierror.InvalidArgument("%s must be an RSA 2048 public key", field)
	}
	return rk, string(x509.MarshalPKCS1PublicKey(rk)), nil
}

func samePem(a, b string) bool {
	_, x, err1 := twoFactorKey("", a)
	_, y, err2 := twoFactorKey("", b)
	return err1 == nil && err2 == nil && x == y
}

func newChallenges(pems []string) ([]hsmChallenge, error) {
	out := make([]hsmChallenge, len(pems))
	for i, p := range pems {
		b := make([]byte, challengeBytes)
		if _, err := rand.Read(b); err != nil {
			return nil, apierror.Internal(err, "generate a challenge")
		}
		out[i] = hsmChallenge{Challenge: b, Pem: p}
	}
	return out, nil
}

// doneOp stores and returns a finished operation in loc, with resp as its
// response or, if failed is set, that error.
func (s *Server) doneOp(loc string, md, resp proto.Message, failed *apierror.Error) (*longrunningpb.Operation, error) {
	mdAny, err := anypb.New(md)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode the operation metadata"))
	}
	op := &longrunningpb.Operation{Name: loc + "/operations/" + uuid.NewString(), Done: true, Metadata: mdAny}
	if failed != nil {
		op.Result = &longrunningpb.Operation_Error{Error: failed.GRPCStatus().Proto()}
	} else {
		r, err := anypb.New(resp)
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "encode the operation response"))
		}
		op.Result = &longrunningpb.Operation_Response{Response: r}
	}
	b, err := protojson.Marshal(op)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode the operation"))
	}
	if err := s.db.Put(dbKey(operationPrefix, op.GetName()), b); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "store"))
	}
	return op, nil
}

// loadInstance reads an instance as it is now.
func (s *Server) loadInstance(name string) (hsmInstance, error) {
	var i hsmInstance
	if err := s.load(dbKey(hsmInstancePrefix, name), &i, "SingleTenantHsmInstance", name); err != nil {
		return hsmInstance{}, err
	}
	return i.effective(s.now()), nil
}

// getProposal reads a proposal; one purged after deletion is absent.
func (s *Server) getProposal(name string) (hsmProposal, bool, error) {
	var p hsmProposal
	found, err := s.get(dbKey(hsmProposalPrefix, name), &p)
	if err != nil || !found {
		return hsmProposal{}, false, err
	}
	if p.state() == kmspb.SingleTenantHsmInstanceProposal_DELETED && !s.now().Before(p.Purge) {
		return hsmProposal{}, false, nil
	}
	return p, true, nil
}

func (s *Server) loadProposal(name string) (hsmProposal, error) {
	p, found, err := s.getProposal(name)
	if err != nil {
		return hsmProposal{}, apierror.Wrap(err)
	}
	if !found {
		return hsmProposal{}, apierror.Wrap(apierror.NotFound("SingleTenantHsmInstanceProposal %s not found", name))
	}
	return p, nil
}

func (h *HsmManagementServer) CreateSingleTenantHsmInstance(_ context.Context, req *kmspb.CreateSingleTenantHsmInstanceRequest) (*longrunningpb.Operation, error) {
	s := h.s
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	id := req.GetSingleTenantHsmInstanceId()
	if id == "" {
		// gcloud: "If omitted, a UUID is assigned."
		id = uuid.NewString()
	}
	if !idRE.MatchString(id) {
		return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance_id %q must match [a-zA-Z0-9_-]{1,63}", id))
	}
	in := req.GetSingleTenantHsmInstance()
	if in.GetQuorumAuth() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance.quorum_auth is required"))
	}
	n := int(in.GetQuorumAuth().GetTotalApproverCount())
	if n < hsmMinApprovers || n > hsmMaxApprovers {
		return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance.quorum_auth.total_approver_count %d must be from %d to %d", n, hsmMinApprovers, hsmMaxApprovers))
	}
	name := req.GetParent() + "/singleTenantHsmInstances/" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing hsmInstance
	if found, err := s.get(dbKey(hsmInstancePrefix, name), &existing); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("SingleTenantHsmInstance %s already exists", name))
	}
	now := s.now().UTC()
	// required_approver_count and two_factor_public_key_pems are output
	// only: register_2fa_keys sets them.
	i := hsmInstance{Name: name, Created: now, State: kmspb.SingleTenantHsmInstance_PENDING_TWO_FACTOR_AUTH_REGISTRATION.String(),
		Total: n, DisableAt: now.Add(hsmUnrefreshed), Portability: in.GetKeyPortabilityEnabled()}
	if err := s.put(dbKey(hsmInstancePrefix, name), i); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.doneOp(req.GetParent(), &kmspb.CreateSingleTenantHsmInstanceMetadata{}, i.proto(), nil)
}

func (h *HsmManagementServer) GetSingleTenantHsmInstance(_ context.Context, req *kmspb.GetSingleTenantHsmInstanceRequest) (*kmspb.SingleTenantHsmInstance, error) {
	if _, err := parseHsmInstance("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	i, err := h.s.loadInstance(req.GetName())
	if err != nil {
		return nil, err
	}
	return i.proto(), nil
}

var hsmFilterFields = fieldSupport{"name": {"=", "!=", ":"}, "state": {"=", "!="}}

func (h *HsmManagementServer) ListSingleTenantHsmInstances(_ context.Context, req *kmspb.ListSingleTenantHsmInstancesRequest) (*kmspb.ListSingleTenantHsmInstancesResponse, error) {
	s := h.s
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseFilter(req.GetFilter(), hsmFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	desc, err := parseOrderBy(req.GetOrderBy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(hsmInstancePrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	byN := map[string]hsmInstance{}
	var keep []string
	for _, n := range names {
		i, err := s.loadInstance(n)
		if err != nil {
			return nil, err
		}
		if i.state() == kmspb.SingleTenantHsmInstance_DELETED && !req.GetShowDeleted() {
			continue
		}
		if filter != nil && !filter.match(func(f string) (string, bool) {
			switch f {
			case "name":
				return i.Name, true
			case "state":
				return i.State, true
			}
			return "", false
		}) {
			continue
		}
		byN[n] = i
		keep = append(keep, n)
	}
	page, next, err := orderedPage("kms-hsm:"+req.GetParent()+"|filter="+req.GetFilter(), keep, byName, desc, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListSingleTenantHsmInstancesResponse{NextPageToken: next}
	if filter == nil {
		resp.TotalSize = int32(len(keep))
	}
	for _, n := range page {
		resp.SingleTenantHsmInstances = append(resp.SingleTenantHsmInstances, byN[n].proto())
	}
	return resp, nil
}

// needState is the instance state an operation needs.
func needState(op string) []kmspb.SingleTenantHsmInstance_State {
	switch op {
	case opRegister:
		return []kmspb.SingleTenantHsmInstance_State{kmspb.SingleTenantHsmInstance_PENDING_TWO_FACTOR_AUTH_REGISTRATION}
	case opEnable:
		return []kmspb.SingleTenantHsmInstance_State{kmspb.SingleTenantHsmInstance_DISABLED}
	case opDelete:
		return []kmspb.SingleTenantHsmInstance_State{kmspb.SingleTenantHsmInstance_DISABLED, kmspb.SingleTenantHsmInstance_PENDING_TWO_FACTOR_AUTH_REGISTRATION}
	}
	return []kmspb.SingleTenantHsmInstance_State{kmspb.SingleTenantHsmInstance_ACTIVE}
}

// checkProposal is checkOperation, with upgrade_key_trust's checks of the
// version it names.
func (s *Server) checkProposal(i hsmInstance, p hsmProposal) *apierror.Error {
	if e := checkOperation(i, p); e != nil {
		return e
	}
	if p.Op == opUpgrade {
		return s.checkUpgrade(i, p)
	}
	return nil
}

// checkOperation is whether op can run on i now, for proposal p.
func checkOperation(i hsmInstance, p hsmProposal) *apierror.Error {
	ok := false
	for _, st := range needState(p.Op) {
		ok = ok || i.state() == st
	}
	if !ok {
		return apierror.FailedPrecondition("SingleTenantHsmInstance %s is %s: %s needs it %v", i.Name, i.state(), p.Op, needState(p.Op))
	}
	switch p.Op {
	case opRegister:
		if len(p.RegisterPems) != i.Total {
			return apierror.InvalidArgument("register_two_factor_auth_keys.two_factor_public_key_pems has %d keys: the instance's total_approver_count is %d", len(p.RegisterPems), i.Total)
		}
		if m := p.RegisterRequired; m < 2 || m > i.Total-1 {
			return apierror.InvalidArgument("register_two_factor_auth_keys.required_approver_count %d must be from 2 to %d (total_approver_count - 1)", m, i.Total-1)
		}
	case opAdd:
		for _, k := range i.Pems {
			if samePem(k, p.MemberPem) {
				return apierror.FailedPrecondition("add_quorum_member.two_factor_public_key_pem is already a quorum member of %s", i.Name)
			}
		}
		if i.Total+1 > hsmMaxApprovers {
			return apierror.FailedPrecondition("%s has %d quorum members, the most total_approver_count allows", i.Name, i.Total)
		}
	case opRemove:
		member := false
		for _, k := range i.Pems {
			member = member || samePem(k, p.MemberPem)
		}
		if !member {
			return apierror.FailedPrecondition("remove_quorum_member.two_factor_public_key_pem is not a quorum member of %s", i.Name)
		}
		// total_approver_count stays at least 3, and required_approver_count
		// at most total_approver_count - 1.
		if i.Total-1 < hsmMinApprovers || i.Required > i.Total-2 {
			return apierror.FailedPrecondition("removing a quorum member from %s would leave %d members for a required_approver_count of %d: total_approver_count must be at least %d and greater than required_approver_count", i.Name, i.Total-1, i.Required, hsmMinApprovers)
		}
	}
	return nil
}

func (h *HsmManagementServer) CreateSingleTenantHsmInstanceProposal(_ context.Context, req *kmspb.CreateSingleTenantHsmInstanceProposalRequest) (*longrunningpb.Operation, error) {
	s := h.s
	loc, err := parseHsmInstance("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	id := req.GetSingleTenantHsmInstanceProposalId()
	if id == "" {
		id = uuid.NewString()
	}
	if !idRE.MatchString(id) {
		return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance_proposal_id %q must match [a-zA-Z0-9_-]{1,63}", id))
	}
	in := req.GetSingleTenantHsmInstanceProposal()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance_proposal is required"))
	}
	now := s.now().UTC()
	p := hsmProposal{Name: req.GetParent() + "/proposals/" + id, Created: now, State: kmspb.SingleTenantHsmInstanceProposal_PENDING.String()}
	switch op := in.GetOperation().(type) {
	case *kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_:
		p.Op, p.RegisterRequired = opRegister, int(op.RegisterTwoFactorAuthKeys.GetRequiredApproverCount())
		seen := map[string]bool{}
		for n, k := range op.RegisterTwoFactorAuthKeys.GetTwoFactorPublicKeyPems() {
			_, der, err := twoFactorKey("register_two_factor_auth_keys.two_factor_public_key_pems["+strconv.Itoa(n)+"]", k)
			if err != nil {
				return nil, apierror.Wrap(err)
			}
			if seen[der] {
				return nil, apierror.Wrap(apierror.InvalidArgument("register_two_factor_auth_keys.two_factor_public_key_pems[%d] repeats a key", n))
			}
			seen[der] = true
			p.RegisterPems = append(p.RegisterPems, k)
		}
	case *kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_:
		p.Op = opDisable
	case *kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance_:
		p.Op = opEnable
	case *kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_:
		p.Op = opDelete
	case *kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember_:
		p.Op, p.MemberPem = opAdd, op.AddQuorumMember.GetTwoFactorPublicKeyPem()
		if _, _, err := twoFactorKey("add_quorum_member.two_factor_public_key_pem", p.MemberPem); err != nil {
			return nil, apierror.Wrap(err)
		}
	case *kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember_:
		p.Op, p.MemberPem = opRemove, op.RemoveQuorumMember.GetTwoFactorPublicKeyPem()
		if _, _, err := twoFactorKey("remove_quorum_member.two_factor_public_key_pem", p.MemberPem); err != nil {
			return nil, apierror.Wrap(err)
		}
	case *kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_:
		p.Op = opRefresh
	case *kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust_:
		p.Op, p.Version, p.MemberPem = opUpgrade, op.UpgradeKeyTrust.GetName(), op.UpgradeKeyTrust.GetTwoFactorPublicKeyPem()
		if _, _, err := parseCryptoKeyVersion("upgrade_key_trust.name", p.Version); err != nil {
			return nil, apierror.Wrap(err)
		}
		if _, _, err := twoFactorKey("upgrade_key_trust.two_factor_public_key_pem", p.MemberPem); err != nil {
			return nil, apierror.Wrap(err)
		}
	default:
		return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance_proposal.operation is required"))
	}
	ttl := proposalDefaultTTL
	switch e := in.GetExpiration().(type) {
	case *kmspb.SingleTenantHsmInstanceProposal_Ttl:
		if err := e.Ttl.CheckValid(); err != nil {
			return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance_proposal.ttl: %v", err))
		}
		ttl = e.Ttl.AsDuration()
	case *kmspb.SingleTenantHsmInstanceProposal_ExpireTime:
		if err := e.ExpireTime.CheckValid(); err != nil {
			return nil, apierror.Wrap(apierror.InvalidArgument("single_tenant_hsm_instance_proposal.expire_time: %v", err))
		}
		ttl = e.ExpireTime.AsTime().Sub(now)
	}
	if ttl < proposalMinTTL || ttl > proposalMaxTTL {
		return nil, apierror.Wrap(apierror.InvalidArgument("the proposal's expiration is %s away: it must be from 5 minutes to 7 days", ttl.Round(time.Second)))
	}
	p.Expire = now.Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.loadInstance(req.GetParent())
	if err != nil {
		return nil, err
	}
	if e := s.checkProposal(i, p); e != nil {
		return nil, apierror.Wrap(e)
	}
	if _, found, err := s.getProposal(p.Name); err != nil {
		return nil, apierror.Wrap(err)
	} else if found {
		return nil, apierror.Wrap(apierror.AlreadyExists("SingleTenantHsmInstanceProposal %s already exists", p.Name))
	}
	others, err := s.list(hsmProposalPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	for _, n := range others {
		if o, found, err := s.getProposal(n); err != nil {
			return nil, apierror.Wrap(err)
		} else if found && o.active(now) {
			return nil, apierror.Wrap(apierror.FailedPrecondition("SingleTenantHsmInstance %s already has the %s proposal %s: delete it or wait for it to expire", i.Name, o.State, o.Name))
		}
	}
	// register_2fa_keys needs every new key to sign; the others M of the N
	// registered keys, and add_quorum_member the new key as well.
	keys, required := i.Pems, i.Required
	if p.Op == opRegister {
		keys, required = p.RegisterPems, len(p.RegisterPems)
	}
	p.Required = required
	if p.Quorum, err = newChallenges(keys); err != nil {
		return nil, apierror.Wrap(err)
	}
	if p.Op == opAdd || p.Op == opUpgrade {
		if p.Must, err = newChallenges([]string{p.MemberPem}); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	if err := s.put(dbKey(hsmProposalPrefix, p.Name), p); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.doneOp(loc, &kmspb.CreateSingleTenantHsmInstanceProposalMetadata{}, p.proto(), nil)
}

func (h *HsmManagementServer) GetSingleTenantHsmInstanceProposal(_ context.Context, req *kmspb.GetSingleTenantHsmInstanceProposalRequest) (*kmspb.SingleTenantHsmInstanceProposal, error) {
	if _, err := parseHsmProposal("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := h.s.loadProposal(req.GetName())
	if err != nil {
		return nil, err
	}
	return p.proto(), nil
}

func (h *HsmManagementServer) ListSingleTenantHsmInstanceProposals(_ context.Context, req *kmspb.ListSingleTenantHsmInstanceProposalsRequest) (*kmspb.ListSingleTenantHsmInstanceProposalsResponse, error) {
	s := h.s
	if _, err := parseHsmInstance("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseFilter(req.GetFilter(), hsmFilterFields)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	desc, err := parseOrderBy(req.GetOrderBy())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if _, err := s.loadInstance(req.GetParent()); err != nil {
		return nil, err
	}
	names, err := s.list(hsmProposalPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	byN := map[string]hsmProposal{}
	var keep []string
	for _, n := range names {
		p, found, err := s.getProposal(n)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if !found || (p.state() == kmspb.SingleTenantHsmInstanceProposal_DELETED && !req.GetShowDeleted()) {
			continue
		}
		if filter != nil && !filter.match(func(f string) (string, bool) {
			switch f {
			case "name":
				return p.Name, true
			case "state":
				return p.State, true
			}
			return "", false
		}) {
			continue
		}
		byN[n] = p
		keep = append(keep, n)
	}
	page, next, err := orderedPage("kms-hsmproposals:"+req.GetParent()+"|filter="+req.GetFilter(), keep, byName, desc, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListSingleTenantHsmInstanceProposalsResponse{NextPageToken: next}
	if filter == nil {
		resp.TotalSize = int32(len(keep))
	}
	for _, n := range page {
		resp.SingleTenantHsmInstanceProposals = append(resp.SingleTenantHsmInstanceProposals, byN[n].proto())
	}
	return resp, nil
}

// verifyReplies checks every reply against cs and returns the indexes of
// the challenges they sign. Each must name a key cs has a challenge for, and
// sign that challenge RSASSA-PKCS1 v1.5 over SHA-256.
func verifyReplies(field string, replies []*kmspb.ChallengeReply, cs []hsmChallenge) ([]int, error) {
	var out []int
	for n, r := range replies {
		f := field + "[" + strconv.Itoa(n) + "]"
		if len(r.GetSignedChallenge()) == 0 {
			return nil, apierror.InvalidArgument("%s.signed_challenge is required", f)
		}
		pub, der, err := twoFactorKey(f+".public_key_pem", r.GetPublicKeyPem())
		if err != nil {
			return nil, err
		}
		at := -1
		for j, c := range cs {
			if _, d, err := twoFactorKey("", c.Pem); err == nil && d == der {
				at = j
			}
		}
		if at < 0 {
			return nil, apierror.InvalidArgument("%s.public_key_pem is not a key this proposal has a %s challenge for", f, strings.TrimSuffix(field, "_replies"))
		}
		digest := sha256.Sum256(cs[at].Challenge)
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], r.GetSignedChallenge()); err != nil {
			return nil, apierror.InvalidArgument("%s.signed_challenge does not verify: it must be the challenge signed RSASSA-PKCS1 v1.5 with SHA-256 by the key in public_key_pem", f)
		}
		out = append(out, at)
	}
	return out, nil
}

func countApproved(cs []hsmChallenge) int {
	n := 0
	for _, c := range cs {
		if c.Approved {
			n++
		}
	}
	return n
}

func (h *HsmManagementServer) ApproveSingleTenantHsmInstanceProposal(_ context.Context, req *kmspb.ApproveSingleTenantHsmInstanceProposalRequest) (*kmspb.ApproveSingleTenantHsmInstanceProposalResponse, error) {
	s := h.s
	if _, err := parseHsmProposal("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadProposal(req.GetName())
	if err != nil {
		return nil, err
	}
	if st := p.state(); st != kmspb.SingleTenantHsmInstanceProposal_PENDING {
		return nil, apierror.Wrap(apierror.FailedPrecondition("SingleTenantHsmInstanceProposal %s is %s: only a PENDING proposal can be approved", p.Name, st))
	}
	if !s.now().Before(p.Expire) {
		return nil, apierror.Wrap(apierror.FailedPrecondition("SingleTenantHsmInstanceProposal %s expired at %s", p.Name, p.Expire.Format(time.RFC3339)))
	}
	// "The type of approval payload must correspond to the type of
	// approval_parameters in the proposal."
	var quorum, must []int
	switch pl := req.GetApprovalPayload().(type) {
	case *kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply_:
		if len(p.Must) > 0 {
			return nil, apierror.Wrap(apierror.InvalidArgument("%s has required_action_quorum_parameters: approve it with required_action_quorum_reply", p.Name))
		}
		if len(pl.QuorumReply.GetChallengeReplies()) == 0 {
			return nil, apierror.Wrap(apierror.InvalidArgument("quorum_reply.challenge_replies is required"))
		}
		if quorum, err = verifyReplies("quorum_reply.challenge_replies", pl.QuorumReply.GetChallengeReplies(), p.Quorum); err != nil {
			return nil, apierror.Wrap(err)
		}
	case *kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply_:
		if len(p.Must) == 0 {
			return nil, apierror.Wrap(apierror.InvalidArgument("%s has quorum_parameters: approve it with quorum_reply", p.Name))
		}
		r := pl.RequiredActionQuorumReply
		if len(r.GetRequiredChallengeReplies())+len(r.GetQuorumChallengeReplies()) == 0 {
			return nil, apierror.Wrap(apierror.InvalidArgument("required_action_quorum_reply has no challenge replies"))
		}
		if must, err = verifyReplies("required_action_quorum_reply.required_challenge_replies", r.GetRequiredChallengeReplies(), p.Must); err != nil {
			return nil, apierror.Wrap(err)
		}
		if quorum, err = verifyReplies("required_action_quorum_reply.quorum_challenge_replies", r.GetQuorumChallengeReplies(), p.Quorum); err != nil {
			return nil, apierror.Wrap(err)
		}
	default:
		return nil, apierror.Wrap(apierror.InvalidArgument("approval_payload is required: quorum_reply or required_action_quorum_reply"))
	}
	// Every reply verified; only now is any recorded.
	for _, j := range quorum {
		p.Quorum[j].Approved = true
	}
	for _, j := range must {
		p.Must[j].Approved = true
	}
	if countApproved(p.Quorum) >= p.Required && countApproved(p.Must) == len(p.Must) {
		p.State = kmspb.SingleTenantHsmInstanceProposal_APPROVED.String()
	}
	if err := s.put(dbKey(hsmProposalPrefix, p.Name), p); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &kmspb.ApproveSingleTenantHsmInstanceProposalResponse{}, nil
}

func (h *HsmManagementServer) ExecuteSingleTenantHsmInstanceProposal(_ context.Context, req *kmspb.ExecuteSingleTenantHsmInstanceProposalRequest) (*longrunningpb.Operation, error) {
	s := h.s
	loc, err := parseHsmProposal("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadProposal(req.GetName())
	if err != nil {
		return nil, err
	}
	if st := p.state(); st != kmspb.SingleTenantHsmInstanceProposal_APPROVED {
		return nil, apierror.Wrap(apierror.FailedPrecondition("SingleTenantHsmInstanceProposal %s is %s: only an APPROVED proposal can be executed", p.Name, st))
	}
	now := s.now().UTC()
	if !now.Before(p.Expire) {
		return nil, apierror.Wrap(apierror.FailedPrecondition("SingleTenantHsmInstanceProposal %s expired at %s", p.Name, p.Expire.Format(time.RFC3339)))
	}
	i, err := s.loadInstance(p.instance())
	if err != nil {
		return nil, err
	}
	md := &kmspb.ExecuteSingleTenantHsmInstanceProposalMetadata{}
	// The instance may have changed since the proposal was made (an
	// unrefreshed instance disables itself): the proposal then FAILS.
	if e := s.checkProposal(i, p); e != nil {
		p.State, p.Failure = kmspb.SingleTenantHsmInstanceProposal_FAILED.String(), e.Message
		if err := s.put(dbKey(hsmProposalPrefix, p.Name), p); err != nil {
			return nil, apierror.Wrap(err)
		}
		return s.doneOp(loc, md, nil, e)
	}
	switch p.Op {
	case opRegister:
		i.Pems, i.Required, i.State, i.DisableAt = p.RegisterPems, p.RegisterRequired, kmspb.SingleTenantHsmInstance_ACTIVE.String(), now.Add(hsmUnrefreshed)
	case opDisable:
		i.State = kmspb.SingleTenantHsmInstance_DISABLED.String()
	case opEnable:
		i.State, i.DisableAt = kmspb.SingleTenantHsmInstance_ACTIVE.String(), now.Add(hsmUnrefreshed)
	case opDelete:
		i.State, i.Deleted = kmspb.SingleTenantHsmInstance_DELETED.String(), now
	case opAdd:
		i.Pems, i.Total = append(append([]string(nil), i.Pems...), p.MemberPem), i.Total+1
	case opRemove:
		var keep []string
		for _, k := range i.Pems {
			if !samePem(k, p.MemberPem) {
				keep = append(keep, k)
			}
		}
		i.Pems, i.Total = keep, len(keep)
	case opRefresh:
		i.DisableAt = now.Add(hsmUnrefreshed)
	case opUpgrade:
		if err := s.upgradeTrust(p.Version); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	if err := s.put(dbKey(hsmInstancePrefix, i.Name), i); err != nil {
		return nil, apierror.Wrap(err)
	}
	p.State = kmspb.SingleTenantHsmInstanceProposal_SUCCEEDED.String()
	if err := s.put(dbKey(hsmProposalPrefix, p.Name), p); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.doneOp(loc, md, &kmspb.ExecuteSingleTenantHsmInstanceProposalResponse{}, nil)
}

// DeleteSingleTenantHsmInstanceProposal soft-deletes a proposal: it is
// DELETED, cannot be executed, no longer blocks a new proposal, and is
// purged at its purge_time, which is its expiry ("a time after expiration
// of the proposal").
func (h *HsmManagementServer) DeleteSingleTenantHsmInstanceProposal(_ context.Context, req *kmspb.DeleteSingleTenantHsmInstanceProposalRequest) (*emptypb.Empty, error) {
	s := h.s
	if _, err := parseHsmProposal("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.loadProposal(req.GetName())
	if err != nil {
		return nil, err
	}
	if p.state() == kmspb.SingleTenantHsmInstanceProposal_DELETED {
		return nil, apierror.Wrap(apierror.FailedPrecondition("SingleTenantHsmInstanceProposal %s is already DELETED", p.Name))
	}
	now := s.now().UTC()
	p.State, p.Deleted, p.Purge = kmspb.SingleTenantHsmInstanceProposal_DELETED.String(), now, p.Expire
	if p.Purge.Before(now) {
		p.Purge = now
	}
	if err := s.put(dbKey(hsmProposalPrefix, p.Name), p); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// hsmBackend resolves an HSM_SINGLE_TENANT key's crypto_key_backend: an
// existing instance in the key ring's location (its project may differ:
// "projects/INSTANCE_PROJECT/..."), which must be ACTIVE.
func (s *Server) hsmBackend(field, named, keyRing string) (string, error) {
	if named == "" {
		return "", apierror.InvalidArgument("%s is required at protection level HSM_SINGLE_TENANT: it names the SingleTenantHsmInstance", field)
	}
	loc, err := parseHsmInstance(field, named)
	if err != nil {
		return "", err
	}
	ringLoc := keyRing[:strings.Index(keyRing, "/keyRings/")]
	if loc[strings.LastIndex(loc, "/")+1:] != ringLoc[strings.LastIndex(ringLoc, "/")+1:] {
		return "", apierror.InvalidArgument("%s %s is not in the key ring's location %s", field, named, ringLoc[strings.LastIndex(ringLoc, "/")+1:])
	}
	var i hsmInstance
	found, err := s.get(dbKey(hsmInstancePrefix, named), &i)
	if err != nil {
		return "", err
	}
	if !found {
		return "", apierror.InvalidArgument("%s %s does not exist", field, named)
	}
	if st := i.effective(s.now()).state(); st != kmspb.SingleTenantHsmInstance_ACTIVE {
		return "", apierror.FailedPrecondition("%s %s is %s: keys and import jobs can be created only in an ACTIVE SingleTenantHsmInstance", field, named, st)
	}
	return named, nil
}

// backendUsable refuses a cryptographic operation on, or a new version of,
// an HSM_SINGLE_TENANT key whose instance is not ACTIVE: "Keys in a
// DISABLED or deleted instance still show ACTIVE but fail cryptographic
// operations."
func (s *Server) backendUsable(k cryptoKey) error {
	if k.protectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		return nil
	}
	var i hsmInstance
	found, err := s.get(dbKey(hsmInstancePrefix, k.Backend), &i)
	if err != nil {
		return apierror.Wrap(err)
	}
	st := kmspb.SingleTenantHsmInstance_DELETED
	if found {
		st = i.effective(s.now()).state()
	}
	if st != kmspb.SingleTenantHsmInstance_ACTIVE {
		return apierror.Wrap(apierror.FailedPrecondition("CryptoKey %s is in the SingleTenantHsmInstance %s, which is %s: its keys can be used only while it is ACTIVE", k.Name, k.Backend, st))
	}
	return nil
}

// HsmInstancesInProject returns every SingleTenantHsmInstance of project,
// deleted ones included, ordered by name, for the console.
func (s *Server) HsmInstancesInProject(project string) ([]*kmspb.SingleTenantHsmInstance, error) {
	if err := parseLocation("project", "projects/"+project+"/locations/global"); err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(hsmInstancePrefix, "projects/"+project+"/locations")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var out []*kmspb.SingleTenantHsmInstance
	for _, n := range names {
		i, err := s.loadInstance(n)
		if err != nil {
			return nil, err
		}
		out = append(out, i.proto())
	}
	return out, nil
}
