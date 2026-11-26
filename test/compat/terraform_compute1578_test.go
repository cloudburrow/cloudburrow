//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	certificatemanager "cloud.google.com/go/certificatemanager/apiv1"
	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	dataproc "cloud.google.com/go/dataproc/v2/apiv1"
	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iap "cloud.google.com/go/iap/apiv1"
	"cloud.google.com/go/iap/apiv1/iappb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	networkservices "cloud.google.com/go/networkservices/apiv1"
	"cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"cloud.google.com/go/vpcaccess/apiv1/vpcaccesspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The APIs the Compute Engine port serves beside Compute Engine (#2153,
// #2160) through `cloudburrow terraform` (#1578, round 3): before it the
// wrapper pointed certificate_manager_, network_security_,
// network_services_, iap_ and vpc_access_custom_endpoint at the local
// refusal though the port served them. Each test applies a module with
// hashicorp/google (the pin, hash-locked) with egress blocked, reads what
// it made back with the official Go client, plans clean, updates in place
// where the provider can, and destroys.

// tfNotFound fails unless err is NOT_FOUND, over REST (404) or gRPC.
func tfNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if computeCode(err) != 404 && status.Code(err) != codes.NotFound {
		t.Errorf("%s after destroy = %v; want NOT_FOUND", what, err)
	}
}

// TestTerraformCertificateManager (#1578): a DNS authorization, a
// self-managed certificate and a Google-managed one it authorizes, a
// certificate map with a hostname entry, a trust config, and a
// certificate issuance config of a CA Service pool the module makes. The
// official certificatemanager/apiv1 REST client reads each; the plan is
// clean; descriptions and labels change in place; destroy removes them.
// covers: google.cloud.certificatemanager.v1.CertificateManager/CreateTrustConfig, google.cloud.certificatemanager.v1.CertificateManager/GetTrustConfig, google.cloud.certificatemanager.v1.CertificateManager/UpdateTrustConfig, google.cloud.certificatemanager.v1.CertificateManager/DeleteTrustConfig, google.cloud.certificatemanager.v1.CertificateManager/CreateCertificateIssuanceConfig, google.cloud.certificatemanager.v1.CertificateManager/GetCertificateIssuanceConfig, google.cloud.certificatemanager.v1.CertificateManager/DeleteCertificateIssuanceConfig
func TestTerraformCertificateManager(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvPrivateCA) // the issuance config's pool; skips without it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	c, err := certificatemanager.NewRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	m := newTFModule(t)
	id := tfID("tfcm-", 20)
	domain := id + ".example.com"
	crt, key := compatSelfSigned(t, "www."+domain)
	ca, _ := compatSelfSigned(t, "anchor."+domain)
	module := func(desc, env string) string {
		return fmt.Sprintf(`
resource "google_certificate_manager_dns_authorization" "d" {
  name        = "%[1]s-dns"
  domain      = %[2]q
  description = %[3]q
  labels      = { env = %[4]q }
}

resource "google_certificate_manager_certificate" "self" {
  name        = "%[1]s-self"
  description = %[3]q
  labels      = { env = %[4]q }
  self_managed {
    pem_certificate = %[5]q
    pem_private_key = %[6]q
  }
}

resource "google_certificate_manager_certificate" "managed" {
  name        = "%[1]s-managed"
  description = %[3]q
  labels      = { env = %[4]q }
  managed {
    domains            = ["*.%[2]s"]
    dns_authorizations = [google_certificate_manager_dns_authorization.d.id]
  }
}

resource "google_certificate_manager_certificate_map" "m" {
  name        = "%[1]s-map"
  description = %[3]q
  labels      = { env = %[4]q }
}

resource "google_certificate_manager_certificate_map_entry" "e" {
  name         = "%[1]s-entry"
  description  = %[3]q
  labels       = { env = %[4]q }
  map          = google_certificate_manager_certificate_map.m.name
  certificates = [google_certificate_manager_certificate.self.id]
  hostname     = "www.%[2]s"
}

resource "google_certificate_manager_trust_config" "t" {
  name        = "%[1]s-trust"
  location    = "global"
  description = %[3]q
  labels      = { env = %[4]q }
  trust_stores {
    trust_anchors {
      pem_certificate = %[7]q
    }
  }
}

resource "google_privateca_ca_pool" "p" {
  name     = "%[1]s-pool"
  location = "us-central1"
  tier     = "DEVOPS"
}

resource "google_certificate_manager_certificate_issuance_config" "i" {
  name                       = "%[1]s-issue"
  location                   = "global"
  description                = "issued by Terraform"
  lifetime                   = "1814400s"
  rotation_window_percentage = 34
  key_algorithm              = "ECDSA_P256"
  certificate_authority_config {
    certificate_authority_service_config {
      ca_pool = google_privateca_ca_pool.p.id
    }
  }
}
`, id, domain, desc, env, crt, key, ca)
	}
	g := "projects/" + m.project + "/locations/global/"
	read := func(desc, env string) {
		t.Helper()
		d, err := c.GetDnsAuthorization(ctx, &certificatemanagerpb.GetDnsAuthorizationRequest{Name: g + "dnsAuthorizations/" + id + "-dns"})
		if err != nil || d.GetDomain() != domain || d.GetDescription() != desc || d.GetLabels()["env"] != env {
			t.Errorf("GetDnsAuthorization = %v, %v; want %q %q", d, err, desc, env)
		}
		for _, n := range []string{"-self", "-managed"} {
			cert, err := c.GetCertificate(ctx, &certificatemanagerpb.GetCertificateRequest{Name: g + "certificates/" + id + n})
			if err != nil || cert.GetDescription() != desc || cert.GetLabels()["env"] != env {
				t.Errorf("GetCertificate %s = %v, %v; want %q %q", n, cert, err, desc, env)
			}
		}
		mp, err := c.GetCertificateMap(ctx, &certificatemanagerpb.GetCertificateMapRequest{Name: g + "certificateMaps/" + id + "-map"})
		if err != nil || mp.GetDescription() != desc || mp.GetLabels()["env"] != env {
			t.Errorf("GetCertificateMap = %v, %v", mp, err)
		}
		e, err := c.GetCertificateMapEntry(ctx, &certificatemanagerpb.GetCertificateMapEntryRequest{Name: g + "certificateMaps/" + id + "-map/certificateMapEntries/" + id + "-entry"})
		if err != nil || e.GetHostname() != "www."+domain || len(e.GetCertificates()) != 1 || e.GetDescription() != desc || e.GetLabels()["env"] != env {
			t.Errorf("GetCertificateMapEntry = %v, %v", e, err)
		}
		tc, err := c.GetTrustConfig(ctx, &certificatemanagerpb.GetTrustConfigRequest{Name: g + "trustConfigs/" + id + "-trust"})
		if err != nil || len(tc.GetTrustStores()) != 1 || len(tc.GetTrustStores()[0].GetTrustAnchors()) != 1 || tc.GetDescription() != desc || tc.GetLabels()["env"] != env {
			t.Errorf("GetTrustConfig = %v, %v", tc, err)
		}
		ic, err := c.GetCertificateIssuanceConfig(ctx, &certificatemanagerpb.GetCertificateIssuanceConfigRequest{Name: g + "certificateIssuanceConfigs/" + id + "-issue"})
		if err != nil || ic.GetRotationWindowPercentage() != 34 || ic.GetKeyAlgorithm() != certificatemanagerpb.CertificateIssuanceConfig_ECDSA_P256 ||
			ic.GetLifetime().AsDuration() != 21*24*time.Hour || ic.GetCertificateAuthorityConfig().GetCertificateAuthorityServiceConfig().GetCaPool() == "" {
			t.Errorf("GetCertificateIssuanceConfig = %v, %v", ic, err)
		}
	}
	m.write(module("made by Terraform", "dev"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	read("made by Terraform", "dev")
	m.planClean()
	m.write(module("changed by Terraform", "prod"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !tfInPlaceOnly(out) {
		t.Errorf("the description and label change was not in place:\n%s", lastLines(out, 40))
	}
	read("changed by Terraform", "prod")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = c.GetDnsAuthorization(ctx, &certificatemanagerpb.GetDnsAuthorizationRequest{Name: g + "dnsAuthorizations/" + id + "-dns"})
	tfNotFound(t, "GetDnsAuthorization", err)
	_, err = c.GetCertificate(ctx, &certificatemanagerpb.GetCertificateRequest{Name: g + "certificates/" + id + "-self"})
	tfNotFound(t, "GetCertificate", err)
	_, err = c.GetCertificateMap(ctx, &certificatemanagerpb.GetCertificateMapRequest{Name: g + "certificateMaps/" + id + "-map"})
	tfNotFound(t, "GetCertificateMap", err)
	_, err = c.GetTrustConfig(ctx, &certificatemanagerpb.GetTrustConfigRequest{Name: g + "trustConfigs/" + id + "-trust"})
	tfNotFound(t, "GetTrustConfig", err)
	_, err = c.GetCertificateIssuanceConfig(ctx, &certificatemanagerpb.GetCertificateIssuanceConfigRequest{Name: g + "certificateIssuanceConfigs/" + id + "-issue"})
	tfNotFound(t, "GetCertificateIssuanceConfig", err)
}

var tfApplySummary = regexp.MustCompile(`Apply complete! Resources: (\d+) added, (\d+) changed, (\d+) destroyed`)

// tfInPlaceOnly reports whether an apply's summary changed something and
// added and destroyed nothing: an update in place, not a replacement.
func tfInPlaceOnly(out string) bool {
	s := tfApplySummary.FindStringSubmatch(out)
	return s != nil && s[1] == "0" && s[2] != "0" && s[3] == "0"
}

// TestTerraformNetworkSecurity (#1578): Network Security's policies
// through network_security_custom_endpoint: a Traffic Director and an
// Application Load Balancer server TLS policy, a client TLS policy, an
// address group, a backend authentication
// config, and Secure Web Proxy's URL list, TLS inspection policy, gateway
// security policy and rule. The official networksecurity/apiv1 REST
// clients read each; the plan is clean; descriptions, the group's items,
// the list's values and the rule's priority change in place; destroy
// removes them.
// covers: google.cloud.networksecurity.v1.NetworkSecurity/CreateServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/UpdateServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteServerTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/UpdateClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteClientTlsPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateBackendAuthenticationConfig, google.cloud.networksecurity.v1.NetworkSecurity/GetBackendAuthenticationConfig, google.cloud.networksecurity.v1.NetworkSecurity/UpdateBackendAuthenticationConfig, google.cloud.networksecurity.v1.NetworkSecurity/DeleteBackendAuthenticationConfig, google.cloud.networksecurity.v1.AddressGroupService/CreateAddressGroup, google.cloud.networksecurity.v1.AddressGroupService/GetAddressGroup, google.cloud.networksecurity.v1.AddressGroupService/UpdateAddressGroup, google.cloud.networksecurity.v1.AddressGroupService/DeleteAddressGroup, google.cloud.networksecurity.v1.NetworkSecurity/CreateUrlList, google.cloud.networksecurity.v1.NetworkSecurity/GetUrlList, google.cloud.networksecurity.v1.NetworkSecurity/UpdateUrlList, google.cloud.networksecurity.v1.NetworkSecurity/DeleteUrlList, google.cloud.networksecurity.v1.NetworkSecurity/CreateTlsInspectionPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetTlsInspectionPolicy, google.cloud.networksecurity.v1.NetworkSecurity/UpdateTlsInspectionPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteTlsInspectionPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/UpdateGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/GetGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/UpdateGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/DeleteGatewaySecurityPolicyRule
func TestTerraformNetworkSecurity(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvPrivateCA) // the TLS inspection policy's pool; skips without it
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	ag, err := networksecurity.NewAddressGroupRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ag.Close() })
	m := newTFModule(t)
	id := tfID("tfns-", 20)
	module := func(desc string, item string, value string, prio int) string {
		return fmt.Sprintf(`
resource "google_network_security_server_tls_policy" "td" {
  name        = "%[1]s-td"
  location    = "global"
  description = %[2]q
  labels      = { team = "burrow" }
  allow_open  = false
  server_certificate {
    certificate_provider_instance {
      plugin_instance = "google_cloud_private_spiffe"
    }
  }
  mtls_policy {
    client_validation_ca {
      certificate_provider_instance {
        plugin_instance = "google_cloud_private_spiffe"
      }
    }
  }
}

resource "google_network_security_server_tls_policy" "alb" {
  name        = "%[1]s-alb"
  location    = "global"
  description = %[2]q
  mtls_policy {
    client_validation_mode = "ALLOW_INVALID_OR_MISSING_CLIENT_CERT"
  }
}

resource "google_network_security_client_tls_policy" "c" {
  name        = "%[1]s-client"
  location    = "global"
  description = %[2]q
  sni         = "backend.example.com"
  client_certificate {
    certificate_provider_instance {
      plugin_instance = "google_cloud_private_spiffe"
    }
  }
  server_validation_ca {
    certificate_provider_instance {
      plugin_instance = "google_cloud_private_spiffe"
    }
  }
}

resource "google_network_security_address_group" "g" {
  name        = "%[1]s-group"
  parent      = "projects/%[6]s"
  location    = "us-central1"
  description = %[2]q
  type        = "IPV4"
  capacity    = 100
  items       = ["208.80.154.224/32", %[3]q]
}

resource "google_network_security_backend_authentication_config" "b" {
  name             = "%[1]s-backend"
  location         = "global"
  description      = %[2]q
  well_known_roots = "PUBLIC_ROOTS"
}

resource "google_network_security_url_lists" "u" {
  name        = "%[1]s-list"
  location    = "us-central1"
  description = %[2]q
  values      = ["www.example.com", %[4]q]
}

resource "google_privateca_ca_pool" "p" {
  name     = "%[1]s-pool"
  location = "us-central1"
  tier     = "DEVOPS"
}

resource "google_network_security_tls_inspection_policy" "i" {
  name                  = "%[1]s-tls"
  location              = "us-central1"
  description           = %[2]q
  ca_pool               = google_privateca_ca_pool.p.id
  exclude_public_ca_set = false
}

resource "google_network_security_gateway_security_policy" "s" {
  name                  = "%[1]s-policy"
  location              = "us-central1"
  description           = %[2]q
  tls_inspection_policy = google_network_security_tls_inspection_policy.i.id
}

resource "google_network_security_gateway_security_policy_rule" "r" {
  name                    = "%[1]s-rule"
  location                = "us-central1"
  gateway_security_policy = google_network_security_gateway_security_policy.s.name
  description             = %[2]q
  enabled                 = true
  priority                = %[5]d
  session_matcher         = "host() == 'www.example.com'"
  basic_profile           = "ALLOW"
}
`, id, desc, item, value, prio, m.project)
	}
	gl := "projects/" + m.project + "/locations/global/"
	uc := "projects/" + m.project + "/locations/us-central1/"
	read := func(desc, item, value string, prio int32) {
		t.Helper()
		for _, n := range []string{"-td", "-alb"} {
			p, err := ns.GetServerTlsPolicy(ctx, &networksecuritypb.GetServerTlsPolicyRequest{Name: gl + "serverTlsPolicies/" + id + n})
			if err != nil || p.GetDescription() != desc || p.GetMtlsPolicy() == nil {
				t.Errorf("GetServerTlsPolicy %s = %v, %v", n, p, err)
			}
		}
		cp, err := ns.GetClientTlsPolicy(ctx, &networksecuritypb.GetClientTlsPolicyRequest{Name: gl + "clientTlsPolicies/" + id + "-client"})
		if err != nil || cp.GetSni() != "backend.example.com" || cp.GetDescription() != desc || len(cp.GetServerValidationCa()) != 1 {
			t.Errorf("GetClientTlsPolicy = %v, %v", cp, err)
		}
		g, err := ag.GetAddressGroup(ctx, &networksecuritypb.GetAddressGroupRequest{Name: uc + "addressGroups/" + id + "-group"})
		if err != nil || g.GetCapacity() != 100 || len(g.GetItems()) != 2 || !slices.Contains(g.GetItems(), item) || g.GetDescription() != desc {
			t.Errorf("GetAddressGroup = %v, %v; want %s", g, err, item)
		}
		b, err := ns.GetBackendAuthenticationConfig(ctx, &networksecuritypb.GetBackendAuthenticationConfigRequest{Name: gl + "backendAuthenticationConfigs/" + id + "-backend"})
		if err != nil || b.GetWellKnownRoots() != networksecuritypb.BackendAuthenticationConfig_PUBLIC_ROOTS || b.GetDescription() != desc {
			t.Errorf("GetBackendAuthenticationConfig = %v, %v", b, err)
		}
		u, err := ns.GetUrlList(ctx, &networksecuritypb.GetUrlListRequest{Name: uc + "urlLists/" + id + "-list"})
		if err != nil || len(u.GetValues()) != 2 || !slices.Contains(u.GetValues(), value) || u.GetDescription() != desc {
			t.Errorf("GetUrlList = %v, %v; want %s", u, err, value)
		}
		ti, err := ns.GetTlsInspectionPolicy(ctx, &networksecuritypb.GetTlsInspectionPolicyRequest{Name: uc + "tlsInspectionPolicies/" + id + "-tls"})
		if err != nil || !strings.HasSuffix(ti.GetCaPool(), "/caPools/"+id+"-pool") || ti.GetDescription() != desc {
			t.Errorf("GetTlsInspectionPolicy = %v, %v", ti, err)
		}
		sp, err := ns.GetGatewaySecurityPolicy(ctx, &networksecuritypb.GetGatewaySecurityPolicyRequest{Name: uc + "gatewaySecurityPolicies/" + id + "-policy"})
		if err != nil || sp.GetTlsInspectionPolicy() != ti.GetName() || sp.GetDescription() != desc {
			t.Errorf("GetGatewaySecurityPolicy = %v, %v", sp, err)
		}
		r, err := ns.GetGatewaySecurityPolicyRule(ctx, &networksecuritypb.GetGatewaySecurityPolicyRuleRequest{Name: uc + "gatewaySecurityPolicies/" + id + "-policy/rules/" + id + "-rule"})
		if err != nil || r.GetPriority() != prio || !r.GetEnabled() || r.GetBasicProfile() != networksecuritypb.GatewaySecurityPolicyRule_ALLOW || r.GetDescription() != desc {
			t.Errorf("GetGatewaySecurityPolicyRule = %v, %v; want priority %d", r, err, prio)
		}
	}
	m.write(module("made by Terraform", "10.1.0.0/16", "*.example.org", 10))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	read("made by Terraform", "10.1.0.0/16", "*.example.org", 10)
	m.planClean()
	m.write(module("changed by Terraform", "10.2.0.0/16", "*.example.net", 20))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !tfInPlaceOnly(out) {
		t.Errorf("the update was not in place:\n%s", lastLines(out, 60))
	}
	read("changed by Terraform", "10.2.0.0/16", "*.example.net", 20)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = ns.GetServerTlsPolicy(ctx, &networksecuritypb.GetServerTlsPolicyRequest{Name: gl + "serverTlsPolicies/" + id + "-td"})
	tfNotFound(t, "GetServerTlsPolicy", err)
	_, err = ag.GetAddressGroup(ctx, &networksecuritypb.GetAddressGroupRequest{Name: uc + "addressGroups/" + id + "-group"})
	tfNotFound(t, "GetAddressGroup", err)
	_, err = ns.GetGatewaySecurityPolicy(ctx, &networksecuritypb.GetGatewaySecurityPolicyRequest{Name: uc + "gatewaySecurityPolicies/" + id + "-policy"})
	tfNotFound(t, "GetGatewaySecurityPolicy", err)
	_, err = ns.GetUrlList(ctx, &networksecuritypb.GetUrlListRequest{Name: uc + "urlLists/" + id + "-list"})
	tfNotFound(t, "GetUrlList", err)
}

// TestTerraformNetworkServices (#1578): Network Services through
// network_services_custom_endpoint: an OPEN_MESH gateway, an endpoint
// policy with a server TLS policy, a Secure Web Proxy gateway (network,
// subnetwork, proxy-only subnetwork and gateway security policy all made
// by the module) and an authorization extension of a regional backend
// service. The official networkservices/apiv1 REST clients read each;
// the plan is clean; descriptions and labels change in place; destroy
// removes them.
// covers: google.cloud.networkservices.v1.NetworkServices/CreateGateway, google.cloud.networkservices.v1.NetworkServices/GetGateway, google.cloud.networkservices.v1.NetworkServices/UpdateGateway, google.cloud.networkservices.v1.NetworkServices/DeleteGateway, google.cloud.networkservices.v1.NetworkServices/CreateEndpointPolicy, google.cloud.networkservices.v1.NetworkServices/GetEndpointPolicy, google.cloud.networkservices.v1.NetworkServices/UpdateEndpointPolicy, google.cloud.networkservices.v1.NetworkServices/DeleteEndpointPolicy, google.cloud.networkservices.v1.DepService/CreateAuthzExtension, google.cloud.networkservices.v1.DepService/GetAuthzExtension, google.cloud.networkservices.v1.DepService/UpdateAuthzExtension, google.cloud.networkservices.v1.DepService/DeleteAuthzExtension
func TestTerraformNetworkServices(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	nsv, err := networkservices.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nsv.Close() })
	dep, err := networkservices.NewDepRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dep.Close() })
	m := newTFModule(t)
	id := tfID("tfnsv-", 20)
	module := func(desc, env string) string {
		return fmt.Sprintf(`
resource "google_network_services_gateway" "mesh" {
  name        = "%[1]s-mesh"
  location    = "global"
  type        = "OPEN_MESH"
  scope       = "%[1]s-scope"
  ports       = [443]
  description = %[2]q
  labels      = { env = %[3]q }
}

resource "google_network_security_server_tls_policy" "s" {
  name       = "%[1]s-tls"
  location   = "global"
  allow_open = true
  server_certificate {
    certificate_provider_instance {
      plugin_instance = "google_cloud_private_spiffe"
    }
  }
}

resource "google_network_services_endpoint_policy" "e" {
  name              = "%[1]s-ep"
  type              = "GRPC_SERVER"
  description       = %[2]q
  labels            = { env = %[3]q }
  server_tls_policy = google_network_security_server_tls_policy.s.id
  endpoint_matcher {
    metadata_label_matcher {
      metadata_label_match_criteria = "MATCH_ALL"
      metadata_labels {
        label_name  = "app"
        label_value = "frontend"
      }
    }
  }
  traffic_port_selector {
    ports = ["8080"]
  }
}

resource "google_compute_network" "n" {
  name                    = %[1]q
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "s" {
  name          = %[1]q
  ip_cidr_range = "10.151.0.0/24"
  region        = "us-central1"
  network       = google_compute_network.n.id
}

resource "google_compute_subnetwork" "proxy" {
  name          = "%[1]s-proxy"
  ip_cidr_range = "10.152.0.0/23"
  region        = "us-central1"
  network       = google_compute_network.n.id
  purpose       = "REGIONAL_MANAGED_PROXY"
  role          = "ACTIVE"
}

resource "google_network_security_gateway_security_policy" "p" {
  name     = "%[1]s-policy"
  location = "us-central1"
}

resource "google_network_services_gateway" "swg" {
  name                                 = "%[1]s-swg"
  location                             = "us-central1"
  type                                 = "SECURE_WEB_GATEWAY"
  addresses                            = ["10.151.0.99"]
  ports                                = [443]
  scope                                = "%[1]s-swg"
  description                          = %[2]q
  labels                               = { env = %[3]q }
  network                              = google_compute_network.n.id
  subnetwork                           = google_compute_subnetwork.s.id
  gateway_security_policy              = google_network_security_gateway_security_policy.p.id
  delete_swg_autogen_router_on_destroy = true
  depends_on                           = [google_compute_subnetwork.proxy]
}

resource "google_compute_region_health_check" "hc" {
  name   = %[1]q
  region = "us-central1"
  grpc_health_check {
    port = 50051
  }
}

resource "google_compute_region_backend_service" "b" {
  name                  = %[1]q
  region                = "us-central1"
  load_balancing_scheme = "INTERNAL_MANAGED"
  protocol              = "HTTP2"
  health_checks         = [google_compute_region_health_check.hc.id]
}

resource "google_network_services_authz_extension" "x" {
  name                  = "%[1]s-ext"
  location              = "us-central1"
  description           = %[2]q
  labels                = { env = %[3]q }
  load_balancing_scheme = "INTERNAL_MANAGED"
  authority             = "authz.example.com"
  service               = google_compute_region_backend_service.b.self_link
  timeout               = "0.1s"
  fail_open             = false
  forward_headers       = ["Authorization"]
}
`, id, desc, env)
	}
	gl := "projects/" + m.project + "/locations/global/"
	uc := "projects/" + m.project + "/locations/us-central1/"
	read := func(desc, env string) {
		t.Helper()
		g, err := nsv.GetGateway(ctx, &networkservicespb.GetGatewayRequest{Name: gl + "gateways/" + id + "-mesh"})
		if err != nil || g.GetType() != networkservicespb.Gateway_OPEN_MESH || g.GetScope() != id+"-scope" || g.GetDescription() != desc || g.GetLabels()["env"] != env {
			t.Errorf("GetGateway mesh = %v, %v", g, err)
		}
		e, err := nsv.GetEndpointPolicy(ctx, &networkservicespb.GetEndpointPolicyRequest{Name: gl + "endpointPolicies/" + id + "-ep"})
		if err != nil || e.GetType() != networkservicespb.EndpointPolicy_GRPC_SERVER || !strings.HasSuffix(e.GetServerTlsPolicy(), "/serverTlsPolicies/"+id+"-tls") ||
			e.GetDescription() != desc || e.GetLabels()["env"] != env {
			t.Errorf("GetEndpointPolicy = %v, %v", e, err)
		}
		s, err := nsv.GetGateway(ctx, &networkservicespb.GetGatewayRequest{Name: uc + "gateways/" + id + "-swg"})
		if err != nil || s.GetType() != networkservicespb.Gateway_SECURE_WEB_GATEWAY || len(s.GetAddresses()) != 1 || s.GetAddresses()[0] != "10.151.0.99" ||
			!strings.HasSuffix(s.GetGatewaySecurityPolicy(), "/gatewaySecurityPolicies/"+id+"-policy") || s.GetDescription() != desc || s.GetLabels()["env"] != env {
			t.Errorf("GetGateway swg = %v, %v", s, err)
		}
		x, err := dep.GetAuthzExtension(ctx, &networkservicespb.GetAuthzExtensionRequest{Name: uc + "authzExtensions/" + id + "-ext"})
		if err != nil || x.GetAuthority() != "authz.example.com" || x.GetLoadBalancingScheme() != networkservicespb.LoadBalancingScheme_INTERNAL_MANAGED ||
			x.GetTimeout().AsDuration() != 100*time.Millisecond || x.GetDescription() != desc || x.GetLabels()["env"] != env {
			t.Errorf("GetAuthzExtension = %v, %v", x, err)
		}
	}
	m.write(module("made by Terraform", "dev"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	read("made by Terraform", "dev")
	m.planClean()
	m.write(module("changed by Terraform", "prod"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !tfInPlaceOnly(out) {
		t.Errorf("the update was not in place:\n%s", lastLines(out, 60))
	}
	read("changed by Terraform", "prod")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = nsv.GetGateway(ctx, &networkservicespb.GetGatewayRequest{Name: gl + "gateways/" + id + "-mesh"})
	tfNotFound(t, "GetGateway mesh", err)
	_, err = nsv.GetGateway(ctx, &networkservicespb.GetGatewayRequest{Name: uc + "gateways/" + id + "-swg"})
	tfNotFound(t, "GetGateway swg", err)
	_, err = nsv.GetEndpointPolicy(ctx, &networkservicespb.GetEndpointPolicyRequest{Name: gl + "endpointPolicies/" + id + "-ep"})
	tfNotFound(t, "GetEndpointPolicy", err)
	_, err = dep.GetAuthzExtension(ctx, &networkservicespb.GetAuthzExtensionRequest{Name: uc + "authzExtensions/" + id + "-ext"})
	tfNotFound(t, "GetAuthzExtension", err)
}

// TestTerraformIdentityAwareProxy (#1578): IAP through iap_custom_endpoint:
// IAM members of the project's web resource, of a backend service's and
// of the project's TCP forwarding resource, the compute web type's
// settings, and a tunnel destination group. The official iap/apiv1 REST
// client reads each; the plan is clean; the settings' cookie domain, the group's ranges
// and the tunnel binding's members change in place; destroy removes them.
// (google_iap_brand and google_iap_client are refused FAILED_PRECONDITION,
// as Google's OAuth Admin API refuses them since January 22, 2025.)
// covers: google.cloud.iap.v1.IdentityAwareProxyAdminService/SetIamPolicy, google.cloud.iap.v1.IdentityAwareProxyAdminService/GetIamPolicy, google.cloud.iap.v1.IdentityAwareProxyAdminService/GetIapSettings, google.cloud.iap.v1.IdentityAwareProxyAdminService/UpdateIapSettings, google.cloud.iap.v1.IdentityAwareProxyAdminService/CreateTunnelDestGroup, google.cloud.iap.v1.IdentityAwareProxyAdminService/GetTunnelDestGroup, google.cloud.iap.v1.IdentityAwareProxyAdminService/UpdateTunnelDestGroup, google.cloud.iap.v1.IdentityAwareProxyAdminService/DeleteTunnelDestGroup
func TestTerraformIdentityAwareProxy(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	admin, err := iap.NewIdentityAwareProxyAdminRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	m := newTFModule(t)
	id := tfID("tfiap-", 20)
	web := "user:" + id + "-web@example.com"
	bs := "user:" + id + "-bs@example.com"
	module := func(cookie, cidr string, tunnel ...string) string {
		members := make([]string, len(tunnel))
		for i, u := range tunnel {
			members[i] = fmt.Sprintf("%q", u)
		}
		return fmt.Sprintf(`
resource "google_iap_web_iam_member" "w" {
  role   = "roles/iap.httpsResourceAccessor"
  member = %[2]q
}

resource "google_compute_health_check" "hc" {
  name = %[1]q
  http_health_check {
    port = 80
  }
}

resource "google_compute_backend_service" "b" {
  name                  = %[1]q
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTP"
  health_checks         = [google_compute_health_check.hc.id]
}

resource "google_iap_web_backend_service_iam_member" "b" {
  web_backend_service = google_compute_backend_service.b.name
  role                = "roles/iap.httpsResourceAccessor"
  member              = %[3]q
}

resource "google_iap_tunnel_iam_binding" "t" {
  role    = "roles/iap.tunnelResourceAccessor"
  members = [%[4]s]
}

resource "google_iap_settings" "s" {
  name = "projects/%[5]s/iap_web/compute"
  access_settings {
    cors_settings {
      allow_http_options = true
    }
  }
  application_settings {
    cookie_domain = %[6]q
  }
}

resource "google_iap_tunnel_dest_group" "g" {
  region     = "us-central1"
  group_name = "%[1]s-dg"
  cidrs      = [%[7]q]
  fqdns      = ["db.internal.example.com"]
}
`, id, web, bs, strings.Join(members, ", "), m.project, cookie, cidr)
	}
	pweb := "projects/" + m.project + "/iap_web"
	members := func(resource, role string) []string {
		t.Helper()
		p, err := admin.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
		if err != nil {
			t.Fatalf("GetIamPolicy %s: %v", resource, err)
		}
		for _, b := range p.GetBindings() {
			if b.GetRole() == role {
				return b.GetMembers()
			}
		}
		return nil
	}
	group := "projects/" + m.project + "/iap_tunnel/locations/us-central1/destGroups/" + id + "-dg"
	read := func(cookie, cidr string, tunnel ...string) {
		t.Helper()
		if got := members(pweb, "roles/iap.httpsResourceAccessor"); !slices.Contains(got, web) {
			t.Errorf("the web resource's accessors are %v; want %s", got, web)
		}
		if got := members(pweb+"/compute/services/"+id, "roles/iap.httpsResourceAccessor"); !slices.Equal(got, []string{bs}) {
			t.Errorf("the backend service's accessors are %v; want %s", got, bs)
		}
		got := members("projects/"+m.project+"/iap_tunnel", "roles/iap.tunnelResourceAccessor")
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(tunnel)); !slices.Equal(got, want) {
			t.Errorf("the tunnel's accessors are %v; want %v", got, want)
		}
		st, err := admin.GetIapSettings(ctx, &iappb.GetIapSettingsRequest{Name: pweb + "/compute"})
		if err != nil || !st.GetAccessSettings().GetCorsSettings().GetAllowHttpOptions().GetValue() || st.GetApplicationSettings().GetCookieDomain().GetValue() != cookie {
			t.Errorf("GetIapSettings = %v, %v; want cookie_domain %s", st, err, cookie)
		}
		g, err := admin.GetTunnelDestGroup(ctx, &iappb.GetTunnelDestGroupRequest{Name: group})
		if err != nil || !slices.Equal(g.GetCidrs(), []string{cidr}) || !slices.Equal(g.GetFqdns(), []string{"db.internal.example.com"}) {
			t.Errorf("GetTunnelDestGroup = %v, %v; want %s", g, err, cidr)
		}
	}
	a, b := "user:"+id+"-a@example.com", "user:"+id+"-b@example.com"
	m.write(module("a.example.com", "10.20.0.0/24", a))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	read("a.example.com", "10.20.0.0/24", a)
	m.planClean()
	m.write(module("b.example.com", "10.21.0.0/24", a, b))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !tfInPlaceOnly(out) {
		t.Errorf("the update was not in place:\n%s", lastLines(out, 60))
	}
	read("b.example.com", "10.21.0.0/24", a, b)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if got := members(pweb, "roles/iap.httpsResourceAccessor"); slices.Contains(got, web) {
		t.Errorf("after destroy the web resource's accessors are %v", got)
	}
	if got := members("projects/"+m.project+"/iap_tunnel", "roles/iap.tunnelResourceAccessor"); len(got) != 0 {
		t.Errorf("after destroy the tunnel's accessors are %v", got)
	}
	_, err = admin.GetTunnelDestGroup(ctx, &iappb.GetTunnelDestGroupRequest{Name: group})
	tfNotFound(t, "GetTunnelDestGroup", err)
}

// TestTerraformVPCAccessConnector (#1578): a Serverless VPC Access
// connector on a network the module makes, through
// vpc_access_custom_endpoint; the official vpcaccess/apiv1 REST client
// reads it, the plan is clean, its max_instances and machine_type change
// in place, and destroy removes it.
// covers: google.cloud.vpcaccess.v1.VpcAccessService/CreateConnector, google.cloud.vpcaccess.v1.VpcAccessService/GetConnector, google.cloud.vpcaccess.v1.VpcAccessService/DeleteConnector
func TestTerraformVPCAccessConnector(t *testing.T) {
	h := New(t)
	c := vpcAccessClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	m := newTFModule(t)
	id := tfID("tfvpc-", 20)
	module := func(max int, machine string) string {
		return fmt.Sprintf(`
resource "google_compute_network" "n" {
  name                    = %[1]q
  auto_create_subnetworks = false
}

resource "google_vpc_access_connector" "c" {
  name          = %[1]q
  region        = "us-central1"
  network       = google_compute_network.n.name
  ip_cidr_range = "10.8.0.0/28"
  min_instances = 2
  max_instances = %[2]d
  machine_type  = %[3]q
}
`, id, max, machine)
	}
	name := "projects/" + m.project + "/locations/us-central1/connectors/" + id
	read := func(max int32, machine string) {
		t.Helper()
		cn, err := c.GetConnector(ctx, &vpcaccesspb.GetConnectorRequest{Name: name})
		if err != nil || cn.GetIpCidrRange() != "10.8.0.0/28" || cn.GetNetwork() != id || cn.GetMinInstances() != 2 || cn.GetMaxInstances() != max ||
			cn.GetMachineType() != machine || cn.GetState() != vpcaccesspb.Connector_READY {
			t.Fatalf("GetConnector = %v, %v; want max %d, %s", cn, err, max, machine)
		}
	}
	m.write(module(3, "e2-micro"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	read(3, "e2-micro")
	m.planClean()
	m.write(module(4, "e2-standard-4"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !tfInPlaceOnly(out) {
		t.Errorf("the update was not in place:\n%s", lastLines(out, 40))
	}
	read(4, "e2-standard-4")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err := c.GetConnector(ctx, &vpcaccesspb.GetConnectorRequest{Name: name})
	tfNotFound(t, "GetConnector", err)
}

// TestTerraformNetworkSecurityAuthorizationPolicy (#1578): google-beta's
// google_network_security_authorization_policy (hashicorp/google has
// none) through network_security_custom_endpoint; the official
// networksecurity/apiv1 REST client reads it, the plan is clean, its
// action and description change in place, and destroy removes it.
// covers: google.cloud.networksecurity.v1.NetworkSecurity/CreateAuthorizationPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetAuthorizationPolicy, google.cloud.networksecurity.v1.NetworkSecurity/UpdateAuthorizationPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteAuthorizationPolicy
func TestTerraformNetworkSecurityAuthorizationPolicy(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	m := newTFModule(t)
	id := tfID("tfaz-", 20)
	conf := func(action, desc string) string {
		return fmt.Sprintf(`
terraform {
  required_providers {
    google      = { source = "hashicorp/google", version = %[4]q }
    google-beta = { source = "hashicorp/google-beta", version = %[4]q }
  }
}

resource "google_network_security_authorization_policy" "a" {
  provider    = google-beta
  name        = %[1]q
  location    = "global"
  description = %[3]q
  labels      = { team = "burrow" }
  action      = %[2]q
  rules {
    sources {
      principals = ["spiffe://example.com/ns/default/sa/frontend"]
      ip_blocks  = ["10.0.0.0/8"]
    }
    destinations {
      hosts   = ["*.example.com"]
      ports   = [443]
      methods = ["GET", "POST"]
    }
  }
}
`, id, action, desc, googleProviderVersion)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(m.dir, "main.tf"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	name := "projects/" + m.project + "/locations/global/authorizationPolicies/" + id
	read := func(action networksecuritypb.AuthorizationPolicy_Action, desc string) {
		t.Helper()
		ap, err := ns.GetAuthorizationPolicy(ctx, &networksecuritypb.GetAuthorizationPolicyRequest{Name: name})
		if err != nil || ap.GetAction() != action || len(ap.GetRules()) != 1 || len(ap.GetRules()[0].GetDestinations()) != 1 ||
			!slices.Equal(ap.GetRules()[0].GetDestinations()[0].GetPorts(), []uint32{443}) || ap.GetDescription() != desc {
			t.Fatalf("GetAuthorizationPolicy = %v, %v; want %v %q", ap, err, action, desc)
		}
	}
	write(conf("ALLOW", "made by Terraform"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	read(networksecuritypb.AuthorizationPolicy_ALLOW, "made by Terraform")
	m.planClean()
	write(conf("DENY", "changed by Terraform"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !tfInPlaceOnly(out) {
		t.Errorf("the update was not in place:\n%s", lastLines(out, 40))
	}
	read(networksecuritypb.AuthorizationPolicy_DENY, "changed by Terraform")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = ns.GetAuthorizationPolicy(ctx, &networksecuritypb.GetAuthorizationPolicyRequest{Name: name})
	tfNotFound(t, "GetAuthorizationPolicy", err)
}

// TestTerraformDataprocTemplatesAndPolicy (#1578): Dataproc through
// dataproc_custom_endpoint: an autoscaling policy, a workflow template
// whose managed cluster uses it, and a Serverless for Apache Spark session
// template. Nothing runs, so no Spark image is needed. The official
// dataproc/apiv1 REST clients read each; the plan is clean; the policy's
// limits and the session template's properties change in place; destroy
// removes them.
// covers: google.cloud.dataproc.v1.AutoscalingPolicyService/CreateAutoscalingPolicy, google.cloud.dataproc.v1.AutoscalingPolicyService/GetAutoscalingPolicy, google.cloud.dataproc.v1.AutoscalingPolicyService/UpdateAutoscalingPolicy, google.cloud.dataproc.v1.AutoscalingPolicyService/DeleteAutoscalingPolicy, google.cloud.dataproc.v1.WorkflowTemplateService/CreateWorkflowTemplate, google.cloud.dataproc.v1.WorkflowTemplateService/GetWorkflowTemplate, google.cloud.dataproc.v1.WorkflowTemplateService/DeleteWorkflowTemplate, google.cloud.dataproc.v1.SessionTemplateController/CreateSessionTemplate, google.cloud.dataproc.v1.SessionTemplateController/GetSessionTemplate, google.cloud.dataproc.v1.SessionTemplateController/UpdateSessionTemplate, google.cloud.dataproc.v1.SessionTemplateController/DeleteSessionTemplate
func TestTerraformDataprocTemplatesAndPolicy(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvDataproc) // skips without it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	opts := []option.ClientOption{option.WithEndpoint("http://" + addr), option.WithoutAuthentication()}
	pc, err := dataproc.NewAutoscalingPolicyRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	wc, err := dataproc.NewWorkflowTemplateRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	sc, err := dataproc.NewSessionTemplateControllerRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	m := newTFModule(t)
	id := tfID("tfdp-", 20)
	module := func(max int, shuffle string) string {
		return fmt.Sprintf(`
resource "google_dataproc_autoscaling_policy" "p" {
  policy_id = "%[1]s-policy"
  location  = "us-central1"
  worker_config {
    min_instances = 2
    max_instances = %[2]d
  }
  basic_algorithm {
    cooldown_period = "120s"
    yarn_config {
      graceful_decommission_timeout = "30s"
      scale_up_factor               = 0.5
      scale_down_factor             = 0.5
    }
  }
}

resource "google_dataproc_workflow_template" "w" {
  name     = "%[1]s-wf"
  location = "us-central1"
  placement {
    managed_cluster {
      cluster_name = "%[1]s-c"
      config {
        master_config {
          num_instances = 1
          machine_type  = "n1-standard-2"
        }
        worker_config {
          num_instances = 2
          machine_type  = "n1-standard-2"
        }
        autoscaling_config {
          policy = google_dataproc_autoscaling_policy.p.id
        }
      }
    }
  }
  jobs {
    step_id = "spark-pi"
    spark_job {
      main_class    = "org.apache.spark.examples.SparkPi"
      jar_file_uris = ["file:///usr/lib/spark/examples/jars/spark-examples.jar"]
      args          = ["10"]
    }
  }
}

resource "google_dataproc_session_template" "s" {
  name     = "projects/%[3]s/locations/us-central1/sessionTemplates/%[1]s-st"
  location = "us-central1"
  labels   = { team = "burrow" }
  runtime_config {
    properties = { "spark.sql.shuffle.partitions" = %[4]q }
  }
  jupyter_session {
    kernel       = "PYTHON"
    display_name = "Terraform session"
  }
}
`, id, max, m.project, shuffle)
	}
	parent := "projects/" + m.project + "/regions/us-central1/"
	read := func(max int32, shuffle string) {
		t.Helper()
		p, err := pc.GetAutoscalingPolicy(ctx, &dataprocpb.GetAutoscalingPolicyRequest{Name: parent + "autoscalingPolicies/" + id + "-policy"})
		if err != nil || p.GetWorkerConfig().GetMinInstances() != 2 || p.GetWorkerConfig().GetMaxInstances() != max ||
			p.GetBasicAlgorithm().GetYarnConfig().GetScaleUpFactor() != 0.5 {
			t.Errorf("GetAutoscalingPolicy = %v, %v; want max %d", p, err, max)
		}
		w, err := wc.GetWorkflowTemplate(ctx, &dataprocpb.GetWorkflowTemplateRequest{Name: parent + "workflowTemplates/" + id + "-wf"})
		if err != nil || w.GetPlacement().GetManagedCluster().GetClusterName() != id+"-c" || len(w.GetJobs()) != 1 || w.GetJobs()[0].GetStepId() != "spark-pi" ||
			!strings.HasSuffix(w.GetPlacement().GetManagedCluster().GetConfig().GetAutoscalingConfig().GetPolicyUri(), "/autoscalingPolicies/"+id+"-policy") {
			t.Errorf("GetWorkflowTemplate = %v, %v", w, err)
		}
		st, err := sc.GetSessionTemplate(ctx, &dataprocpb.GetSessionTemplateRequest{Name: "projects/" + m.project + "/locations/us-central1/sessionTemplates/" + id + "-st"})
		if err != nil || st.GetRuntimeConfig().GetProperties()["spark.sql.shuffle.partitions"] != shuffle || st.GetJupyterSession().GetDisplayName() != "Terraform session" ||
			st.GetLabels()["team"] != "burrow" {
			t.Errorf("GetSessionTemplate = %v, %v; want %s", st, err, shuffle)
		}
	}
	m.write(module(4, "8"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	read(4, "8")
	m.planClean()
	m.write(module(6, "16"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !tfInPlaceOnly(out) {
		t.Errorf("the update was not in place:\n%s", lastLines(out, 40))
	}
	read(6, "16")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	_, err = pc.GetAutoscalingPolicy(ctx, &dataprocpb.GetAutoscalingPolicyRequest{Name: parent + "autoscalingPolicies/" + id + "-policy"})
	tfNotFound(t, "GetAutoscalingPolicy", err)
	_, err = wc.GetWorkflowTemplate(ctx, &dataprocpb.GetWorkflowTemplateRequest{Name: parent + "workflowTemplates/" + id + "-wf"})
	tfNotFound(t, "GetWorkflowTemplate", err)
	_, err = sc.GetSessionTemplate(ctx, &dataprocpb.GetSessionTemplateRequest{Name: "projects/" + m.project + "/locations/us-central1/sessionTemplates/" + id + "-st"})
	tfNotFound(t, "GetSessionTemplate", err)
}
