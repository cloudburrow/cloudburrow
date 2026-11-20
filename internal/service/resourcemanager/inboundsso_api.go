package resourcemanager

import (
	"fmt"
	"net/http"

	ci "google.golang.org/api/cloudidentity/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Cloud Identity's inbound SSO JSON surface (inboundsso.go, #1959):
// /v1/inboundSamlSsoProfiles (with idpCredentials),
// /v1/inboundOidcSsoProfiles and /v1/inboundSsoAssignments. Mutations
// answer a done Operation whose response is the resource.

const (
	samlProfileType   = "type.googleapis.com/google.apps.cloudidentity.inboundsso.v1.InboundSamlSsoProfile"
	oidcProfileType   = "type.googleapis.com/google.apps.cloudidentity.inboundsso.v1.InboundOidcSsoProfile"
	idpCredentialType = "type.googleapis.com/google.apps.cloudidentity.inboundsso.v1.IdpCredential"
	ssoAssignmentType = "type.googleapis.com/google.apps.cloudidentity.inboundsso.v1.InboundSsoAssignment"
)

// ssoPage pages an SSO listing: "The maximum allowed value is 100.
// Requests with page_size greater than that will be silently interpreted
// as 100."
func ssoPage(r *http.Request, scope string, keys []string) ([]string, string, error) {
	size, err := rest.QueryInt(r, "pageSize", 0)
	if err != nil {
		return nil, "", err
	}
	if size < 0 {
		return nil, "", apierror.InvalidArgument("pageSize must not be negative")
	}
	if size == 0 {
		size = ssoDefaultPage
	}
	size = min(size, ssoMaxPage)
	page, next, err := paging.Page(scope, keys, r.URL.Query().Get("pageToken"), size)
	if err != nil {
		return nil, "", apierror.InvalidArgument("Invalid pageToken: %v", err)
	}
	return page, next, nil
}

func samlJSON(p *SAMLProfile) *ci.InboundSamlSsoProfile {
	out := p.Profile
	return &out
}

func oidcJSON(p *OIDCProfile) *ci.InboundOidcSsoProfile {
	out := p.Profile
	if out.RpConfig != nil {
		rp := *out.RpConfig
		rp.ClientSecret = "" // input only
		out.RpConfig = &rp
	}
	return &out
}

func (r *Registry) inboundSSORoutes(rt *rest.Router) {
	// SAML profiles.
	rt.Handle("POST /v1/inboundSamlSsoProfiles", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.InboundSamlSsoProfile
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		p, err := r.CreateSAMLProfile(in)
		if err != nil {
			return err
		}
		return doneOp(w, samlProfileType, samlJSON(p))
	})
	rt.Handle("GET /v1/inboundSamlSsoProfiles", func(w http.ResponseWriter, req *http.Request) error {
		if err := ssoFilter(req.URL.Query().Get("filter")); err != nil {
			return err
		}
		ps, err := r.ListSAMLProfiles()
		if err != nil {
			return err
		}
		by := map[string]*SAMLProfile{}
		var keys []string
		for _, p := range ps {
			by[p.Profile.Name], keys = p, append(keys, p.Profile.Name)
		}
		page, next, err := ssoPage(req, "saml", keys)
		if err != nil {
			return err
		}
		resp := ci.ListInboundSamlSsoProfilesResponse{NextPageToken: next, InboundSamlSsoProfiles: []*ci.InboundSamlSsoProfile{}}
		for _, k := range page {
			resp.InboundSamlSsoProfiles = append(resp.InboundSamlSsoProfiles, samlJSON(by[k]))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/inboundSamlSsoProfiles/{id}", func(w http.ResponseWriter, req *http.Request) error {
		p, err := r.GetSAMLProfile("inboundSamlSsoProfiles/" + req.PathValue("id"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, samlJSON(p))
	})
	rt.Handle("PATCH /v1/inboundSamlSsoProfiles/{id}", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.InboundSamlSsoProfile
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		p, err := r.UpdateSAMLProfile("inboundSamlSsoProfiles/"+req.PathValue("id"), in, ciMask(req.URL.Query().Get("updateMask")))
		if err != nil {
			return err
		}
		return doneOp(w, samlProfileType, samlJSON(p))
	})
	rt.Handle("DELETE /v1/inboundSamlSsoProfiles/{id}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteSAMLProfile("inboundSamlSsoProfiles/" + req.PathValue("id")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})
	// IdP credentials.
	rt.Handle("POST /v1/inboundSamlSsoProfiles/{id}/{verb}", func(w http.ResponseWriter, req *http.Request) error {
		if req.PathValue("verb") != "idpCredentials:add" {
			return apierror.NotFound("no such method: POST %s", req.URL.Path)
		}
		var in ci.AddIdpCredentialRequest
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		p, c, err := r.AddIdPCredential("inboundSamlSsoProfiles/"+req.PathValue("id"), in.PemData)
		if err != nil {
			return err
		}
		return doneOp(w, idpCredentialType, c.JSON(p.Profile.Name))
	})
	rt.Handle("GET /v1/inboundSamlSsoProfiles/{id}/idpCredentials", func(w http.ResponseWriter, req *http.Request) error {
		p, err := r.GetSAMLProfile("inboundSamlSsoProfiles/" + req.PathValue("id"))
		if err != nil {
			return err
		}
		by := map[string]IdPCredential{}
		var keys []string
		for _, c := range p.Credentials {
			by[c.ID], keys = c, append(keys, c.ID)
		}
		page, next, err := ssoPage(req, "creds:"+p.Profile.Name, keys)
		if err != nil {
			return err
		}
		resp := ci.ListIdpCredentialsResponse{NextPageToken: next, IdpCredentials: []*ci.IdpCredential{}}
		for _, k := range page {
			resp.IdpCredentials = append(resp.IdpCredentials, by[k].JSON(p.Profile.Name))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/inboundSamlSsoProfiles/{id}/idpCredentials/{cred}", func(w http.ResponseWriter, req *http.Request) error {
		p, c, err := r.GetIdPCredential("inboundSamlSsoProfiles/" + req.PathValue("id") + "/idpCredentials/" + req.PathValue("cred"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, c.JSON(p.Profile.Name))
	})
	rt.Handle("DELETE /v1/inboundSamlSsoProfiles/{id}/idpCredentials/{cred}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteIdPCredential("inboundSamlSsoProfiles/" + req.PathValue("id") + "/idpCredentials/" + req.PathValue("cred")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})

	// OIDC profiles.
	rt.Handle("POST /v1/inboundOidcSsoProfiles", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.InboundOidcSsoProfile
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		p, err := r.CreateOIDCProfile(in)
		if err != nil {
			return err
		}
		return doneOp(w, oidcProfileType, oidcJSON(p))
	})
	rt.Handle("GET /v1/inboundOidcSsoProfiles", func(w http.ResponseWriter, req *http.Request) error {
		if err := ssoFilter(req.URL.Query().Get("filter")); err != nil {
			return err
		}
		ps, err := r.ListOIDCProfiles()
		if err != nil {
			return err
		}
		by := map[string]*OIDCProfile{}
		var keys []string
		for _, p := range ps {
			by[p.Profile.Name], keys = p, append(keys, p.Profile.Name)
		}
		page, next, err := ssoPage(req, "oidc", keys)
		if err != nil {
			return err
		}
		resp := ci.ListInboundOidcSsoProfilesResponse{NextPageToken: next, InboundOidcSsoProfiles: []*ci.InboundOidcSsoProfile{}}
		for _, k := range page {
			resp.InboundOidcSsoProfiles = append(resp.InboundOidcSsoProfiles, oidcJSON(by[k]))
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/inboundOidcSsoProfiles/{id}", func(w http.ResponseWriter, req *http.Request) error {
		p, err := r.GetOIDCProfile("inboundOidcSsoProfiles/" + req.PathValue("id"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, oidcJSON(p))
	})
	rt.Handle("PATCH /v1/inboundOidcSsoProfiles/{id}", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.InboundOidcSsoProfile
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		p, err := r.UpdateOIDCProfile("inboundOidcSsoProfiles/"+req.PathValue("id"), in, ciMask(req.URL.Query().Get("updateMask")))
		if err != nil {
			return err
		}
		return doneOp(w, oidcProfileType, oidcJSON(p))
	})
	rt.Handle("DELETE /v1/inboundOidcSsoProfiles/{id}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteOIDCProfile("inboundOidcSsoProfiles/" + req.PathValue("id")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})

	// SSO assignments.
	rt.Handle("POST /v1/inboundSsoAssignments", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.InboundSsoAssignment
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		in.Name = ""
		a, err := r.CreateSSOAssignment(in)
		if err != nil {
			return err
		}
		return doneOp(w, ssoAssignmentType, a)
	})
	rt.Handle("GET /v1/inboundSsoAssignments", func(w http.ResponseWriter, req *http.Request) error {
		if err := ssoFilter(req.URL.Query().Get("filter")); err != nil {
			return err
		}
		as, err := r.ListSSOAssignments()
		if err != nil {
			return err
		}
		by := map[string]*ci.InboundSsoAssignment{}
		var keys []string
		// The keys keep the precedence order: a group's by rank, then
		// the org units'.
		for i, a := range as {
			k := fmt.Sprintf("%06d %s", i, a.Name)
			by[k], keys = a, append(keys, k)
		}
		page, next, err := ssoPage(req, "assignments", keys)
		if err != nil {
			return err
		}
		resp := ci.ListInboundSsoAssignmentsResponse{NextPageToken: next, InboundSsoAssignments: []*ci.InboundSsoAssignment{}}
		for _, k := range page {
			resp.InboundSsoAssignments = append(resp.InboundSsoAssignments, by[k])
		}
		return rest.WriteJSON(w, http.StatusOK, resp)
	})
	rt.Handle("GET /v1/inboundSsoAssignments/{id}", func(w http.ResponseWriter, req *http.Request) error {
		a, err := r.GetSSOAssignment("inboundSsoAssignments/" + req.PathValue("id"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, a)
	})
	rt.Handle("PATCH /v1/inboundSsoAssignments/{id}", func(w http.ResponseWriter, req *http.Request) error {
		var in ci.InboundSsoAssignment
		if err := rest.DecodeJSON(req, &in); err != nil {
			return err
		}
		a, err := r.UpdateSSOAssignment("inboundSsoAssignments/"+req.PathValue("id"), in, ciMask(req.URL.Query().Get("updateMask")))
		if err != nil {
			return err
		}
		return doneOp(w, ssoAssignmentType, a)
	})
	rt.Handle("DELETE /v1/inboundSsoAssignments/{id}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteSSOAssignment("inboundSsoAssignments/" + req.PathValue("id")); err != nil {
			return err
		}
		return doneOp(w, "", nil)
	})
}
