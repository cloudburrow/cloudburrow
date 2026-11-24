// Package azurefake is a local stand-in for the parts of Azure that
// internal/byoc/azureprov calls, for tests: a minimal Microsoft Entra ID
// authority (OpenID configuration, authorize and token endpoints, as MSAL
// reads them), the Azure Resource Manager operations used (resource groups,
// virtual networks, network security groups, public IPs, network
// interfaces, VMs and their instance view and power actions, role
// assignments, resources by tag, Permissions - List, subscriptions and
// locations), and the Retail Prices API. Request paths, api-versions and
// response shapes follow the Azure REST API reference; state is in memory.
//
// It serves TLS (httptest.NewTLSServer), since azcore refuses to send a
// bearer token over plain HTTP; Client trusts its certificate. Nothing here
// contacts Azure.
package azurefake

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov"
)

// The fake's fixed identities: one tenant, one user, two subscriptions.
const (
	TenantID      = "72f988bf-0000-4000-8000-00000000aaaa"
	UserOID       = "6a0c2d31-0000-4000-8000-00000000bbbb"
	Username      = "dev@contoso.example"
	Subscription  = "11111111-0000-4000-8000-000000000001"
	SubName       = "Pay-As-You-Go"
	Subscription2 = "22222222-0000-4000-8000-000000000002"
	SubName2      = "Startup credits"
	// StaticToken is an access token the fake always accepts (Credential).
	StaticToken = "fake-static-arm-token"
)

// Server is the fake.
type Server struct {
	srv *httptest.Server

	mu sync.Mutex
	// TokenLifetime is the expires_in the token endpoint issues; under five
	// minutes, MSAL refreshes on every silent call.
	TokenLifetime time.Duration
	// AsyncPolls is how many polls a VM create's Azure-AsyncOperation
	// answers InProgress before Succeeded.
	AsyncPolls int
	// Prices is the Retail Prices API's VM prices by armSkuName (USD/hour).
	Prices map[string]float64
	// PricesDown makes the Retail Prices API answer 503.
	PricesDown bool
	// DenyPermissionsList makes Permissions - List answer 403.
	DenyPermissionsList bool
	// ResizeNeedsDeallocate makes a running VM refuse a resize to these sizes.
	ResizeNeedsDeallocate map[string]bool

	codes    map[string]bool // authorization codes issued
	refresh  map[string]bool
	access   map[string]bool
	denied   map[string]bool // lower-cased actions
	failNext map[string]failure
	keep     map[string]bool // lower-cased resource types deletes leave in place
	res      map[string]*resource
	ops      map[string]int    // async operation id -> polls left
	opRes    map[string]string // async operation id -> the resource it creates
	ipSeq    int
	// Requests records "METHOD path" of every ARM request, in order.
	Requests []string
	// TokenGrants counts the token endpoint's grants by grant_type.
	TokenGrants map[string]int
}

type failure struct {
	status        int
	code, message string
}

type resource struct {
	id   string
	body map[string]any
}

// New starts the fake. Close it when done.
func New() *Server {
	s := &Server{
		TokenLifetime: time.Hour, AsyncPolls: 1,
		Prices: map[string]float64{
			"Standard_D4s_v5": 0.192, "Standard_D16s_v5": 0.768, "Standard_D32s_v5": 1.536, "Standard_NC4as_T4_v3": 0.526,
		},
		ResizeNeedsDeallocate: map[string]bool{},
		codes:                 map[string]bool{}, refresh: map[string]bool{}, access: map[string]bool{StaticToken: true},
		denied: map[string]bool{}, failNext: map[string]failure{}, keep: map[string]bool{},
		res: map[string]*resource{}, ops: map[string]int{}, opRes: map[string]string{}, TokenGrants: map[string]int{},
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s
}

// URL is the fake's base URL.
func (s *Server) URL() string { return s.srv.URL }

// Client is an HTTP client that trusts the fake's certificate.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Close stops the fake.
func (s *Server) Close() { s.srv.Close() }

// Certificate is the fake's TLS certificate, for a client in another
// process (test/byocfakes writes it out as PEM).
func (s *Server) Certificate() *x509.Certificate { return s.srv.Certificate() }

// Options are azureprov.Options pointing every endpoint at the fake, with a
// fast poll.
func (s *Server) Options() azureprov.Options {
	return azureprov.Options{
		AuthorityHost: s.srv.URL + "/", ResourceManager: s.srv.URL + "/", Audience: "https://management.fake.example/",
		PricesURL: s.srv.URL + "/api/retail/prices", HTTPClient: s.srv.Client(), DisableInstanceDiscovery: true,
		PollInterval: time.Millisecond, OpenBrowser: s.OpenBrowser,
	}
}

// Deny refuses action (e.g. Microsoft.Network/networkSecurityGroups/write)
// with AuthorizationFailed, and leaves it out of Permissions - List.
func (s *Server) Deny(action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied[strings.ToLower(action)] = true
}

// FailNext fails the next request for action with status and Azure's error
// code and message (e.g. 409 QuotaExceeded on
// Microsoft.Compute/virtualMachines/write).
func (s *Server) FailNext(action string, status int, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[strings.ToLower(action)] = failure{status, code, message}
}

// KeepOnDelete makes deletes leave resources of type (e.g.
// Microsoft.Network/publicIPAddresses) in place, as a delete that did not
// take, for Delete's check.
func (s *Server) KeepOnDelete(typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keep[strings.ToLower(typ)] = true
}

// Resource is a resource's JSON, or nil.
func (s *Server) Resource(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.res[strings.ToLower(id)]; r != nil {
		return r.body
	}
	return nil
}

// IDs is every resource ID the fake holds, sorted.
func (s *Server) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.res {
		out = append(out, r.id)
	}
	sort.Strings(out)
	return out
}

// SetPower sets a VM's power state code (running, deallocated, stopped).
func (s *Server) SetPower(vmID, power string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.res[strings.ToLower(vmID)]; r != nil {
		r.body["__power"] = power
	}
}

// Revoke invalidates every token the token endpoint issued, as a password
// change or an administrator's revocation does.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = map[string]bool{StaticToken: true}
	s.refresh = map[string]bool{}
}

// Credential is a token credential the fake accepts, for a provider opened
// without the sign-in.
func (s *Server) Credential() azcore.TokenCredential { return staticCred{} }

type staticCred struct{}

func (staticCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: StaticToken, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// OpenBrowser does what a browser does once the user has signed in on
// Microsoft's page: it posts the authorization code and state back to
// MSAL's localhost redirect (response_mode=form_post).
func (s *Server) OpenBrowser(authURL string) error {
	u, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	q := u.Query()
	redirect, state := q.Get("redirect_uri"), q.Get("state")
	if redirect == "" || state == "" {
		return fmt.Errorf("azurefake: authorize URL without redirect_uri or state: %s", authURL)
	}
	if !strings.HasPrefix(u.String(), s.srv.URL) {
		return fmt.Errorf("azurefake: the sign-in page %s is not the fake's", u.Host)
	}
	code := s.issueCode()
	go func() {
		resp, err := http.PostForm(redirect, url.Values{"code": {code}, "state": {state}})
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	return nil
}

func (s *Server) issueCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := "code-" + randHex(8)
	s.codes[c] = true
	return c
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func guid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func armError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("x-ms-error-code", code)
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.Contains(p, "/oauth2/v2.0/") || strings.HasSuffix(p, "/.well-known/openid-configuration"):
		s.serveAuthority(w, r)
	case p == "/api/retail/prices":
		s.servePrices(w, r)
	default:
		s.serveARM(w, r)
	}
}

// --- Microsoft Entra ID ---

func (s *Server) serveAuthority(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	tenant := parts[0]
	base := s.srv.URL + "/" + tenant
	switch {
	case strings.HasSuffix(r.URL.Path, "/v2.0/.well-known/openid-configuration"):
		writeJSON(w, 200, map[string]any{
			"authorization_endpoint":   base + "/oauth2/v2.0/authorize",
			"token_endpoint":           base + "/oauth2/v2.0/token",
			"issuer":                   s.srv.URL + "/" + TenantID + "/v2.0",
			"response_modes_supported": []string{"query", "fragment", "form_post"},
		})
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/authorize"):
		// What a browser gets once the user has signed in: a form that
		// posts the code back to the redirect URI.
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>Signed in</title><form method="post" action="%s"><input type="hidden" name="code" value="%s"><input type="hidden" name="state" value="%s"></form><script>document.forms[0].submit()</script>`,
			html.EscapeString(q.Get("redirect_uri")), html.EscapeString(s.issueCode()), html.EscapeString(q.Get("state")))
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		s.serveToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid_request", "error_description": err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant := r.PostFormValue("grant_type")
	s.TokenGrants[grant]++
	switch grant {
	case "authorization_code":
		code := r.PostFormValue("code")
		if !s.codes[code] {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "AADSTS70000: the authorization code is not valid"})
			return
		}
		delete(s.codes, code)
		if r.PostFormValue("code_verifier") == "" {
			writeJSON(w, 400, map[string]any{"error": "invalid_request", "error_description": "AADSTS50148: PKCE code_verifier missing"})
			return
		}
	case "refresh_token":
		rt := r.PostFormValue("refresh_token")
		if !s.refresh[rt] {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "AADSTS700082: the refresh token has expired"})
			return
		}
		delete(s.refresh, rt)
	default:
		writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	now := time.Now()
	at, rt := "fake-at-"+randHex(12), "fake-rt-"+randHex(12)
	s.access[at], s.refresh[rt] = true, true
	idt := b64(map[string]any{"alg": "none", "typ": "JWT"}) + "." + b64(map[string]any{
		"aud": azureprov.ClientID, "iss": s.srv.URL + "/" + TenantID + "/v2.0", "iat": now.Unix(), "nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(), "oid": UserOID, "sub": "sub-" + UserOID, "tid": TenantID,
		"preferred_username": Username, "name": "Dev User", "ver": "2.0",
	}) + ".sig"
	scope := r.PostFormValue("scope")
	writeJSON(w, 200, map[string]any{
		"token_type": "Bearer", "scope": scope, "expires_in": int(s.TokenLifetime.Seconds()),
		"ext_expires_in": int(s.TokenLifetime.Seconds()), "access_token": at, "refresh_token": rt, "id_token": idt,
		"client_info": base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"uid":"%s","utid":"%s"}`, UserOID, TenantID))),
	})
}

// --- Retail Prices API ---

var filterField = regexp.MustCompile(`(\w+) eq '([^']*)'`)

func (s *Server) servePrices(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PricesDown {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	f := map[string]string{}
	for _, m := range filterField.FindAllStringSubmatch(r.URL.Query().Get("$filter"), -1) {
		f[m[1]] = m[2]
	}
	region := f["armRegionName"]
	var items []map[string]any
	item := func(price float64, product, sku, meter, unit string) map[string]any {
		return map[string]any{"currencyCode": "USD", "tierMinimumUnits": 0, "retailPrice": price, "unitPrice": price,
			"armRegionName": region, "location": region, "effectiveStartDate": "2025-01-01T00:00:00Z",
			"meterName": meter, "productName": product, "skuName": sku, "serviceName": f["serviceName"],
			"armSkuName": f["armSkuName"], "type": "Consumption", "isPrimaryMeterRegion": true, "unitOfMeasure": unit}
	}
	switch f["serviceName"] {
	case "Virtual Machines":
		if price, ok := s.Prices[f["armSkuName"]]; ok {
			short := strings.ReplaceAll(strings.TrimPrefix(f["armSkuName"], "Standard_"), "_", " ")
			items = append(items,
				item(price*1.8, "Virtual Machines "+short+" Series Windows", short, short, "1 Hour"),
				item(price/5, "Virtual Machines "+short+" Series", short+" Spot", short+" Spot", "1 Hour"),
				item(price/4, "Virtual Machines "+short+" Series", short+" Low Priority", short+" Low Priority", "1 Hour"),
			)
			// The Linux pay-as-you-go meter comes on a second page.
			if r.URL.Query().Get("page") != "2" {
				writeJSON(w, 200, map[string]any{"BillingCurrency": "USD", "CustomerEntityId": "Default", "CustomerEntityType": "Retail",
					"Items": items, "NextPageLink": s.srv.URL + r.URL.Path + "?" + r.URL.RawQuery + "&page=2", "Count": len(items)})
				return
			}
			items = []map[string]any{item(price, "Virtual Machines "+short+" Series", short, short, "1 Hour")}
		}
	case "Storage":
		if f["skuName"] == "P10 LRS" {
			items = append(items, item(19.71, "Premium SSD Managed Disks", "P10 LRS", "P10 LRS Disk", "1/Month"))
		}
	}
	writeJSON(w, 200, map[string]any{"BillingCurrency": "USD", "CustomerEntityId": "Default", "CustomerEntityType": "Retail",
		"Items": items, "NextPageLink": nil, "Count": len(items)})
}

// --- Azure Resource Manager ---

var (
	reRGPath      = regexp.MustCompile(`(?i)^/subscriptions/([^/]+)/resourcegroups/([^/]+)$`)
	reProviderRes = regexp.MustCompile(`(?i)^/subscriptions/([^/]+)/resourcegroups/([^/]+)/providers/([^/]+)/([^/]+)/([^/]+)(?:/([^/]+))?$`)
	reRoleAssign  = regexp.MustCompile(`(?i)^(.*)/providers/Microsoft\.Authorization/roleAssignments/([^/]+)$`)
)

func (s *Server) serveARM(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimSuffix(r.URL.Path, "/")
	s.Requests = append(s.Requests, r.Method+" "+path)
	if strings.HasPrefix(path, "/fake/operations/") {
		s.serveOperation(w, strings.TrimPrefix(path, "/fake/operations/"))
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.access[tok] {
		armError(w, 401, "InvalidAuthenticationToken", "The access token is invalid.")
		return
	}
	if r.URL.Query().Get("api-version") == "" {
		armError(w, 400, "MissingApiVersionParameter", "The api-version query parameter (?api-version=) is required for all requests.")
		return
	}
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			if err := json.Unmarshal(b, &body); err != nil {
				armError(w, 400, "InvalidRequestContent", "The request content was invalid and could not be deserialized: "+err.Error())
				return
			}
		}
	}
	lp := strings.ToLower(path)
	switch {
	case lp == "/subscriptions" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"value": []any{s.subJSON(Subscription, SubName), s.subJSON(Subscription2, SubName2)}})
	case regexp.MustCompile(`^/subscriptions/[^/]+$`).MatchString(lp) && r.Method == http.MethodGet:
		sub := path[len("/subscriptions/"):]
		switch sub {
		case Subscription:
			writeJSON(w, 200, s.subJSON(Subscription, SubName))
		case Subscription2:
			writeJSON(w, 200, s.subJSON(Subscription2, SubName2))
		default:
			armError(w, 404, "SubscriptionNotFound", "The subscription '"+sub+"' could not be found.")
		}
	case strings.HasSuffix(lp, "/locations") && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"value": []any{
			location("eastus", "East US", "37.3719", "-79.8164", "Physical"),
			location("westeurope", "West Europe", "52.3667", "4.9", "Physical"),
			location("japaneast", "Japan East", "35.68", "139.77", "Physical"),
			location("eastusstg", "East US STG", "37.3719", "-79.8164", "Logical"),
		}})
	case strings.HasSuffix(lp, "/providers/microsoft.authorization/permissions") && r.Method == http.MethodGet:
		if s.DenyPermissionsList {
			armError(w, 403, "AuthorizationFailed", fmt.Sprintf("The client '%s' with object id '%s' does not have authorization to perform action 'Microsoft.Authorization/permissions/read' over scope '%s'.", Username, UserOID, path))
			return
		}
		var not []string
		for a := range s.denied {
			not = append(not, a)
		}
		sort.Strings(not)
		writeJSON(w, 200, map[string]any{"value": []any{map[string]any{"actions": []string{"*"}, "notActions": append([]string{"Microsoft.Authorization/elevateAccess/Action"}, not...), "dataActions": []string{}, "notDataActions": []string{}}}})
	case strings.HasSuffix(lp, "/resourcegroups") && r.Method == http.MethodGet:
		s.listGroups(w, r)
	case strings.HasSuffix(lp, "/resources") && r.Method == http.MethodGet:
		s.listResources(w, r, path)
	case strings.HasSuffix(lp, "/providers/microsoft.authorization/roleassignments") && r.Method == http.MethodGet:
		s.listRoleAssignments(w, path)
	case reRoleAssign.MatchString(path):
		s.serveRoleAssignment(w, r, path, body)
	case reRGPath.MatchString(path):
		s.serveGroup(w, r, path, body)
	case reProviderRes.MatchString(path):
		s.serveResource(w, r, path, body)
	default:
		armError(w, 404, "InvalidResourceType", "The resource type could not be found for "+r.Method+" "+path)
	}
}

func (s *Server) subJSON(id, name string) map[string]any {
	return map[string]any{"id": "/subscriptions/" + id, "subscriptionId": id, "tenantId": TenantID, "displayName": name,
		"state": "Enabled", "authorizationSource": "RoleBased",
		"subscriptionPolicies": map[string]any{"locationPlacementId": "Public_2014-09-01", "quotaId": "PayAsYouGo_2014-09-01", "spendingLimit": "Off"}}
}

func location(name, display, lat, long, typ string) map[string]any {
	return map[string]any{"id": "/subscriptions/" + Subscription + "/locations/" + name, "name": name, "type": "Region",
		"displayName": display, "regionalDisplayName": "(US) " + display,
		"metadata": map[string]any{"regionType": typ, "regionCategory": "Recommended", "latitude": lat, "longitude": long}}
}

// check refuses a denied action or fails an injected failure; it reports
// whether the request may go on.
func (s *Server) check(w http.ResponseWriter, action, scope string) bool {
	a := strings.ToLower(action)
	if f, ok := s.failNext[a]; ok {
		delete(s.failNext, a)
		armError(w, f.status, f.code, f.message)
		return false
	}
	for d := range s.denied {
		if d == a {
			armError(w, 403, "AuthorizationFailed", fmt.Sprintf("The client '%s' with object id '%s' does not have authorization to perform action '%s' over scope '%s' or the scope is invalid. If access was recently granted, please refresh your credentials.", Username, UserOID, action, scope))
			return false
		}
	}
	return true
}

func verb(method string) string {
	switch method {
	case http.MethodPut, http.MethodPatch:
		return "write"
	case http.MethodDelete:
		return "delete"
	}
	return "read"
}

var reTagFilter = regexp.MustCompile(`tagName eq '([^']*)' and tagValue eq '([^']*)'`)

func tagMatch(body map[string]any, filter string) bool {
	m := reTagFilter.FindStringSubmatch(filter)
	if m == nil {
		return filter == ""
	}
	tags, _ := body["tags"].(map[string]any)
	v, ok := tags[m[1]].(string)
	return ok && v == m[2]
}

func (s *Server) groupIDs() []string {
	var out []string
	for k, r := range s.res {
		if r.body["type"] == "Microsoft.Resources/resourceGroups" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	if !s.check(w, "Microsoft.Resources/subscriptions/resourceGroups/read", "/subscriptions/"+Subscription) {
		return
	}
	f := r.URL.Query().Get("$filter")
	out := []any{}
	for _, k := range s.groupIDs() {
		if tagMatch(s.res[k].body, f) {
			out = append(out, s.res[k].body)
		}
	}
	writeJSON(w, 200, map[string]any{"value": out})
}

func expanded(b map[string]any) map[string]any {
	out := map[string]any{"id": b["id"], "name": b["name"], "type": b["type"], "location": b["location"], "tags": b["tags"]}
	if props, ok := b["properties"].(map[string]any); ok {
		out["provisioningState"] = props["provisioningState"]
	}
	return out
}

func (s *Server) listResources(w http.ResponseWriter, r *http.Request, path string) {
	f := r.URL.Query().Get("$filter")
	prefix := ""
	if m := regexp.MustCompile(`(?i)^(/subscriptions/[^/]+/resourcegroups/[^/]+)/resources$`).FindStringSubmatch(path); m != nil {
		prefix = strings.ToLower(m[1]) + "/"
		if s.res[strings.ToLower(m[1])] == nil {
			armError(w, 404, "ResourceGroupNotFound", "Resource group '"+m[1]+"' could not be found.")
			return
		}
	}
	var keys []string
	for k := range s.res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []any{}
	for _, k := range keys {
		b := s.res[k].body
		if b["type"] == "Microsoft.Resources/resourceGroups" || strings.Contains(k, "/providers/microsoft.authorization/") {
			continue
		}
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		if !tagMatch(b, f) {
			continue
		}
		out = append(out, expanded(b))
	}
	writeJSON(w, 200, map[string]any{"value": out})
}

func (s *Server) serveGroup(w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
	m := reRGPath.FindStringSubmatch(path)
	id := "/subscriptions/" + m[1] + "/resourceGroups/" + m[2]
	k := strings.ToLower(id)
	if !s.check(w, "Microsoft.Resources/subscriptions/resourceGroups/"+verb(r.Method), id) {
		return
	}
	switch r.Method {
	case http.MethodPut:
		if body == nil || body["location"] == nil {
			armError(w, 400, "LocationRequired", "The location property is required for this definition.")
			return
		}
		status := 201
		if s.res[k] != nil {
			status = 200
		}
		g := map[string]any{"id": id, "name": m[2], "type": "Microsoft.Resources/resourceGroups", "location": body["location"],
			"tags": body["tags"], "properties": map[string]any{"provisioningState": "Succeeded"}}
		s.res[k] = &resource{id: id, body: g}
		writeJSON(w, status, g)
	case http.MethodGet:
		if g := s.res[k]; g != nil {
			writeJSON(w, 200, g.body)
			return
		}
		armError(w, 404, "ResourceGroupNotFound", "Resource group '"+m[2]+"' could not be found.")
	case http.MethodDelete:
		if s.res[k] == nil {
			armError(w, 404, "ResourceGroupNotFound", "Resource group '"+m[2]+"' could not be found.")
			return
		}
		for key, res := range s.res {
			if key == k || strings.HasPrefix(key, k+"/") {
				// Azure leaves role assignments at a deleted scope behind.
				if res.body["type"] == "Microsoft.Authorization/roleAssignments" {
					continue
				}
				if s.keep[strings.ToLower(fmt.Sprint(res.body["type"]))] {
					continue
				}
				delete(s.res, key)
			}
		}
		w.WriteHeader(200)
	default:
		armError(w, 405, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) nextIP() string {
	s.ipSeq++
	return fmt.Sprintf("203.0.113.%d", 10+s.ipSeq)
}

func (s *Server) serveResource(w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
	m := reProviderRes.FindStringSubmatch(path)
	sub, rg, ns, typ, name, action := m[1], m[2], m[3], m[4], m[5], m[6]
	fullType := ns + "/" + typ
	id := "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/" + fullType + "/" + name
	k := strings.ToLower(id)
	group := s.res[strings.ToLower("/subscriptions/"+sub+"/resourceGroups/"+rg)]
	if action != "" {
		s.serveVMAction(w, r, id, fullType, action)
		return
	}
	if !s.check(w, fullType+"/"+verb(r.Method), id) {
		return
	}
	switch r.Method {
	case http.MethodPut:
		if group == nil {
			armError(w, 404, "ResourceGroupNotFound", "Resource group '"+rg+"' could not be found.")
			return
		}
		if body == nil {
			body = map[string]any{}
		}
		status := 201
		if s.res[k] != nil {
			status = 200
		}
		body["id"], body["name"], body["type"] = id, name, fullType
		props, _ := body["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
			body["properties"] = props
		}
		props["provisioningState"] = "Succeeded"
		async := false
		switch strings.ToLower(fullType) {
		case "microsoft.network/publicipaddresses":
			if old := s.res[k]; old != nil {
				props["ipAddress"] = old.body["properties"].(map[string]any)["ipAddress"]
			} else {
				props["ipAddress"] = s.nextIP()
			}
		case "microsoft.network/virtualnetworks":
			if subnets, ok := props["subnets"].([]any); ok {
				for _, sn := range subnets {
					if snm, ok := sn.(map[string]any); ok {
						snm["id"] = id + "/subnets/" + fmt.Sprint(snm["name"])
					}
				}
			}
		case "microsoft.network/networkinterfaces":
			if cfgs, ok := props["ipConfigurations"].([]any); ok {
				for _, c := range cfgs {
					if cm, ok := c.(map[string]any); ok {
						cm["id"] = id + "/ipConfigurations/" + fmt.Sprint(cm["name"])
						cp, _ := cm["properties"].(map[string]any)
						if cp != nil {
							cp["privateIPAddress"] = "10.209.0.4"
						}
					}
				}
			}
		case "microsoft.compute/virtualmachines":
			if old := s.res[k]; old != nil {
				body["__power"] = old.body["__power"]
				body["identity"] = old.body["identity"]
			} else {
				body["__power"] = "running"
				if ident, ok := body["identity"].(map[string]any); ok && ident["type"] == "SystemAssigned" {
					ident["principalId"], ident["tenantId"] = guid(), TenantID
				}
			}
			if sp, ok := props["storageProfile"].(map[string]any); ok {
				if od, ok := sp["osDisk"].(map[string]any); ok {
					diskName := fmt.Sprint(od["name"])
					diskID := "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/Microsoft.Compute/disks/" + diskName
					md, _ := od["managedDisk"].(map[string]any)
					if md == nil {
						md = map[string]any{}
						od["managedDisk"] = md
					}
					md["id"] = diskID
					// Azure makes the OS disk from the VM and gives it no tag.
					s.res[strings.ToLower(diskID)] = &resource{id: diskID, body: map[string]any{"id": diskID, "name": diskName,
						"type": "Microsoft.Compute/disks", "location": body["location"],
						"properties": map[string]any{"provisioningState": "Succeeded", "diskSizeGB": od["diskSizeGB"]}}}
				}
			}
			if status == 201 && s.AsyncPolls > 0 {
				async = true
				props["provisioningState"] = "Creating"
			}
		}
		s.res[k] = &resource{id: id, body: body}
		if async {
			op := guid()
			s.ops[op] = s.AsyncPolls
			s.opRes[op] = k
			w.Header().Set("Azure-AsyncOperation", s.srv.URL+"/fake/operations/"+op)
			writeJSON(w, status, public(body))
			return
		}
		writeJSON(w, status, public(body))
	case http.MethodPatch:
		old := s.res[k]
		if old == nil {
			armError(w, 404, "ResourceNotFound", "The Resource '"+fullType+"/"+name+"' under resource group '"+rg+"' was not found.")
			return
		}
		if strings.EqualFold(fullType, "Microsoft.Compute/virtualMachines") {
			newSize := ""
			if props, ok := body["properties"].(map[string]any); ok {
				if hw, ok := props["hardwareProfile"].(map[string]any); ok {
					newSize = fmt.Sprint(hw["vmSize"])
				}
			}
			if newSize != "" && s.ResizeNeedsDeallocate[newSize] && old.body["__power"] == "running" {
				armError(w, 409, "OperationNotAllowed", "Unable to resize the VM '"+name+"' because the requested size "+newSize+" is not available in the current hardware cluster. Deallocate the VM and retry.")
				return
			}
			if newSize != "" {
				old.body["properties"].(map[string]any)["hardwareProfile"] = map[string]any{"vmSize": newSize}
			}
		}
		writeJSON(w, 200, public(old.body))
	case http.MethodGet:
		if res := s.res[k]; res != nil {
			writeJSON(w, 200, public(res.body))
			return
		}
		armError(w, 404, "ResourceNotFound", "The Resource '"+fullType+"/"+name+"' under resource group '"+rg+"' was not found.")
	case http.MethodDelete:
		res := s.res[k]
		if res == nil {
			w.WriteHeader(204)
			return
		}
		if !s.keep[strings.ToLower(fullType)] {
			delete(s.res, k)
		}
		w.WriteHeader(200)
	default:
		armError(w, 405, "MethodNotAllowed", r.Method)
	}
}

// public is a resource's JSON without the fake's own fields.
func public(b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range b {
		if !strings.HasPrefix(k, "__") {
			out[k] = v
		}
	}
	return out
}

func (s *Server) serveVMAction(w http.ResponseWriter, r *http.Request, id, fullType, action string) {
	k := strings.ToLower(id)
	vm := s.res[k]
	if !strings.EqualFold(fullType, "Microsoft.Compute/virtualMachines") {
		armError(w, 404, "InvalidResourceType", "no action "+action+" on "+fullType)
		return
	}
	a := strings.ToLower(action)
	if a == "instanceview" && r.Method == http.MethodGet {
		if !s.check(w, "Microsoft.Compute/virtualMachines/read", id) {
			return
		}
		if vm == nil {
			armError(w, 404, "ResourceNotFound", "The Resource 'Microsoft.Compute/virtualMachines' was not found.")
			return
		}
		prov := fmt.Sprint(vm.body["properties"].(map[string]any)["provisioningState"])
		statuses := []any{
			map[string]any{"code": "ProvisioningState/" + strings.ToLower(prov), "level": "Info", "displayStatus": "Provisioning " + strings.ToLower(prov)},
		}
		if p, _ := vm.body["__power"].(string); p != "" {
			statuses = append(statuses, map[string]any{"code": "PowerState/" + p, "level": "Info", "displayStatus": "VM " + p})
		}
		writeJSON(w, 200, map[string]any{"computerName": vm.body["name"], "osName": "ubuntu", "statuses": statuses})
		return
	}
	if r.Method != http.MethodPost {
		armError(w, 405, "MethodNotAllowed", r.Method)
		return
	}
	if !s.check(w, "Microsoft.Compute/virtualMachines/"+a+"/action", id) {
		return
	}
	if vm == nil {
		armError(w, 404, "ResourceNotFound", "The Resource 'Microsoft.Compute/virtualMachines' was not found.")
		return
	}
	switch a {
	case "start":
		vm.body["__power"] = "running"
	case "deallocate":
		vm.body["__power"] = "deallocated"
	case "poweroff":
		vm.body["__power"] = "stopped"
	case "restart":
		vm.body["__power"] = "running"
	default:
		armError(w, 400, "InvalidRequest", "unknown action "+action)
		return
	}
	w.WriteHeader(200)
}

func (s *Server) serveOperation(w http.ResponseWriter, op string) {
	left, ok := s.ops[op]
	if !ok {
		armError(w, 404, "NotFound", "operation not found")
		return
	}
	if left > 0 {
		s.ops[op] = left - 1
		writeJSON(w, 200, map[string]any{"name": op, "status": "InProgress", "startTime": time.Now().UTC().Format(time.RFC3339)})
		return
	}
	// Done: the VM's provisioning state becomes Succeeded.
	if res := s.res[s.opRes[op]]; res != nil {
		res.body["properties"].(map[string]any)["provisioningState"] = "Succeeded"
	}
	writeJSON(w, 200, map[string]any{"name": op, "status": "Succeeded", "startTime": time.Now().UTC().Format(time.RFC3339),
		"endTime": time.Now().UTC().Format(time.RFC3339)})
}

// --- role assignments ---

func (s *Server) serveRoleAssignment(w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
	m := reRoleAssign.FindStringSubmatch(path)
	scope, name := m[1], m[2]
	id := scope + "/providers/Microsoft.Authorization/roleAssignments/" + name
	k := strings.ToLower(id)
	if !s.check(w, "Microsoft.Authorization/roleAssignments/"+verb(r.Method), scope) {
		return
	}
	switch r.Method {
	case http.MethodPut:
		if s.res[strings.ToLower(scope)] == nil {
			armError(w, 404, "ResourceNotFound", "The scope '"+scope+"' does not exist.")
			return
		}
		props, _ := body["properties"].(map[string]any)
		if props == nil || props["principalId"] == nil || props["roleDefinitionId"] == nil {
			armError(w, 400, "InvalidRoleAssignmentRequest", "principalId and roleDefinitionId are required")
			return
		}
		props["scope"] = scope
		ra := map[string]any{"id": id, "name": name, "type": "Microsoft.Authorization/roleAssignments", "properties": props}
		s.res[k] = &resource{id: id, body: ra}
		writeJSON(w, 201, ra)
	case http.MethodGet:
		if res := s.res[k]; res != nil {
			writeJSON(w, 200, res.body)
			return
		}
		armError(w, 404, "RoleAssignmentNotFound", "The role assignment '"+name+"' is not found.")
	case http.MethodDelete:
		res := s.res[k]
		if res == nil {
			w.WriteHeader(204)
			return
		}
		delete(s.res, k)
		writeJSON(w, 200, res.body)
	}
}

func (s *Server) listRoleAssignments(w http.ResponseWriter, path string) {
	scope := strings.ToLower(strings.TrimSuffix(path[:len(path)-len("/providers/Microsoft.Authorization/roleAssignments")], "/"))
	if s.res[scope] == nil {
		armError(w, 404, "ResourceGroupNotFound", "Resource group could not be found.")
		return
	}
	var keys []string
	for k, r := range s.res {
		if r.body["type"] == "Microsoft.Authorization/roleAssignments" {
			sc := strings.ToLower(fmt.Sprint(r.body["properties"].(map[string]any)["scope"]))
			if sc == scope || strings.HasPrefix(sc, scope+"/") {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	out := []any{}
	for _, k := range keys {
		out = append(out, s.res[k].body)
	}
	writeJSON(w, 200, map[string]any{"value": out})
}

// RoleAssignments is every role assignment the fake holds.
func (s *Server) RoleAssignments() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, r := range s.res {
		if r.body["type"] == "Microsoft.Authorization/roleAssignments" {
			out = append(out, r.body)
		}
	}
	return out
}
