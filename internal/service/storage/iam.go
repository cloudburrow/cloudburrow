package storage

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iamcond"
)

// Bucket IAM (#504), stored and never enforced, under ADR-0006 as amended
// for Cloud Storage: getIamPolicy, setIamPolicy and testIamPermissions on
// b/{bucket}/iam. No method consults a stored policy (rule 1). A set
// returns a new etag, and one carrying a stale etag is refused (rule 2;
// 412 conditionNotMet is the JSON API's code for a failed precondition,
// UNVERIFIED for this method). A version 3 policy without conditions is
// accepted (rule 3).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/buckets/setIamPolicy
//
// IAM Conditions (#1745), as Google's "Overview of IAM Conditions" and
// Cloud Storage's "Use IAM Conditions on buckets" document them: a bucket's
// policy may hold a conditional binding (title, optional description, CEL
// expression internal/iamcond compiles) when the bucket has uniform
// bucket-level access ("To use conditions in the allow policy for a Cloud
// Storage bucket, you must enable uniform bucket-level access"), in a
// version 3 policy; uniform bucket-level access cannot then be disabled
// while the policy has one. A getIamPolicy that does not ask for version 3
// is answered version 1 with each conditional role renamed
// ROLE_withcond_HASH and its condition left out (Google's "Policy
// versions"). A managed folder's policy takes none: Google does not list
// managed folders among the resources that accept conditions, and they are
// refused 400. Google does not publish these errors' text.
// testIamPermissions returns every requested permission on a bucket that
// exists (rule 4). The policy is part of the bucket record, so it follows
// the server's persistence, reset and state capture (rule 5). Managed
// folders (#828) keep theirs the same way, on the managed folder's record
// (managedfolders.go). Object IAM, and every ACL method, stay 501 by name.

// bucketPolicy is a bucket's stored policy. Seq numbers each set; the etag
// encodes it.
type bucketPolicy struct {
	Version  int64           `json:"version"`
	Bindings []policyBinding `json:"bindings,omitempty"`
	Seq      uint64          `json:"seq"`
}

type policyBinding struct {
	Role      string           `json:"role"`
	Members   []string         `json:"members"`
	Condition *policyCondition `json:"condition,omitempty"`
}

// policyCondition is a binding's IAM condition.
type policyCondition struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Expression  string `json:"expression"`
}

// withCond joins a conditional role and its condition's hash in a version
// 1 rendering, as Google's "Policy versions" shows.
const withCond = "_withcond_"

func (c *policyCondition) hash(role string) string {
	sum := sha256.Sum256([]byte(role + "\x00" + c.Title + "\x00" + c.Description + "\x00" + c.Expression))
	return hex.EncodeToString(sum[:])[:20]
}

// conditional is whether p holds a conditional binding.
func (p *bucketPolicy) conditional() bool {
	if p == nil {
		return false
	}
	for _, b := range p.Bindings {
		if b.Condition != nil {
			return true
		}
	}
	return false
}

func policyETag(seq uint64) string {
	return base64.StdEncoding.EncodeToString(binary.AppendUvarint([]byte{0x08}, seq+1))
}

func (s *Server) policyJSON(b bucketRecord) map[string]any {
	return policyDocument("projects/_/buckets/"+b.Name, b.Policy)
}

// policyJSONFor renders b's policy for a reader that asked for version.
func (s *Server) policyJSONFor(b bucketRecord, version int) map[string]any {
	return policyDocumentVersion("projects/_/buckets/"+b.Name, b.Policy, version)
}

// policyDocument is a stored policy as the JSON API's Policy, for the
// resource it is set on; nil is the empty version 1 policy.
func policyDocument(resourceID string, stored *bucketPolicy) map[string]any {
	return policyDocumentVersion(resourceID, stored, 3)
}

// policyDocumentVersion renders stored for a reader that asked for
// version: below 3, a conditional role carries "_withcond_" and its
// condition's hash, and the condition is left out.
func policyDocumentVersion(resourceID string, stored *bucketPolicy, version int) map[string]any {
	p := bucketPolicy{Version: 1}
	if stored != nil {
		p = *stored
	}
	if version < 3 && p.Version == 3 && p.conditional() {
		p.Version = 1
	}
	bindings := []any{}
	for _, bd := range p.Bindings {
		m := map[string]any{"role": bd.Role, "members": bd.Members}
		if c := bd.Condition; c != nil {
			if version < 3 {
				m["role"] = bd.Role + withCond + c.hash(bd.Role)
			} else {
				cm := map[string]any{"title": c.Title, "expression": c.Expression}
				if c.Description != "" {
					cm["description"] = c.Description
				}
				m["condition"] = cm
			}
		}
		bindings = append(bindings, m)
	}
	return map[string]any{
		"kind": "storage#policy", "resourceId": resourceID,
		"version": p.Version, "etag": policyETag(p.Seq), "bindings": bindings,
	}
}

// checkPolicyVersion checks getIamPolicy's optionsRequestedPolicyVersion.
func checkPolicyVersion(r *http.Request) error {
	_, err := requestedPolicyVersion(r)
	return err
}

// requestedPolicyVersion is getIamPolicy's optionsRequestedPolicyVersion,
// 1 when absent.
func requestedPolicyVersion(r *http.Request) (int, error) {
	v := r.URL.Query().Get("optionsRequestedPolicyVersion")
	if v == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 3 {
		return 0, badRequest("Invalid argument: optionsRequestedPolicyVersion=%q must be 1, 2 or 3", v)
	}
	return n, nil
}

// readPolicy reads a setIamPolicy body: the policy to store, its Seq still
// unset, and the etag the caller sent, if any. A conditional binding is
// validated here; whether the resource takes one is the caller's check
// (checkConditional), and a ROLE_withcond_HASH role is left for
// resolveHashes.
func readPolicy(r *http.Request) (bucketPolicy, string, error) {
	body, err := readBody(r)
	if err != nil {
		return bucketPolicy{}, "", err
	}
	var in struct {
		Version  json.Number `json:"version"`
		ETag     string      `json:"etag"`
		Bindings []struct {
			Role      string           `json:"role"`
			Members   []string         `json:"members"`
			Condition *policyCondition `json:"condition"`
		} `json:"bindings"`
	}
	raw, _ := json.Marshal(body)
	if err := json.Unmarshal(raw, &in); err != nil {
		return bucketPolicy{}, "", badRequest("Invalid argument: %v", err)
	}
	for k := range body {
		switch k {
		case "version", "etag", "bindings", "kind", "resourceId":
		default:
			return bucketPolicy{}, "", errorf(http.StatusNotImplemented, "notImplemented", "policy.%s is not implemented", k)
		}
	}
	version := int64(1)
	if in.Version != "" {
		n, err := in.Version.Int64()
		if err != nil || n < 0 || n > 3 {
			return bucketPolicy{}, "", badRequest("Invalid argument: policy.version %s must be 0 to 3", in.Version)
		}
		if n > 0 {
			version = n
		}
	}
	p := bucketPolicy{Version: version}
	hashes := false
	for i, bd := range in.Bindings {
		if bd.Role == "" || len(bd.Members) == 0 {
			return bucketPolicy{}, "", badRequest("Invalid argument: policy.bindings[%d] needs a role and at least one member", i)
		}
		pb := policyBinding{Role: bd.Role, Members: bd.Members}
		if c := bd.Condition; c != nil {
			if c.Title == "" {
				return bucketPolicy{}, "", badRequest("Invalid argument: policy.bindings[%d].condition.title is required", i)
			}
			if strings.TrimSpace(c.Expression) == "" {
				return bucketPolicy{}, "", badRequest("Invalid argument: policy.bindings[%d].condition.expression is required", i)
			}
			if _, err := iamcond.Compile(c.Expression); err != nil {
				return bucketPolicy{}, "", badRequest("Invalid argument: policy.bindings[%d].condition.expression is invalid: %v", i, err)
			}
			if strings.Contains(c.Expression, "iam.googleapis.com/modifiedGrantsByRole") {
				return bucketPolicy{}, "", badRequest("Invalid argument: policy.bindings[%d].condition.expression: iam.googleapis.com/modifiedGrantsByRole is accepted only in the allow policies of projects, folders and organizations", i)
			}
			cc := *c
			pb.Condition = &cc
		} else if strings.Contains(bd.Role, withCond) {
			hashes = true
		}
		p.Bindings = append(p.Bindings, pb)
	}
	explicit := false
	for _, bd := range in.Bindings {
		if bd.Condition != nil {
			explicit = true
		}
	}
	switch {
	case explicit && hashes:
		return bucketPolicy{}, "", badRequest("Policy cannot contain both conditions and condition hashes")
	case explicit && version != 3:
		return bucketPolicy{}, "", badRequest("Invalid argument: a policy with conditional role bindings must be version 3, not %d", version)
	}
	return p, in.ETag, nil
}

// resolveHashes gives each ROLE_withcond_HASH role of p back its condition
// from current, so a version 1 read-modify-write keeps it; the kept
// conditions make p a version 3 policy again.
func resolveHashes(p *bucketPolicy, current *bucketPolicy) error {
	for i, bd := range p.Bindings {
		role, h, ok := strings.Cut(bd.Role, withCond)
		if !ok || bd.Condition != nil {
			continue
		}
		found := false
		if current != nil {
			for _, cb := range current.Bindings {
				if cb.Role == role && cb.Condition != nil && cb.Condition.hash(role) == h {
					cc := *cb.Condition
					p.Bindings[i].Role, p.Bindings[i].Condition, found = role, &cc, true
					break
				}
			}
		}
		if !found {
			return badRequest("Invalid argument: policy.bindings[%d].role %q: the condition hash matches no conditional binding of the current policy; read it with optionsRequestedPolicyVersion=3", i, bd.Role)
		}
		p.Version = 3
	}
	return nil
}

// checkConditional refuses a conditional policy on a bucket without
// uniform bucket-level access.
func checkConditional(b bucketRecord, p bucketPolicy) error {
	if p.conditional() && !uniformAccess(b) {
		return badRequest("Invalid argument: IAM Conditions can be set only on a bucket with iamConfiguration.uniformBucketLevelAccess enabled.")
	}
	return nil
}

// nextPolicy is p stored over current: refused if the caller's etag is
// stale, and numbered one past it otherwise.
func nextPolicy(current *bucketPolicy, p bucketPolicy, sentETag string) (*bucketPolicy, error) {
	var seq uint64
	if current != nil {
		seq = current.Seq
	}
	if sentETag != "" && sentETag != policyETag(seq) {
		return nil, preconditionFailed("The policy's etag %q does not match the current one; read the policy again and retry.", sentETag)
	}
	p.Seq = seq + 1
	return &p, nil
}

func (s *Server) bucketsGetIamPolicy(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	version, err := requestedPolicyVersion(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var b bucketRecord
	err = s.meta.View(func(tx Tx) error {
		var ok bool
		var err error
		if b, ok, err = s.getBucket(tx, name); err != nil {
			return err
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.policyJSONFor(b, version))
}

func (s *Server) bucketsSetIamPolicy(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	p, sentETag, err := readPolicy(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var b bucketRecord
	err = s.meta.Update(func(tx Tx) error {
		var ok bool
		var gerr error
		if b, ok, gerr = s.getBucket(tx, name); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("The specified bucket does not exist.")
		}
		p := p
		if err := resolveHashes(&p, b.Policy); err != nil {
			return err
		}
		if err := checkConditional(b, p); err != nil {
			return err
		}
		next, err := nextPolicy(b.Policy, p, sentETag)
		if err != nil {
			return err
		}
		b.Policy = next
		return putBucket(tx, b)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.policyJSON(b))
}

func (s *Server) bucketsTestIamPermissions(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	perms := r.URL.Query()["permissions"]
	if len(perms) == 0 {
		writeError(w, required("permissions"))
		return
	}
	if err := s.bucketExists(name); err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, map[string]any{"kind": "storage#testIamPermissionsResponse", "permissions": perms})
}
