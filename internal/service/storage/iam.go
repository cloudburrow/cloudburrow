package storage

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strconv"
)

// Bucket IAM (#504), stored and never enforced, under ADR-0006 as amended
// for Cloud Storage: getIamPolicy, setIamPolicy and testIamPermissions on
// b/{bucket}/iam. No method consults a stored policy (rule 1). A set
// returns a new etag, and one carrying a stale etag is refused (rule 2;
// 412 conditionNotMet is the JSON API's code for a failed precondition,
// UNVERIFIED for this method). A condition is 501 naming the field, and a
// version 3 policy without conditions is accepted (rule 3).
// testIamPermissions returns every requested permission on a bucket that
// exists (rule 4). The policy is part of the bucket record, so it follows
// the server's persistence, reset and state capture (rule 5). Object and
// managed-folder IAM, and every ACL method, stay 501 by name.

// bucketPolicy is a bucket's stored policy. Seq numbers each set; the etag
// encodes it.
type bucketPolicy struct {
	Version  int64           `json:"version"`
	Bindings []policyBinding `json:"bindings,omitempty"`
	Seq      uint64          `json:"seq"`
}

type policyBinding struct {
	Role    string   `json:"role"`
	Members []string `json:"members"`
}

func policyETag(seq uint64) string {
	return base64.StdEncoding.EncodeToString(binary.AppendUvarint([]byte{0x08}, seq+1))
}

func (s *Server) policyJSON(b bucketRecord) map[string]any {
	p := bucketPolicy{Version: 1}
	if b.Policy != nil {
		p = *b.Policy
	}
	bindings := []any{}
	for _, bd := range p.Bindings {
		bindings = append(bindings, map[string]any{"role": bd.Role, "members": bd.Members})
	}
	return map[string]any{
		"kind": "storage#policy", "resourceId": "projects/_/buckets/" + b.Name,
		"version": p.Version, "etag": policyETag(p.Seq), "bindings": bindings,
	}
}

func (s *Server) bucketsGetIamPolicy(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	if v := r.URL.Query().Get("optionsRequestedPolicyVersion"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 3 {
			writeError(w, badRequest("Invalid argument: optionsRequestedPolicyVersion=%q must be 1, 2 or 3", v))
			return
		}
	}
	var b bucketRecord
	err := s.meta.View(func(tx Tx) error {
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
	writeResponse(w, r, http.StatusOK, s.policyJSON(b))
}

func (s *Server) bucketsSetIamPolicy(w http.ResponseWriter, r *http.Request) {
	name := pathVar(r, jsonPrefix, 1)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var in struct {
		Version  json.Number `json:"version"`
		ETag     string      `json:"etag"`
		Bindings []struct {
			Role      string          `json:"role"`
			Members   []string        `json:"members"`
			Condition json.RawMessage `json:"condition"`
		} `json:"bindings"`
	}
	raw, _ := json.Marshal(body)
	if err := json.Unmarshal(raw, &in); err != nil {
		writeError(w, badRequest("Invalid argument: %v", err))
		return
	}
	for k := range body {
		switch k {
		case "version", "etag", "bindings", "kind", "resourceId":
		default:
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented", "policy.%s is not implemented", k))
			return
		}
	}
	version := int64(1)
	if in.Version != "" {
		n, err := in.Version.Int64()
		if err != nil || n < 0 || n > 3 {
			writeError(w, badRequest("Invalid argument: policy.version %s must be 0 to 3", in.Version))
			return
		}
		if n > 0 {
			version = n
		}
	}
	p := bucketPolicy{Version: version}
	for i, bd := range in.Bindings {
		if len(bd.Condition) > 0 && string(bd.Condition) != "null" {
			writeError(w, errorf(http.StatusNotImplemented, "notImplemented",
				"policy.bindings[%d].condition is not implemented: IAM Conditions are never stored (ADR-0006)", i))
			return
		}
		if bd.Role == "" || len(bd.Members) == 0 {
			writeError(w, badRequest("Invalid argument: policy.bindings[%d] needs a role and at least one member", i))
			return
		}
		p.Bindings = append(p.Bindings, policyBinding{Role: bd.Role, Members: bd.Members})
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
		var seq uint64
		if b.Policy != nil {
			seq = b.Policy.Seq
		}
		if in.ETag != "" && in.ETag != policyETag(seq) {
			return preconditionFailed("The policy's etag %q does not match the current one; read the policy again and retry.", in.ETag)
		}
		p.Seq = seq + 1
		b.Policy = &p
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
