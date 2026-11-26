// Package azureprov is bring your own cloud's Azure (#2209): Microsoft's own
// browser sign-in, then a subscription's resource group, virtual network,
// network security group, public IP, network interface, VM and managed disk,
// through the Azure SDK for Go (MIT), priced by the Azure Retail Prices API.
//
// Nothing here reads ~/.azure or the Azure CLI's login: the only credential is
// the MSAL token cache the sign-in produced, which the caller keeps in the OS
// keychain.
package azureprov

import (
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// ClientID is the public client the sign-in uses: the Azure CLI's
// well-known application, which azidentity's InteractiveBrowserCredential
// also uses by default (developerSignOnClientID), so the user consents to
// nothing new and registers no application.
const ClientID = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"

// DefaultTenant is the tenant a sign-in starts in when none is named, as
// InteractiveBrowserCredential's default: work and school accounts.
const DefaultTenant = "organizations"

// DefaultPricesURL is the Azure Retail Prices API, which needs no sign-in.
const DefaultPricesURL = "https://prices.azure.com/api/retail/prices"

// Options configure the driver. The zero value is Azure's public cloud.
type Options struct {
	// AuthorityHost is Microsoft Entra ID's sign-in host
	// (https://login.microsoftonline.com/ by default).
	AuthorityHost string
	// ResourceManager is Azure Resource Manager's endpoint
	// (https://management.azure.com/ by default), and Audience its token
	// audience.
	ResourceManager string
	Audience        string
	// PricesURL is the Retail Prices API.
	PricesURL string
	// HTTPClient carries every request: MSAL's, the SDK's and the price
	// lookups. Tests pass their TLS fake's client.
	HTTPClient *http.Client
	// DisableInstanceDiscovery skips MSAL's authority validation, for an
	// authority that is not Microsoft's (a test fake).
	DisableInstanceDiscovery bool
	// Clock and PollInterval pace long-running operations.
	Clock        sched.Clock
	PollInterval time.Duration
	// Credential, when set, is used instead of the stored MSAL cache.
	Credential azcore.TokenCredential
	// OpenBrowser opens the sign-in page; SignInOptions.OpenBrowser wins.
	OpenBrowser func(url string) error
}

func (o Options) withDefaults() Options {
	if o.AuthorityHost == "" {
		o.AuthorityHost = cloud.AzurePublic.ActiveDirectoryAuthorityHost
	}
	if o.ResourceManager == "" {
		o.ResourceManager = cloud.AzurePublic.Services[cloud.ResourceManager].Endpoint
	}
	if o.Audience == "" {
		o.Audience = cloud.AzurePublic.Services[cloud.ResourceManager].Audience
	}
	if o.PricesURL == "" {
		o.PricesURL = DefaultPricesURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	if o.Clock == nil {
		o.Clock = sched.RealClock{}
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	return o
}

// scope is the token scope Resource Manager's calls need.
func (o Options) scope() string {
	return strings.TrimSuffix(o.Audience, "/") + "//.default"
}

// authority is the MSAL authority URL for tenant.
func (o Options) authority(tenant string) string {
	return strings.TrimSuffix(o.AuthorityHost, "/") + "/" + tenant
}

func (o Options) armOptions() *arm.ClientOptions {
	return &arm.ClientOptions{ClientOptions: policy.ClientOptions{
		Cloud: cloud.Configuration{
			ActiveDirectoryAuthorityHost: o.AuthorityHost,
			Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
				cloud.ResourceManager: {Endpoint: o.ResourceManager, Audience: o.Audience},
			},
		},
		Transport: o.HTTPClient,
	}}
}
