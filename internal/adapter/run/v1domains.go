package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Cloud Run v1 domain mappings (#1349): create, get, list and delete, on
// both path styles:
//
//	/apis/domains.cloudrun.com/v1/namespaces/{project}/domainmappings[/{domain}]
//	/v1/projects/{project}/locations/{location}/domainmappings[/{domain}]
//
// A mapping is stored, in a ConfigMap of its own, and reports the DNS
// records Google's "Mapping custom domains" page documents: a subdomain's
// CNAME to ghs.googlehosted.com., and an apex domain's four A and four AAAA
// records. The route (the service) must exist in the mapping's region;
// domain ownership is not checked, as there is no Search Console locally.
// Telling an apex domain from a subdomain by its number of labels (two) is
// a local rule: Google uses the public suffix list (UNVERIFIED for
// suffixes such as co.uk).
//
// What an instance can do with a mapping (#1450): it serves no public DNS
// and issues no certificate, but its Cloud Run gateway routes the domain.
// Each mapping is also a Knative DomainMapping (with the ClusterDomainClaim
// Knative requires for it) to the service's Knative Service, so a request
// to the gateway with the domain as its Host header reaches the service,
// over HTTP. The conditions say so, each with its reason:
//
//   - DomainRoutable is the Knative DomainMapping's Ready: True once the
//     gateway routes the domain, False (with Knative's reason) when it
//     cannot, for instance once the service is deleted;
//   - CertificateProvisioned is True for certificateMode NONE, where no
//     certificate is wanted, and False (CertificateNotIssued) for
//     AUTOMATIC, as no certificate authority is reached locally;
//   - Ready is True when both are, so a NONE mapping becomes Ready and an
//     AUTOMATIC one ends False rather than waiting for ever.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/run/docs/mapping-custom-domains

const (
	resKDomain      = "domainmappings.serving.knative.dev"
	resKDomainClaim = "clusterdomainclaims.networking.internal.knative.dev"
	kindRunDomain   = "run-domain"
	domainCMPrefix  = "run-domain-"
	domainDataKey   = "domainmapping.json"
	labelRunDomain  = "cloudburrow.dev/run-domain"
	ghsCNAME        = "ghs.googlehosted.com."
	domainNoCert    = "CloudBurrow issues no certificate: no certificate authority is reached locally, so the domain is served over HTTP alone, on this instance's Cloud Run gateway"
	domainNoneCert  = "certificateMode is NONE: no certificate is provisioned"
	domainRouted    = "routed by this instance's Cloud Run gateway: send a request with the domain as its Host header (CloudBurrow serves no public DNS, so the records above are not in place)"
)

// kdomain is the subset of a Knative DomainMapping a mapping's status is
// read from.
type kdomain struct {
	Metadata kmeta `json:"metadata"`
	Status   struct {
		Conditions []ksvcCondition `json:"conditions"`
	} `json:"status"`
}

// The apex records Google documents for a domain mapping.
var (
	apexA    = []string{"216.239.32.21", "216.239.34.21", "216.239.36.21", "216.239.38.21"}
	apexAAAA = []string{"2001:4860:4802:32::15", "2001:4860:4802:34::15", "2001:4860:4802:36::15", "2001:4860:4802:38::15"}
	domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// domainCM is the ConfigMap's name for a domain in a project: the project
// is part of it, as one cluster holds every project's mappings.
func domainCM(project, domain string) string {
	return domainCMPrefix + project + "." + domain
}

func (s *Server) v1DomainMapping(r *http.Request, sc v1Scope, domain string) (*runv1.DomainMapping, kconfigMap, error) {
	var cm kconfigMap
	what := fmt.Sprintf("domainmappings.domains.cloudrun.com %q", domain)
	if err := s.kn.getObject(r.Context(), &cm, resConfigMap, domainCM(sc.project, domain), what); err != nil {
		return nil, cm, err
	}
	dm, err := storedDomainMapping(cm)
	if err != nil {
		return nil, cm, err
	}
	if sc.location != "" && dm.Metadata.Labels[v1LocationLabel] != sc.location {
		return nil, cm, apierror.NotFound("%s not found", what)
	}
	var kd kdomain
	found := true
	if err := s.kn.getObject(r.Context(), &kd, resKDomain, domain, what); err != nil {
		if apierror.From(err).Code != codes.NotFound {
			return nil, cm, err
		}
		found = false
	}
	if found && kd.Metadata.Labels[labelRunDomain] != sc.project {
		found = false
	}
	domainConditions(dm, kd, found)
	return dm, cm, nil
}

// domainConditions sets a mapping's conditions from its Knative
// DomainMapping (found false when there is none), as the type comment
// describes.
func domainConditions(dm *runv1.DomainMapping, kd kdomain, found bool) {
	routable := &runv1.GoogleCloudRunV1Condition{Type: "DomainRoutable", Status: "Unknown", Reason: "RoutePending",
		Message: "waiting for this instance's Cloud Run gateway to route the domain"}
	if !found {
		routable.Status, routable.Reason = "False", "RouteNotFound"
		routable.Message = "the domain is not routed: the mapping has no Knative DomainMapping (it was made by an older CloudBurrow); delete it and map the domain again"
	}
	for _, c := range kd.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		routable.Status = c.Status
		switch c.Status {
		case "True":
			routable.Reason, routable.Message = "", domainRouted
		case "False":
			routable.Reason, routable.Message = c.Reason, "the domain is not routed: "+c.Message
		}
		if !c.LastTransitionTime.IsZero() {
			routable.LastTransitionTime = c.LastTransitionTime.UTC().Format(time.RFC3339)
		}
	}
	cert := &runv1.GoogleCloudRunV1Condition{Type: "CertificateProvisioned", Status: "True", Message: domainNoneCert}
	if dm.Spec.CertificateMode != "NONE" {
		cert = &runv1.GoogleCloudRunV1Condition{Type: "CertificateProvisioned", Status: "False", Reason: "CertificateNotIssued",
			Severity: "Error", Message: domainNoCert}
	}
	ready := &runv1.GoogleCloudRunV1Condition{Type: "Ready", Status: routable.Status, Reason: routable.Reason, Message: routable.Message,
		LastTransitionTime: routable.LastTransitionTime}
	if routable.Status == "True" {
		ready.Message = ""
	}
	if cert.Status != "True" && routable.Status != "False" {
		ready.Status, ready.Reason, ready.Message = "False", cert.Reason, cert.Message
	}
	dm.Status.Conditions = []*runv1.GoogleCloudRunV1Condition{ready, cert, routable}
}

func storedDomainMapping(cm kconfigMap) (*runv1.DomainMapping, error) {
	var dm runv1.DomainMapping
	if err := json.Unmarshal([]byte(cm.Data[domainDataKey]), &dm); err != nil {
		return nil, apierror.Internal(err, "decode domain mapping %s", cm.Metadata.Name)
	}
	dm.Metadata.Uid = cm.Metadata.UID
	dm.Metadata.ResourceVersion = cm.Metadata.ResourceVersion
	if !cm.Metadata.CreationTimestamp.IsZero() {
		dm.Metadata.CreationTimestamp = cm.Metadata.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	return &dm, nil
}

func (s *Server) v1CreateDomainMapping(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	var in runv1.DomainMapping
	if err := rest.DecodeJSON(r, &in); err != nil {
		v1Write(w, err, nil)
		return
	}
	if in.Metadata == nil || !domainRE.MatchString(in.Metadata.Name) || len(in.Metadata.Name) > 253 {
		v1Write(w, apierror.InvalidArgument("metadata.name must be a domain name, such as www.example.com"), nil)
		return
	}
	if ns := in.Metadata.Namespace; ns != "" && ns != sc.project {
		v1Write(w, apierror.InvalidArgument("metadata.namespace %q does not match the request's namespace %q", ns, sc.project), nil)
		return
	}
	if in.Spec == nil || in.Spec.RouteName == "" {
		v1Write(w, apierror.InvalidArgument("spec.routeName is required"), nil)
		return
	}
	switch in.Spec.CertificateMode {
	case "", "CERTIFICATE_MODE_UNSPECIFIED":
		// AUTOMATIC is the default, as gcloud and Terraform send it.
		in.Spec.CertificateMode = "AUTOMATIC"
	case "AUTOMATIC", "NONE":
	default:
		v1Write(w, apierror.InvalidArgument("spec.certificateMode %q is not one of AUTOMATIC or NONE", in.Spec.CertificateMode), nil)
		return
	}
	var gaps []string
	for k := range in.Metadata.Annotations {
		if strings.HasPrefix(k, "run.googleapis.com/") && k != v1ClientName && k != v1ClientVersion {
			gaps = append(gaps, "metadata.annotations["+k+"]")
		}
	}
	if len(gaps) > 0 {
		v1Write(w, apierror.Unimplemented("unsupported Cloud Run v1 configuration: %s: not mapped", strings.Join(gaps, ", ")), nil)
		return
	}
	loc, err := sc.writeLocation(&runv1.Service{Metadata: in.Metadata})
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	_, ks, err := s.v1Service(r.Context(), v1Scope{project: sc.project, location: loc}, in.Spec.RouteName)
	if err != nil {
		v1Write(w, apierror.InvalidArgument("spec.routeName: service %q not found in %s", in.Spec.RouteName, loc), nil)
		return
	}
	domain := in.Metadata.Name
	labels := map[string]string{}
	for k, v := range in.Metadata.Labels {
		labels[k] = v
	}
	labels[v1LocationLabel] = loc
	dm := &runv1.DomainMapping{ApiVersion: "domains.cloudrun.com/v1", Kind: "DomainMapping",
		Metadata: &runv1.ObjectMeta{Name: domain, Namespace: sc.project, Labels: labels, Annotations: in.Metadata.Annotations, Generation: 1,
			SelfLink: "/apis/domains.cloudrun.com/v1/namespaces/" + sc.project + "/domainmappings/" + domain},
		Spec: &runv1.DomainMappingSpec{RouteName: in.Spec.RouteName, CertificateMode: in.Spec.CertificateMode, ForceOverride: in.Spec.ForceOverride},
		Status: &runv1.DomainMappingStatus{ObservedGeneration: 1, MappedRouteName: in.Spec.RouteName,
			ResourceRecords: domainRecords(domain), Url: "https://" + domain}}
	if dm.Spec.CertificateMode == "NONE" {
		dm.Status.Url = "http://" + domain
	}

	if r.URL.Query().Get("dryRun") == "all" {
		domainConditions(dm, kdomain{}, true)
		v1Write(w, nil, dm)
		return
	}
	labels = map[string]string{labelOwned: "true", labelInstance: s.instance, labelKind: kindRunDomain, labelRunDomain: sc.project}
	if err := s.routeDomain(r.Context(), domain, ks.Metadata.Name, labels); err != nil {
		v1Write(w, err, nil)
		return
	}
	raw, err := json.Marshal(dm)
	if err != nil {
		v1Write(w, apierror.Internal(err, "encode domain mapping"), nil)
		return
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": domainCM(sc.project, domain), "namespace": s.kn.namespace(),
			"labels": labels,
		},
		"data": map[string]string{domainDataKey: string(raw)},
	})
	if err != nil {
		v1Write(w, apierror.Internal(err, "encode domain mapping"), nil)
		return
	}
	if err := s.kn.writeObject(r.Context(), "create", string(manifest), "domain mapping "+domain); err != nil {
		_ = s.unrouteDomain(r.Context(), sc.project, domain)
		v1Write(w, err, nil)
		return
	}
	got, _, err := s.v1DomainMapping(r, v1Scope{project: sc.project}, domain)
	v1Write(w, err, got)
}

// routeDomain has the gateway route a domain to a Knative Service: a
// ClusterDomainClaim for the domain, delegated to the instance's namespace
// (Knative maps only claimed domains), and a Knative DomainMapping. A domain
// another project has mapped is ALREADY_EXISTS, as a domain is mapped once.
func (s *Server) routeDomain(ctx context.Context, domain, ksvcName string, labels map[string]string) error {
	ns := s.kn.namespace()
	claim, err := json.Marshal(map[string]any{
		"apiVersion": "networking.internal.knative.dev/v1alpha1", "kind": "ClusterDomainClaim",
		"metadata": map[string]any{"name": domain, "labels": labels},
		"spec":     map[string]any{"namespace": ns},
	})
	if err != nil {
		return apierror.Internal(err, "encode domain claim")
	}
	mapping, err := json.Marshal(map[string]any{
		"apiVersion": "serving.knative.dev/v1beta1", "kind": "DomainMapping",
		"metadata": map[string]any{"name": domain, "namespace": ns, "labels": labels},
		"spec": map[string]any{"ref": map[string]any{"apiVersion": "serving.knative.dev/v1", "kind": "Service",
			"name": ksvcName, "namespace": ns}},
	})
	if err != nil {
		return apierror.Internal(err, "encode domain mapping")
	}
	var existing kdomain
	if err := s.kn.getObject(ctx, &existing, resKDomain, domain, "domain mapping "+domain); err == nil {
		return apierror.AlreadyExists("domain mapping %s already exists", domain)
	} else if apierror.From(err).Code != codes.NotFound {
		return err
	}
	if err := s.kn.writeObject(ctx, "create", string(claim), "domain claim "+domain); err != nil &&
		apierror.From(err).Code != codes.AlreadyExists {
		return err
	}
	if err := s.kn.writeObject(ctx, "create", string(mapping), "domain mapping "+domain); err != nil {
		if apierror.From(err).Code != codes.AlreadyExists {
			_ = s.kn.deleteObject(ctx, resKDomainClaim, domain)
		}
		return err
	}
	return nil
}

// unrouteDomain removes what routeDomain made for a project's mapping; a
// route another project's mapping holds is left alone.
func (s *Server) unrouteDomain(ctx context.Context, project, domain string) error {
	var kd kdomain
	if err := s.kn.getObject(ctx, &kd, resKDomain, domain, "domain mapping "+domain); err != nil {
		if apierror.From(err).Code == codes.NotFound {
			return nil
		}
		return err
	}
	if kd.Metadata.Labels[labelRunDomain] != project {
		return nil
	}
	if err := s.kn.deleteObject(ctx, resKDomain, domain); err != nil {
		return err
	}
	return s.kn.deleteObject(ctx, resKDomainClaim, domain)
}

// domainRecords are the DNS records Google documents for a domain.
func domainRecords(domain string) []*runv1.ResourceRecord {
	labels := strings.Split(domain, ".")
	if len(labels) > 2 {
		return []*runv1.ResourceRecord{{Name: strings.Join(labels[:len(labels)-2], "."), Type: "CNAME", Rrdata: ghsCNAME}}
	}
	var out []*runv1.ResourceRecord
	for _, a := range apexA {
		out = append(out, &runv1.ResourceRecord{Type: "A", Rrdata: a})
	}
	for _, a := range apexAAAA {
		out = append(out, &runv1.ResourceRecord{Type: "AAAA", Rrdata: a})
	}
	return out
}

func (s *Server) v1GetDomainMapping(w http.ResponseWriter, r *http.Request) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	dm, _, err := s.v1DomainMapping(r, scopeOf(r), id)
	v1Write(w, err, dm)
}

func (s *Server) v1ListDomainMappings(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	sel, err := labelSelector(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	cms, err := listObjects[kconfigMap](r.Context(), s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunDomain+","+labelRunDomain+"="+sc.project)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	kds, err := listObjects[kdomain](r.Context(), s.kn, resKDomain, labelOwned+"=true,"+labelKind+"="+kindRunDomain+","+labelRunDomain+"="+sc.project)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	routes := map[string]kdomain{}
	for _, kd := range kds {
		routes[kd.Metadata.Name] = kd
	}
	byName := map[string]*runv1.DomainMapping{}
	var names []string
	for _, cm := range cms {
		dm, err := storedDomainMapping(cm)
		if err != nil {
			v1Write(w, err, nil)
			return
		}
		kd, found := routes[dm.Metadata.Name]
		domainConditions(dm, kd, found)
		if (sc.location != "" && dm.Metadata.Labels[v1LocationLabel] != sc.location) || !selected(dm.Metadata, sel) {
			continue
		}
		byName[dm.Metadata.Name] = dm
		names = append(names, dm.Metadata.Name)
	}
	sort.Strings(names)
	page, next, err := v1Page(r, "run-v1-domainmappings:"+sc.project+"/"+sc.location, names)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	out := &runv1.ListDomainMappingsResponse{ApiVersion: "domains.cloudrun.com/v1", Kind: "DomainMappingList",
		Metadata: &runv1.ListMeta{Continue: next}, Items: []*runv1.DomainMapping{}}
	for _, n := range page {
		out.Items = append(out.Items, byName[n])
	}
	v1Write(w, nil, out)
}

func (s *Server) v1DeleteDomainMapping(w http.ResponseWriter, r *http.Request) {
	id, err := resourceName(r)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	sc := scopeOf(r)
	dm, _, err := s.v1DomainMapping(r, sc, id)
	if err != nil {
		v1Write(w, err, nil)
		return
	}
	if r.URL.Query().Get("dryRun") != "all" {
		if err := s.unrouteDomain(r.Context(), sc.project, id); err != nil {
			v1Write(w, err, nil)
			return
		}
		if err := s.kn.deleteObject(r.Context(), resConfigMap, domainCM(sc.project, id)); err != nil {
			v1Write(w, err, nil)
			return
		}
	}
	v1Write(w, nil, &runv1.Status{Status: "Success",
		Details: &runv1.StatusDetails{Name: id, Kind: "domainmappings", Group: "domains.cloudrun.com", Uid: dm.Metadata.Uid}})
}

// v1AuthorizedDomains is projects.authorizeddomains.list and
// namespaces.authorizeddomains.list: the domains the caller has verified
// in Search Console. There is no Search Console locally, so none is, and
// the list is empty; gcloud run domain-mappings create, which checks it
// first, therefore refuses every domain with its own "not verified"
// message, while the API itself (which does not check) maps any.
func (s *Server) v1AuthorizedDomains(w http.ResponseWriter, r *http.Request) {
	v1Write(w, nil, &runv1.ListAuthorizedDomainsResponse{Domains: []*runv1.AuthorizedDomain{}})
}
