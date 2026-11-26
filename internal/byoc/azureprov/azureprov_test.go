package azureprov_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov/azurefake"
)

// Every test runs against azurefake, a local TLS server: nothing here
// reaches Microsoft.

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIC7e0mQnEy0wGmT0bcLQlh5hWm7Q2a0p9x3Yb1sT5d0k cloudburrow-test"

func fake(t *testing.T) *azurefake.Server {
	t.Helper()
	f := azurefake.New()
	t.Cleanup(f.Close)
	return f
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// signIn runs the browser sign-in against the fake and connects sub.
func signIn(t *testing.T, f *azurefake.Server, sub string) (*azureprov.Driver, byoc.Connection, []byte) {
	t.Helper()
	d := azureprov.New(f.Options())
	si, err := d.BeginSignIn(ctx(t), byoc.SignInOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(si.Prompt().Message, "Microsoft") {
		t.Errorf("prompt = %+v", si.Prompt())
	}
	choices, err := si.Wait(ctx(t))
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(choices) != 2 {
		t.Fatalf("choices = %+v, want the fake's two subscriptions", choices)
	}
	conn, secret, err := si.Choose(ctx(t), sub)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	return d, conn, secret
}

func open(t *testing.T, f *azurefake.Server) *azureprov.Provider {
	t.Helper()
	d, conn, secret := signIn(t, f, azurefake.Subscription)
	p, err := d.Open(ctx(t), conn, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p.(*azureprov.Provider)
}

func TestSignInChoosesASubscription(t *testing.T) {
	f := fake(t)
	d, conn, secret := signIn(t, f, azurefake.Subscription2)
	if conn.ID != "azure-"+azurefake.Subscription2 || conn.Cloud != byoc.Azure || conn.Method != "browser" ||
		conn.AccountName != azurefake.SubName2 || conn.Principal != azurefake.Username || conn.Detail["tenant"] != azurefake.TenantID {
		t.Errorf("connection = %+v", conn)
	}
	if f.TokenGrants["authorization_code"] != 1 {
		t.Errorf("token grants = %v, want one authorization code (PKCE) grant", f.TokenGrants)
	}
	p, err := d.Open(ctx(t), conn, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := p.Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != azurefake.Subscription2 || acct.Name != azurefake.SubName2 || acct.Principal != azurefake.Username {
		t.Errorf("account = %+v", acct)
	}
	if _, _, err := mustSignIn(t, f).Choose(ctx(t), "not-a-subscription"); !errors.Is(err, byoc.ErrNotFound) {
		t.Errorf("choosing an unknown subscription: %v", err)
	}
}

func mustSignIn(t *testing.T, f *azurefake.Server) byoc.SignIn {
	si, err := azureprov.New(f.Options()).BeginSignIn(ctx(t), byoc.SignInOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := si.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
	return si
}

func TestSignInFailsWhenTheBrowserCannotOpen(t *testing.T) {
	f := fake(t)
	si, _ := azureprov.New(f.Options()).BeginSignIn(ctx(t), byoc.SignInOptions{OpenBrowser: func(string) error {
		return errors.New("no browser")
	}})
	if _, err := si.Wait(ctx(t)); err == nil || !strings.Contains(err.Error(), "no browser") {
		t.Errorf("Wait = %v", err)
	}
	if _, _, err := si.Choose(ctx(t), azurefake.Subscription); err == nil {
		t.Error("Choose before a sign-in succeeded")
	}
}

func TestSilentRefreshSavesTheCache(t *testing.T) {
	f := fake(t)
	// Tokens that MSAL treats as near expiry: every silent call refreshes.
	f.TokenLifetime = time.Minute
	d, conn, secret := signIn(t, f, azurefake.Subscription)
	var mu sync.Mutex
	var saved []byte
	p, err := d.Open(ctx(t), conn, secret, func(b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		saved = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Connect(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if f.TokenGrants["refresh_token"] == 0 {
		t.Errorf("grants = %v: no refresh", f.TokenGrants)
	}
	mu.Lock()
	got := saved
	mu.Unlock()
	if len(got) == 0 || string(got) == string(secret) {
		t.Fatal("the refreshed cache was not saved")
	}
	// The saved secret opens on its own: the old refresh token is spent.
	p2, err := d.Open(ctx(t), conn, got, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Connect(ctx(t)); err != nil {
		t.Fatalf("Connect with the saved cache: %v", err)
	}
}

func TestRevokedSignInIsAnAuthError(t *testing.T) {
	f := fake(t)
	f.TokenLifetime = time.Minute
	d, conn, secret := signIn(t, f, azurefake.Subscription)
	f.Revoke()
	p, err := d.Open(ctx(t), conn, secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ae *byoc.AuthError
	if _, err := p.Connect(ctx(t)); !errors.As(err, &ae) {
		t.Errorf("Connect after revocation = %v, want *byoc.AuthError", err)
	}
	if _, err := d.Open(ctx(t), conn, []byte("{}"), nil); !errors.As(err, &ae) {
		t.Errorf("Open with no cache = %v, want *byoc.AuthError", err)
	}
	// A provider whose token the fake no longer accepts.
	o := f.Options()
	o.Credential = badCred{}
	bp, err := azureprov.New(o).Open(ctx(t), conn, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bp.Regions(ctx(t)); !errors.As(err, &ae) {
		t.Errorf("Regions with a rejected token = %v, want *byoc.AuthError", err)
	}
}

func TestConnectKeyIsUnsupported(t *testing.T) {
	_, _, err := azureprov.New(azureprov.Options{}).ConnectKey(context.Background(), byoc.AccessKey{AccessKeyID: "x"})
	if !errors.Is(err, byoc.ErrUnsupported) || !strings.Contains(err.Error(), "browser") {
		t.Errorf("ConnectKey = %v", err)
	}
}

func TestRegionsArePhysical(t *testing.T) {
	p := open(t, fake(t))
	regions, err := p.Regions(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 3 {
		t.Fatalf("regions = %+v, want the three physical ones", regions)
	}
	for _, r := range regions {
		if r.Name == "eastus" && (r.DisplayName != "East US" || math.Abs(r.Latitude-37.3719) > 1e-6 || math.Abs(r.Longitude+79.8164) > 1e-6) {
			t.Errorf("eastus = %+v", r)
		}
	}
}

func TestCheckPermissions(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	rep, err := p.CheckPermissions(ctx(t), "eastus")
	if err != nil || !rep.Checked || !rep.OK() {
		t.Fatalf("Owner's report = %+v, %v", rep, err)
	}
	f.Deny("Microsoft.Network/networkSecurityGroups/write")
	f.Deny("Microsoft.Authorization/roleAssignments/write")
	rep, err = p.CheckPermissions(ctx(t), "eastus")
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, m := range rep.Missing {
		missing = append(missing, m.Action)
		if m.Why == "" {
			t.Errorf("%s has no reason", m.Action)
		}
	}
	if strings.Join(missing, ",") != "Microsoft.Network/networkSecurityGroups/write,Microsoft.Authorization/roleAssignments/write" {
		t.Errorf("missing = %v", missing)
	}
	if !strings.Contains(rep.Summary(), "auto-stop") {
		t.Errorf("summary %q does not explain the role assignment", rep.Summary())
	}
	f.DenyPermissionsList = true
	rep, err = p.CheckPermissions(ctx(t), "eastus")
	if err != nil || rep.Checked || rep.Note == "" {
		t.Errorf("report when the list is refused = %+v, %v", rep, err)
	}
}

func TestSizesArePriced(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	sizes, err := p.Sizes(ctx(t), "eastus")
	if err != nil {
		t.Fatal(err)
	}
	want := map[byoc.Preset]struct {
		name  string
		price float64
	}{byoc.Small: {"Standard_D4s_v5", 0.192}, byoc.Medium: {"Standard_D16s_v5", 0.768},
		byoc.Large: {"Standard_D32s_v5", 1.536}, byoc.GPU: {"Standard_NC4as_T4_v3", 0.526}}
	if len(sizes) != 4 {
		t.Fatalf("sizes = %+v", sizes)
	}
	for _, s := range sizes {
		w := want[s.Preset]
		// The Linux pay-as-you-go meter, not the Windows, Spot or Low
		// Priority ones listed before it.
		if s.Name != w.name || s.HourlyUSD != w.price || s.PriceError != "" {
			t.Errorf("%s = %+v, want %s at %v", s.Preset, s, w.name, w.price)
		}
		if math.Abs(s.DiskHourlyUSD-19.71/730) > 1e-9 {
			t.Errorf("%s disk = %v", s.Preset, s.DiskHourlyUSD)
		}
		if s.Preset == byoc.GPU && (s.GPU != "NVIDIA T4" || !strings.Contains(s.Note, "no NVIDIA L4")) {
			t.Errorf("GPU preset = %+v", s)
		}
	}
	f.PricesDown = true
	sizes, err = p.Sizes(ctx(t), "eastus")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sizes {
		if s.HourlyUSD != 0 || !strings.Contains(s.PriceError, "503") {
			t.Errorf("%s with the price API down = %+v", s.Preset, s)
		}
	}
}

type recorder struct {
	mu     sync.Mutex
	events []byoc.Event
}

func (r *recorder) progress() byoc.Progress {
	return func(e byoc.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e)
	}
}

func (r *recorder) has(step string, status byoc.EventStatus, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Step == step && e.Status == status && strings.Contains(e.Message, text) {
			return true
		}
	}
	return false
}

func spec(instance string) byoc.MachineSpec {
	return byoc.MachineSpec{Instance: instance, Region: "eastus", Size: "Standard_D4s_v5", Preset: byoc.Small,
		AutoStopMinutes: 30, SSHUser: "cloudburrow", SSHPublicKey: testKey, SourceCIDR: "198.51.100.7/32",
		UserData: "#cloud-config\n"}
}

func rgID(instance string) string {
	return "/subscriptions/" + azurefake.Subscription + "/resourceGroups/" + azureprov.ResourceGroup(instance)
}

func TestCreateBuildsTaggedLockedDownVM(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	var rec recorder
	m, err := p.Create(ctx(t), spec("dev"), rec.progress())
	if err != nil {
		t.Fatal(err)
	}
	if m.State != byoc.Running || m.PublicIP == "" || m.PrivateIP != "10.209.0.4" || m.SSHPort != 22 || m.HourlyUSD != 0.192 ||
		m.IDs["vm"] == "" || m.IDs["disk"] == "" || m.IDs["principal"] == "" || m.IDs["nsg"] == "" {
		t.Errorf("machine = %+v", m)
	}
	// The NSG lets in SSH from the one address and nothing else.
	nsg := f.Resource(m.IDs["nsg"])
	rules := nsg["properties"].(map[string]any)["securityRules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules = %v", rules)
	}
	rule := rules[0].(map[string]any)["properties"].(map[string]any)
	if rule["sourceAddressPrefix"] != "198.51.100.7/32" || rule["destinationPortRange"] != "22" ||
		rule["direction"] != "Inbound" || rule["access"] != "Allow" || rule["protocol"] != "Tcp" {
		t.Errorf("rule = %v", rule)
	}
	// Every resource CloudBurrow created carries the tag; the OS disk Azure
	// made from the VM is in the group.
	for _, id := range f.IDs() {
		b := f.Resource(id)
		if b["type"] == "Microsoft.Authorization/roleAssignments" || b["type"] == "Microsoft.Compute/disks" {
			continue
		}
		tags, _ := b["tags"].(map[string]any)
		if tags[byoc.TagKey] != "dev" {
			t.Errorf("%s is not tagged: %v", id, b["tags"])
		}
	}
	vm := f.Resource(m.IDs["vm"])
	props := vm["properties"].(map[string]any)
	osp := props["osProfile"].(map[string]any)
	if osp["customData"] != "I2Nsb3VkLWNvbmZpZwo=" || osp["linuxConfiguration"].(map[string]any)["disablePasswordAuthentication"] != true {
		t.Errorf("osProfile = %v", osp)
	}
	od := props["storageProfile"].(map[string]any)["osDisk"].(map[string]any)
	if od["deleteOption"] != "Delete" || od["diskSizeGB"] != float64(128) {
		t.Errorf("osDisk = %v", od)
	}
	// The VM's identity may deallocate the VM and nothing else.
	ras := f.RoleAssignments()
	if len(ras) != 1 {
		t.Fatalf("role assignments = %v", ras)
	}
	rp := ras[0]["properties"].(map[string]any)
	if rp["scope"] != m.IDs["vm"] || !strings.HasSuffix(rp["roleDefinitionId"].(string), azureprov.VirtualMachineContributor) || rp["principalId"] != m.IDs["principal"] {
		t.Errorf("role assignment = %v", rp)
	}
	polled := false
	for _, r := range f.Requests {
		polled = polled || strings.HasPrefix(r, "GET /fake/operations/")
	}
	if !polled {
		t.Error("the VM create's Azure-AsyncOperation was never polled")
	}
	for _, s := range []string{"network", "firewall", "vm", "autostop"} {
		if !rec.has(s, byoc.StepDone, "") {
			t.Errorf("no %s done event: %+v", s, rec.events)
		}
	}
	res, err := p.ListResources(ctx(t), "dev", "eastus")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]bool{}
	for _, r := range res {
		types[r.Type] = true
	}
	for _, typ := range []string{"Microsoft.Resources/resourceGroups", "Microsoft.Network/virtualNetworks", "Microsoft.Network/networkSecurityGroups",
		"Microsoft.Network/publicIPAddresses", "Microsoft.Network/networkInterfaces", "Microsoft.Compute/virtualMachines", "Microsoft.Compute/disks"} {
		if !types[typ] {
			t.Errorf("ListResources lacks %s: %+v", typ, res)
		}
	}
}

func TestCreateWithoutSourceOpensNothing(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	sp := spec("closed")
	sp.SourceCIDR = ""
	m, err := p.Create(ctx(t), sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rules, _ := f.Resource(m.IDs["nsg"])["properties"].(map[string]any)["securityRules"].([]any); len(rules) != 0 {
		t.Errorf("rules = %v, want none", rules)
	}
}

func TestQuotaExceededRollsBack(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	f.FailNext("Microsoft.Compute/virtualMachines/write", 409, "QuotaExceeded",
		"Operation could not be completed as it results in exceeding approved standardDSv5Family Cores quota. Location: eastus, Current Limit: 0, Current Usage: 0, Additional Required: 4.")
	var rec recorder
	_, err := p.Create(ctx(t), spec("q"), rec.progress())
	var qe *byoc.QuotaError
	if !errors.As(err, &qe) || !strings.Contains(err.Error(), "standardDSv5Family") {
		t.Fatalf("Create = %v, want *byoc.QuotaError", err)
	}
	if ids := f.IDs(); len(ids) != 0 {
		t.Errorf("left after the rollback: %v", ids)
	}
	if !rec.has("rollback", byoc.StepRolledBack, "Removed resource group") || !rec.has("vm", byoc.StepFailed, "QuotaExceeded") {
		t.Errorf("events = %+v", rec.events)
	}
}

func TestMissingPermissionRollsBack(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	f.Deny("Microsoft.Network/networkSecurityGroups/write")
	_, err := p.Create(ctx(t), spec("perm"), nil)
	var pe *byoc.PermissionError
	if !errors.As(err, &pe) || len(pe.Missing) != 1 || pe.Missing[0].Action != "Microsoft.Network/networkSecurityGroups/write" {
		t.Fatalf("Create = %v, want *byoc.PermissionError naming the NSG write", err)
	}
	if f.Resource(rgID("perm")) != nil {
		t.Error("the resource group outlived the rollback")
	}
	// No resource group could be made: nothing to roll back, the error
	// still names the permission.
	f.Deny("Microsoft.Resources/subscriptions/resourceGroups/write")
	if _, err := p.Create(ctx(t), spec("perm2"), nil); !errors.As(err, &pe) {
		t.Errorf("Create without resource group write = %v", err)
	}
}

func TestRoleAssignmentRefusedStillCreates(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	f.Deny("Microsoft.Authorization/roleAssignments/write")
	var rec recorder
	m, err := p.Create(ctx(t), spec("noauto"), rec.progress())
	if err != nil {
		t.Fatal(err)
	}
	if m.State != byoc.Running || len(f.RoleAssignments()) != 0 {
		t.Errorf("machine = %+v, role assignments %v", m, f.RoleAssignments())
	}
	if !rec.has("autostop", byoc.StepFailed, "keeps billing") {
		t.Errorf("events = %+v", rec.events)
	}
}

func TestPowerAndResize(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	m, err := p.Create(ctx(t), spec("pw"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m, err = p.Stop(ctx(t), m); err != nil || m.State != byoc.Stopped {
		t.Fatalf("Stop = %+v, %v", m, err)
	}
	if m, err = p.Start(ctx(t), m); err != nil || m.State != byoc.Running {
		t.Fatalf("Start = %+v, %v", m, err)
	}
	var rec recorder
	if m, err = p.Resize(ctx(t), m, "Standard_D16s_v5", rec.progress()); err != nil || m.Size != "Standard_D16s_v5" ||
		m.Preset != byoc.Medium || m.State != byoc.Running || m.HourlyUSD != 0.768 {
		t.Fatalf("Resize = %+v, %v", m, err)
	}
	// A size not on the VM's hardware: deallocate, resize, start.
	f.ResizeNeedsDeallocate["Standard_NC4as_T4_v3"] = true
	rec = recorder{}
	if m, err = p.Resize(ctx(t), m, "Standard_NC4as_T4_v3", rec.progress()); err != nil || m.Size != "Standard_NC4as_T4_v3" || m.State != byoc.Running {
		t.Fatalf("Resize with deallocation = %+v, %v", m, err)
	}
	if !rec.has("resize", byoc.StepRunning, "deallocating") || !rec.has("resize", byoc.StepDone, "Standard_NC4as_T4_v3") {
		t.Errorf("events = %+v", rec.events)
	}
	// A stopped VM resizes and stays stopped.
	if m, err = p.Stop(ctx(t), m); err != nil {
		t.Fatal(err)
	}
	if m, err = p.Resize(ctx(t), m, "Standard_D4s_v5", nil); err != nil || m.State != byoc.Stopped || m.Size != "Standard_D4s_v5" {
		t.Errorf("Resize while stopped = %+v, %v", m, err)
	}
	// A refused resize says why.
	f.FailNext("Microsoft.Compute/virtualMachines/write", 409, "SkuNotAvailable", "The requested size for resource is currently not available in location 'eastus'.")
	var qe *byoc.QuotaError
	if _, err := p.Resize(ctx(t), m, "Standard_D32s_v5", nil); !errors.As(err, &qe) {
		t.Errorf("refused resize = %v", err)
	}
}

func TestStatusAfterOutsideChanges(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	m, err := p.Create(ctx(t), spec("st"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The idle script deallocated the VM.
	f.SetPower(m.IDs["vm"], "deallocated")
	if m, err = p.Status(ctx(t), m); err != nil || m.State != byoc.Stopped {
		t.Errorf("Status = %+v, %v", m, err)
	}
	if err := p.Delete(ctx(t), "st", "eastus", nil); err != nil {
		t.Fatal(err)
	}
	if m, err = p.Status(ctx(t), m); err != nil || m.State != byoc.Deleted {
		t.Errorf("Status after Delete = %+v, %v", m, err)
	}
}

func TestDeleteRemovesEverythingAndVerifies(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	if _, err := p.Create(ctx(t), spec("del"), nil); err != nil {
		t.Fatal(err)
	}
	// Another instance's machine is not touched.
	if _, err := p.Create(ctx(t), spec("other"), nil); err != nil {
		t.Fatal(err)
	}
	var rec recorder
	if err := p.Delete(ctx(t), "del", "eastus", rec.progress()); err != nil {
		t.Fatal(err)
	}
	for _, id := range f.IDs() {
		if strings.Contains(strings.ToLower(id), "/cloudburrow-del") {
			t.Errorf("left after Delete: %s", id)
		}
	}
	if n := len(f.RoleAssignments()); n != 1 {
		t.Errorf("%d role assignments left; want only the other instance's", n)
	}
	if res, err := p.ListResources(ctx(t), "del", "eastus"); err != nil || len(res) != 0 {
		t.Errorf("ListResources after Delete = %+v, %v", res, err)
	}
	if res, _ := p.ListResources(ctx(t), "other", "eastus"); len(res) < 7 {
		t.Errorf("the other instance's resources = %+v", res)
	}
	if !rec.has("verify", byoc.StepDone, "Nothing tagged") {
		t.Errorf("events = %+v", rec.events)
	}
	// Deleting again finds nothing and succeeds.
	if err := p.Delete(ctx(t), "del", "eastus", nil); err != nil {
		t.Errorf("second Delete = %v", err)
	}
}

func TestDeleteReportsLeftovers(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	if _, err := p.Create(ctx(t), spec("stuck"), nil); err != nil {
		t.Fatal(err)
	}
	f.KeepOnDelete("Microsoft.Network/publicIPAddresses")
	var rec recorder
	err := p.Delete(ctx(t), "stuck", "eastus", rec.progress())
	var le *byoc.LeftoverError
	if !errors.As(err, &le) || len(le.Resources) != 1 || le.Resources[0].Type != "Microsoft.Network/publicIPAddresses" {
		t.Fatalf("Delete = %v, want a *byoc.LeftoverError naming the public IP", err)
	}
	if !rec.has("verify", byoc.StepFailed, "left 1 resources") {
		t.Errorf("events = %+v", rec.events)
	}
}

func TestSecretHoldsNoPlainTokensOutsideTheCache(t *testing.T) {
	f := fake(t)
	_, conn, secret := signIn(t, f, azurefake.Subscription)
	var s map[string]any
	if err := json.Unmarshal(secret, &s); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"msal_cache", "home_account_id", "tenant", "subscription"} {
		if s[k] == nil || s[k] == "" {
			t.Errorf("secret lacks %s", k)
		}
	}
	// The connection, which is written to disk, holds no token.
	b, _ := json.Marshal(conn)
	if strings.Contains(string(b), "fake-at-") || strings.Contains(string(b), "fake-rt-") {
		t.Errorf("the connection holds a token: %s", b)
	}
}

// badCred hands out a token the fake does not accept.
type badCred struct{}

func (badCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "not-a-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// TestUpdateSourceAddressFollowsTheUser: the network security group's SSH
// rule moves to the user's new address, and admits only it.
func TestUpdateSourceAddressFollowsTheUser(t *testing.T) {
	f := fake(t)
	p := open(t, f)
	m, err := p.Create(ctx(t), spec("moves"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.UpdateSourceAddress(ctx(t), m, "198.51.100.77/32")
	if err != nil || got.SourceCIDR != "198.51.100.77/32" {
		t.Fatalf("UpdateSourceAddress = %+v %v", got, err)
	}
	rules, _ := f.Resource(m.IDs["nsg"])["properties"].(map[string]any)["securityRules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules %v, want one", rules)
	}
	props := rules[0].(map[string]any)["properties"].(map[string]any)
	if props["sourceAddressPrefix"] != "198.51.100.77/32" || props["destinationPortRange"] != "22" || props["access"] != "Allow" {
		t.Errorf("the SSH rule is %v", props)
	}
	f.Deny("Microsoft.Network/networkSecurityGroups/write")
	if _, err := p.UpdateSourceAddress(ctx(t), m, "192.0.2.9/32"); err == nil {
		t.Error("a refused update succeeded")
	}
}
