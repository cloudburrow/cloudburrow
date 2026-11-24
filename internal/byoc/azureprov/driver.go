package azureprov

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// Driver is Azure's byoc.Driver.
type Driver struct{ opts Options }

// New is Azure's driver.
func New(opts Options) *Driver { return &Driver{opts: opts.withDefaults()} }

var _ byoc.Driver = (*Driver)(nil)

// Cloud implements byoc.Driver.
func (*Driver) Cloud() byoc.Cloud { return byoc.Azure }

// BeginSignIn implements byoc.Driver: Microsoft's sign-in page, in the
// user's browser, then a subscription.
func (d *Driver) BeginSignIn(_ context.Context, opts byoc.SignInOptions) (byoc.SignIn, error) {
	tenant := strings.TrimSpace(opts.Tenant)
	if tenant == "" {
		tenant = DefaultTenant
	}
	return &signIn{d: d, opts: opts, tenant: tenant}, nil
}

// ConnectKey implements byoc.Driver: Azure has no access-key fallback.
func (*Driver) ConnectKey(context.Context, byoc.AccessKey) (byoc.Connection, []byte, error) {
	return byoc.Connection{}, nil, fmt.Errorf("azure has no access-key sign-in: connect Azure with Microsoft's sign-in page in your browser: %w", byoc.ErrUnsupported)
}

// Open implements byoc.Driver.
func (d *Driver) Open(ctx context.Context, conn byoc.Connection, sec []byte, save func([]byte) error) (byoc.Provider, error) {
	var cred azcore.TokenCredential
	sub := conn.AccountID
	if d.opts.Credential != nil {
		cred = d.opts.Credential
	} else {
		var s secret
		if err := json.Unmarshal(sec, &s); err != nil || len(s.Cache) == 0 {
			return nil, &byoc.AuthError{Message: "no stored Microsoft sign-in for " + conn.ID + ": connect Azure again"}
		}
		if s.Subscription != "" {
			sub = s.Subscription
		}
		c, err := d.opts.newCredential(ctx, s, save)
		if err != nil {
			return nil, err
		}
		cred = c
	}
	return NewProvider(sub, cred, d.opts)
}

type subscription struct{ id, name string }

func listSubscriptions(ctx context.Context, o Options, cred azcore.TokenCredential) ([]subscription, error) {
	client, err := armsubscriptions.NewClient(cred, o.armOptions())
	if err != nil {
		return nil, err
	}
	var out []subscription
	pager := client.NewListPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError("listing your Azure subscriptions", err)
		}
		for _, s := range page.Value {
			if s == nil || s.SubscriptionID == nil {
				continue
			}
			if s.State != nil && *s.State != armsubscriptions.SubscriptionStateEnabled {
				continue
			}
			out = append(out, subscription{id: *s.SubscriptionID, name: str(s.DisplayName)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func float(p *string) float64 {
	if p == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(*p, 64)
	return f
}
