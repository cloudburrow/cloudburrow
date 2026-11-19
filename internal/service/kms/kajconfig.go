package kms

// Default Key Access Justifications policies (#1577). Cloud KMS's REST
// reference documents a KeyAccessJustificationsPolicyConfig singleton on
// organizations, folders and projects ({parent}/kajPolicyConfig, get and
// PATCH) and projects/{p}:showEffectiveKeyAccessJustificationsPolicyConfig
// (https://docs.cloud.google.com/kms/docs/reference/rest, read 2026-11-18),
// and the set-default-policy guide how they apply
// (https://docs.cloud.google.com/assured-workloads/key-access-justifications/docs/set-default-policy):
//
//   - defaultKeyAccessJustificationPolicy "is only used when a Key Access
//     Justifications policy is not provided in the CreateCryptoKeyRequest.
//     This overrides any default policies in its ancestry. If this field is
//     unset, or is set but contains an empty allowed_access_reasons list, no
//     default Key Access Justifications (KAJ) policy configuration is active"
//     (the discovery document's KeyAccessJustificationsPolicyConfig);
//   - "Projects inherit the default policy from their nearest parent folder
//     or organization", and describing a project returns only the policy set
//     on that project;
//   - "Default Key Access Justifications policies are not applied to existing
//     keys."
//
// googleapis publishes no proto for the service (resources.proto names
// KeyAccessJustificationsPolicyConfig only in a comment), so it is served on
// the JSON API only, which google.golang.org/api/cloudkms/v1 calls. A config
// whose policy is unset or empty is not active, so the effective one is the
// nearest active config, the project's own first (UNVERIFIED: the reference
// says only "the resource closest to the given project in hierarchy"); with
// none, showEffective answers the project's own, inactive config
// (UNVERIFIED; docs re-read 2026-11-18). defaultPolicyAvailable is true, as every example of the
// guide shows: its prerequisite, the Japan Regions control package of
// Assured Workloads, has no local meaning. The enrollment config
// (showEffectiveKeyAccessJustificationsEnrollmentConfig) is 501: whether a
// project is enrolled in KAJ logging and enforcement is Google's account
// state, which the reference does not say how to derive.

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const kajConfigPrefix = "kms/kajpolicyconfig/"

var (
	kajConfigPath = regexp.MustCompile(`^/v1/((?:organizations|folders|projects)/[^/:]+)/kajPolicyConfig$`)
	kajEffective  = regexp.MustCompile(`^/v1/(projects/[^/:]+):(showEffectiveKeyAccessJustificationsPolicyConfig|showEffectiveKeyAccessJustificationsEnrollmentConfig)$`)
)

// kajConfig is a stored KeyAccessJustificationsPolicyConfig.
type kajConfig struct {
	Name   string     `json:"name"`
	Policy *kajPolicy `json:"defaultKeyAccessJustificationPolicy,omitempty"`
}

// active reports whether c's default policy applies: set, with reasons.
func (c kajConfig) active() bool { return c.Policy != nil && len(c.Policy.Reasons) > 0 }

// kajConfigJSON is the resource as the JSON API answers it.
type kajConfigJSON struct {
	Name                   string     `json:"name"`
	DefaultPolicy          *kajPolicy `json:"defaultKeyAccessJustificationPolicy,omitempty"`
	DefaultPolicyAvailable bool       `json:"defaultPolicyAvailable"`
}

func (c kajConfig) json() kajConfigJSON {
	p := c.Policy
	if p != nil && p.Reasons == nil {
		p = &kajPolicy{Reasons: []string{}}
	}
	return kajConfigJSON{Name: c.Name, DefaultPolicy: p, DefaultPolicyAvailable: true}
}

// checkKAJParent checks a config's parent: a project ID or number, a folder
// number (that exists, with a hierarchy) or an organization number.
func (s *Server) checkKAJParent(parent string) error {
	coll, id, _ := strings.Cut(parent, "/")
	switch coll {
	case "projects":
		if !projectRE.MatchString(parent) {
			return apierror.InvalidArgument("%s is not projects/{project}", parent)
		}
	case "folders", "organizations":
		if id == "" || strings.Trim(id, "0123456789") != "" {
			return apierror.InvalidArgument("%s: the %s must be its number", parent, strings.TrimSuffix(coll, "s"))
		}
		if coll == "folders" && s.hierarchy != nil {
			if err := s.hierarchy.FolderExists(parent); err != nil {
				return apierror.NotFound("%s not found", parent)
			}
		}
	}
	return nil
}

func (s *Server) kajConfig(parent string) (kajConfig, error) {
	c := kajConfig{Name: parent + "/kajPolicyConfig"}
	_, err := s.get(dbKey(kajConfigPrefix, c.Name), &c)
	return c, err
}

// effectiveKAJConfig is the nearest active config of project: its own, then
// its folders', nearest first, then its organization's; or the project's own
// when none is.
func (s *Server) effectiveKAJConfig(project string) (kajConfig, error) {
	chain := []string{project}
	if s.hierarchy != nil {
		// A project Resource Manager does not know has no ancestors.
		anc, _ := s.hierarchy.Ancestors(project)
		chain = append(chain, anc...)
	}
	for _, r := range chain {
		c, err := s.kajConfig(r)
		if err != nil {
			return kajConfig{}, err
		}
		if c.active() {
			return c, nil
		}
	}
	return kajConfig{Name: project + "/kajPolicyConfig"}, nil
}

// defaultKAJPolicy is the policy CreateCryptoKey gives a key made under
// parent (projects/{p}/locations/{l}/keyRings/{r}) without one: the
// effective default's, or none.
func (s *Server) defaultKAJPolicy(parent string) (*kajPolicy, error) {
	parts := strings.SplitN(parent, "/", 3)
	if len(parts) < 2 || parts[0] != "projects" {
		return nil, nil
	}
	c, err := s.effectiveKAJConfig("projects/" + parts[1])
	if err != nil || !c.active() {
		return nil, err
	}
	return &kajPolicy{Reasons: append([]string(nil), c.Policy.Reasons...)}, nil
}

// serveKAJConfig answers the KeyAccessJustificationsConfig JSON methods,
// and reports false when r is not one.
func (s *Server) serveKAJConfig(w http.ResponseWriter, r *http.Request) bool {
	if m := kajConfigPath.FindStringSubmatch(r.URL.Path); m != nil {
		switch r.Method {
		case http.MethodGet:
			s.getKAJConfig(w, m[1])
		case http.MethodPatch:
			s.updateKAJConfig(w, r, m[1])
		default:
			writeKAJ(w, nil, apierror.NotFound("%s %s is not a method of KeyAccessJustificationsConfig", r.Method, r.URL.Path))
		}
		return true
	}
	if m := kajEffective.FindStringSubmatch(r.URL.Path); m != nil {
		if r.Method != http.MethodGet {
			writeKAJ(w, nil, apierror.NotFound("%s %s is not a method of KeyAccessJustificationsConfig", r.Method, r.URL.Path))
			return true
		}
		if !projectRE.MatchString(m[1]) {
			writeKAJ(w, nil, apierror.InvalidArgument("project %q must be projects/{project}", m[1]))
			return true
		}
		if m[2] == "showEffectiveKeyAccessJustificationsEnrollmentConfig" {
			writeKAJ(w, nil, apierror.Unimplemented("showEffectiveKeyAccessJustificationsEnrollmentConfig is not served: "+
				"a project's Key Access Justifications enrollment (audit logging, policy enforcement) is Google's account state, "+
				"which Google does not document how to derive; here every key's policy is enforced (#1577)"))
			return true
		}
		c, err := s.effectiveKAJConfig(m[1])
		if err != nil {
			writeKAJ(w, nil, err)
			return true
		}
		writeKAJ(w, map[string]any{"effectiveKajPolicy": c.json()}, nil)
		return true
	}
	return false
}

func (s *Server) getKAJConfig(w http.ResponseWriter, parent string) {
	if err := s.checkKAJParent(parent); err != nil {
		writeKAJ(w, nil, err)
		return
	}
	c, err := s.kajConfig(parent)
	if err != nil {
		writeKAJ(w, nil, err)
		return
	}
	writeKAJ(w, c.json(), nil)
}

// updateKAJConfig sets defaultKeyAccessJustificationPolicy, the one field
// updateMask may name; an empty mask updates it too.
func (s *Server) updateKAJConfig(w http.ResponseWriter, r *http.Request, parent string) {
	if err := s.checkKAJParent(parent); err != nil {
		writeKAJ(w, nil, err)
		return
	}
	for _, p := range strings.Split(r.URL.Query().Get("updateMask"), ",") {
		switch strings.TrimSpace(p) {
		case "", "defaultKeyAccessJustificationPolicy", "default_key_access_justification_policy":
		case "name", "defaultPolicyAvailable", "default_policy_available":
			writeKAJ(w, nil, apierror.InvalidArgument("updateMask path %q names a field that cannot be updated", p))
			return
		default:
			writeKAJ(w, nil, apierror.InvalidArgument("updateMask path %q is not a KeyAccessJustificationsPolicyConfig field", p))
			return
		}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeKAJ(w, nil, apierror.InvalidArgument("the request body could not be read"))
		return
	}
	var in struct {
		Name   *string `json:"name"`
		Policy *struct {
			Reasons []string `json:"allowedAccessReasons"`
		} `json:"defaultKeyAccessJustificationPolicy"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			writeKAJ(w, nil, apierror.InvalidArgument("invalid KeyAccessJustificationsPolicyConfig: %v", err))
			return
		}
	}
	name := parent + "/kajPolicyConfig"
	if in.Name != nil && *in.Name != "" && *in.Name != name {
		writeKAJ(w, nil, apierror.InvalidArgument("keyAccessJustificationsPolicyConfig.name %q does not match the path's %s", *in.Name, name))
		return
	}
	c := kajConfig{Name: name}
	if in.Policy != nil {
		c.Policy = &kajPolicy{}
		for _, reason := range in.Policy.Reasons {
			v, ok := kmspb.AccessReason_value[reason]
			if !ok || v == int32(kmspb.AccessReason_REASON_UNSPECIFIED) {
				writeKAJ(w, nil, apierror.InvalidArgument("defaultKeyAccessJustificationPolicy.allowedAccessReasons %q is not an AccessReason", reason))
				return
			}
			c.Policy.Reasons = append(c.Policy.Reasons, reason)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.put(dbKey(kajConfigPrefix, name), c); err != nil {
		writeKAJ(w, nil, err)
		return
	}
	writeKAJ(w, c.json(), nil)
}

func writeKAJ(w http.ResponseWriter, v any, err error) {
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	out, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}
