package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
)

// The Cloud KMS single-tenant HSM screen (#1469), through the in-process
// HsmManagement server an SDK calls: a project's instances, Create
// instance, and an instance's page with its quorum and proposals, and the
// actions create-manage-single-tenant-hsm runs with gcloud: Create
// proposal, Approve (one signed challenge at a time), Execute and Delete
// proposal.

type kmsHsmProvider struct{ svc *kmsService }

func (kmsHsmProvider) ID() string    { return "kms-hsm" }
func (kmsHsmProvider) Title() string { return "Cloud KMS single-tenant HSM" }

// kmsHsmNote is what an instance here is, and is not.
const kmsHsmNote = "There is no hardware: instances and proposals follow Google's state machine, every operation " +
	"completes at once, and HSM_SINGLE_TENANT keys behave as HSM keys do. Approvals are real: each is a challenge " +
	"signed RSASSA-PKCS1 v1.5 with SHA-256 (`openssl dgst -sha256 -sign`) by a registered RSA 2048 two-factor key."

func (p kmsHsmProvider) hsm() *kms.HsmManagementServer {
	api := p.svc.API()
	if api == nil {
		return nil
	}
	return api.HsmManagement()
}

func (p kmsHsmProvider) List(ctx context.Context, project string) (console.Listing, error) {
	api := p.svc.API()
	if api == nil {
		return console.Listing{}, errors.New(kmsNotStarted)
	}
	listing := console.Listing{
		Columns:    []string{"Location", "State", "Quorum", "Disables", "Created"},
		NameColumn: "Instance",
		Noun:       "single-tenant HSM instances",
		Note:       kmsHsmNote,
	}
	if project == "" {
		listing.Prompt = "Cloud KMS lists single-tenant HSM instances per project. Choose one in the toolbar."
		return listing, nil
	}
	insts, err := api.HsmInstancesInProject(project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, i := range insts {
		listing.Items = append(listing.Items, console.Resource{
			Name: i.GetName(),
			Fields: map[string]string{
				"Location": kmsLocation(i.GetName()),
				"State":    i.GetState().String(),
				"Quorum":   kmsQuorum(i.GetQuorumAuth()),
				"Disables": kmsTime(i.GetDisableTime()),
				"Created":  kmsTime(i.GetCreateTime()),
			},
		})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func kmsOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func kmsQuorum(q *kmspb.SingleTenantHsmInstance_QuorumAuth) string {
	if q.GetRequiredApproverCount() == 0 {
		return fmt.Sprintf("N=%d, keys not registered", q.GetTotalApproverCount())
	}
	return fmt.Sprintf("%d of %d", q.GetRequiredApproverCount(), q.GetTotalApproverCount())
}

func (kmsHsmProvider) CreateForm() (string, []console.Field) {
	return "Create instance", []console.Field{
		{Name: "singleTenantHsmInstanceId", Label: "Instance name", Type: "text", Pattern: `^[A-Za-z0-9_\-]{1,63}$`,
			Help: "Optional: 1-63 letters, digits, hyphens and underscores; empty is a UUID."},
		{Name: "location", Label: "Location", Type: "select", Required: true, Default: "us-central1", Options: kmsLocations()},
		{Name: "totalApproverCount", Label: "Total approvers (N)", Type: "text", Required: true, Default: "3", Pattern: `^[0-9]{1,2}$`,
			Help: "3 to 16 quorum members. A register_2fa_keys proposal then registers their keys and the M of N."},
		{Name: "keyPortabilityEnabled", Label: "Key portability", Type: "checkbox", Help: "Fixed at creation."},
	}
}

func (p kmsHsmProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	h := p.hsm()
	if h == nil {
		return "", errors.New(kmsNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating an instance")
	}
	n, err := strconv.Atoi(strings.TrimSpace(values["totalApproverCount"]))
	if err != nil {
		return "", fmt.Errorf("total approvers: %q is not a number", values["totalApproverCount"])
	}
	op, err := h.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{
		Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["location"]), SingleTenantHsmInstanceId: strings.TrimSpace(values["singleTenantHsmInstanceId"]),
		SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{KeyPortabilityEnabled: values["keyPortabilityEnabled"] == "true",
			QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: int32(n)}}})
	if err != nil {
		return "", err
	}
	inst := &kmspb.SingleTenantHsmInstance{}
	if err := op.GetResponse().UnmarshalTo(inst); err != nil {
		return "", err
	}
	return inst.GetName(), nil
}

func kmsProposalOp(p *kmspb.SingleTenantHsmInstanceProposal) string {
	switch p.GetOperation().(type) {
	case *kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_:
		return "register_2fa_keys"
	case *kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_:
		return "disable_sthi"
	case *kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance_:
		return "enable_sthi"
	case *kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_:
		return "delete_sthi"
	case *kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember_:
		return "add_quorum_member"
	case *kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember_:
		return "remove_quorum_member"
	case *kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_:
		return "refresh_sthi"
	}
	return "—"
}

// kmsProposalChallenges is a proposal's challenges, base64url as the guide
// decodes them, with the key each one is for.
func kmsProposalChallenges(p *kmspb.SingleTenantHsmInstanceProposal) (text string, approved, required int) {
	var b strings.Builder
	write := func(kind string, cs []*kmspb.Challenge) {
		for _, c := range cs {
			fmt.Fprintf(&b, "%s challenge for:\n%s%s\n\n", kind, c.GetPublicKeyPem(), base64.URLEncoding.EncodeToString(c.GetChallenge()))
		}
	}
	if q := p.GetQuorumParameters(); q != nil {
		write("Quorum", q.GetChallenges())
		return b.String(), len(q.GetApprovedTwoFactorPublicKeyPems()), int(q.GetRequiredApproverCount())
	}
	r := p.GetRequiredActionQuorumParameters()
	write("Required", r.GetRequiredChallenges())
	write("Quorum", r.GetQuorumChallenges())
	return b.String(), len(r.GetApprovedTwoFactorPublicKeyPems()), int(r.GetRequiredApproverCount()) + len(r.GetRequiredChallenges())
}

func (p kmsHsmProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	h := p.hsm()
	if h == nil {
		return console.Detail{Unavailable: kmsNotStarted}, nil
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	i, err := h.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: path[0]})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance: " + consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "Resource name", Value: i.GetName()},
		{Label: "State", Value: i.GetState().String()},
		{Label: "Quorum", Value: kmsQuorum(i.GetQuorumAuth())},
		{Label: "Disables unless refreshed", Value: kmsTime(i.GetDisableTime())},
		{Label: "Key portability", Value: strconv.FormatBool(i.GetKeyPortabilityEnabled())},
	}
	if i.GetDeleteTime() != nil {
		props = append(props, console.Property{Label: "Deleted", Value: kmsTime(i.GetDeleteTime())})
	}
	var keys strings.Builder
	for _, k := range i.GetQuorumAuth().GetTwoFactorPublicKeyPems() {
		keys.WriteString(k + "\n")
	}
	proposals := console.Listing{Columns: []string{"Operation", "State", "Approvals", "Expires", "Failure"}, NameColumn: "Proposal", Noun: "proposals"}
	var challenges strings.Builder
	resp, err := h.ListSingleTenantHsmInstanceProposals(ctx, &kmspb.ListSingleTenantHsmInstanceProposalsRequest{Parent: i.GetName(), ShowDeleted: true, PageSize: 1000})
	if err != nil {
		proposals.Prompt = "cannot list proposals: " + consoleErr(err)
	}
	for _, pr := range resp.GetSingleTenantHsmInstanceProposals() {
		text, got, need := kmsProposalChallenges(pr)
		proposals.Items = append(proposals.Items, console.Resource{Name: pr.GetName(), Fields: map[string]string{
			"Operation": kmsProposalOp(pr), "State": pr.GetState().String(), "Approvals": fmt.Sprintf("%d of %d", got, need),
			"Expires": kmsTime(pr.GetExpireTime()), "Failure": kmsOrDash(pr.GetFailureReason())}})
		if pr.GetState() == kmspb.SingleTenantHsmInstanceProposal_PENDING {
			fmt.Fprintf(&challenges, "== %s (%s)\n\n%s", pr.GetName(), kmsProposalOp(pr), text)
		}
	}
	proposals.Total = len(proposals.Items)
	if keys.Len() == 0 {
		keys.WriteString("No two-factor keys are registered: a register_2fa_keys proposal registers them.")
	}
	if challenges.Len() == 0 {
		challenges.WriteString("No proposal is PENDING.")
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Location", Value: kmsLocation(i.GetName())},
			{Label: "State", Value: i.GetState().String()},
			{Label: "Created", Value: kmsTime(i.GetCreateTime())},
		},
		Sections: []console.Section{
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Instance", Properties: props}}, Note: kmsHsmNote},
			{ID: "proposals", Label: "Proposals", Listing: proposals},
			{ID: "challenges", Label: "Challenges", Kind: console.KindText, Text: challenges.String(),
				Note: "Decode a challenge (basenc --base64url -d), sign it with `openssl dgst -sha256 -sign PRIVATE_KEY`, and submit the signature with Approve."},
			{ID: "keys", Label: "Two-factor keys", Kind: console.KindText, Text: keys.String()},
		},
	}, nil
}

func (p kmsHsmProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || p.hsm() == nil || kmsOwned(project, path[0]) != nil {
		return nil
	}
	proposal := console.Field{Name: "proposal", Label: "Proposal ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_\-]{1,63}$`}
	return []console.Action{
		{ID: "propose", Label: "Create proposal", Fields: []console.Field{
			{Name: "proposal", Label: "Proposal ID", Type: "text", Pattern: `^[A-Za-z0-9_\-]{1,63}$`, Help: "Optional; empty is a UUID."},
			{Name: "operationType", Label: "Operation", Type: "select", Required: true, Default: "register_2fa_keys",
				Options: []string{"register_2fa_keys", "disable_sthi", "enable_sthi", "delete_sthi", "add_quorum_member", "remove_quorum_member", "refresh_sthi"}},
			{Name: "requiredApproverCount", Label: "Required approvers (M)", Type: "text", Pattern: `^[0-9]{0,2}$`, Help: "register_2fa_keys only: 2 to N-1."},
			{Name: "twoFactorPublicKeyPems", Label: "Two-factor public keys (PEM)", Type: "textarea",
				Help: "register_2fa_keys: all N RSA 2048 public keys, concatenated. add_quorum_member and remove_quorum_member: the one member's key."},
		}},
		{ID: "approve", Label: "Approve", Fields: []console.Field{proposal,
			{Name: "kind", Label: "Challenge", Type: "select", Default: "quorum", Options: []string{"quorum", "required"},
				Help: "required: add_quorum_member's challenge for the new key."},
			{Name: "publicKeyPem", Label: "Public key (PEM)", Type: "textarea", Required: true},
			{Name: "signedChallenge", Label: "Signed challenge (base64)", Type: "textarea", Required: true}}},
		{ID: "execute", Label: "Execute", Fields: []console.Field{proposal}},
		{ID: "deleteproposal", Label: "Delete proposal", Destructive: true, Fields: []console.Field{proposal}},
	}
}

// kmsPems splits concatenated PEM blocks.
func kmsPems(raw string) []string {
	var out []string
	for _, part := range strings.SplitAfter(strings.TrimSpace(raw), "-----END PUBLIC KEY-----") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part+"\n")
		}
	}
	return out
}

func (p kmsHsmProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	h := p.hsm()
	if h == nil {
		return errors.New(kmsNotStarted)
	}
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return err
	}
	name := path[0] + "/proposals/" + strings.TrimSpace(values["proposal"])
	switch action {
	case "propose":
		pr := &kmspb.SingleTenantHsmInstanceProposal{}
		pems := kmsPems(values["twoFactorPublicKeyPems"])
		one := ""
		if len(pems) > 0 {
			one = pems[0]
		}
		switch values["operationType"] {
		case "register_2fa_keys":
			m, err := strconv.Atoi(strings.TrimSpace(values["requiredApproverCount"]))
			if err != nil {
				return fmt.Errorf("required approvers: %q is not a number", values["requiredApproverCount"])
			}
			pr.Operation = &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_{RegisterTwoFactorAuthKeys: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys{
				RequiredApproverCount: int32(m), TwoFactorPublicKeyPems: pems}}
		case "disable_sthi":
			pr.Operation = &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_{DisableSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance{}}
		case "enable_sthi":
			pr.Operation = &kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance_{EnableSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance{}}
		case "delete_sthi":
			pr.Operation = &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_{DeleteSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance{}}
		case "add_quorum_member":
			pr.Operation = &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember_{AddQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember{TwoFactorPublicKeyPem: one}}
		case "remove_quorum_member":
			pr.Operation = &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember_{RemoveQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember{TwoFactorPublicKeyPem: one}}
		case "refresh_sthi":
			pr.Operation = &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_{RefreshSingleTenantHsmInstance: &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance{}}
		default:
			return fmt.Errorf("operation: %q is not one of the proposal operations", values["operationType"])
		}
		_, err := h.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: path[0],
			SingleTenantHsmInstanceProposalId: strings.TrimSpace(values["proposal"]), SingleTenantHsmInstanceProposal: pr})
		return err
	case "approve":
		sig, err := kmsB64("the signed challenge", values["signedChallenge"])
		if err != nil {
			return err
		}
		reply := []*kmspb.ChallengeReply{{SignedChallenge: sig, PublicKeyPem: strings.TrimSpace(values["publicKeyPem"]) + "\n"}}
		req := &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: name}
		got, err := h.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: name})
		if err != nil {
			return err
		}
		switch {
		case got.GetQuorumParameters() != nil:
			req.ApprovalPayload = &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply_{QuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply{ChallengeReplies: reply}}
		case values["kind"] == "required":
			req.ApprovalPayload = &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply_{RequiredActionQuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply{RequiredChallengeReplies: reply}}
		default:
			req.ApprovalPayload = &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply_{RequiredActionQuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply{QuorumChallengeReplies: reply}}
		}
		_, err = h.ApproveSingleTenantHsmInstanceProposal(ctx, req)
		return err
	case "execute":
		op, err := h.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: name})
		if err != nil {
			return err
		}
		if e := op.GetError(); e != nil {
			return errors.New(e.GetMessage())
		}
		return nil
	case "deleteproposal":
		_, err := h.DeleteSingleTenantHsmInstanceProposal(ctx, &kmspb.DeleteSingleTenantHsmInstanceProposalRequest{Name: name})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Creator   = kmsHsmProvider{}
	_ console.Driller   = kmsHsmProvider{}
	_ console.PathActor = kmsHsmProvider{}
)
