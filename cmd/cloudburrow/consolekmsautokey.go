package main

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
)

// The Cloud KMS EKM connections and Autokey screens (#1452), through the
// in-process EkmService, Autokey and AutokeyAdmin servers an SDK calls.

// kmsEkmProvider is the EKM connections screen: a project's connections in
// every location, Create EKM connection, a connection's page with its
// service resolver and certificate, Edit and Set as default.
type kmsEkmProvider struct{ svc *kmsService }

func (kmsEkmProvider) ID() string    { return "kms-ekm" }
func (kmsEkmProvider) Title() string { return "Cloud KMS EKM connections" }

// kmsEkmNote is what a stored connection is, and is not.
const kmsEkmNote = "Connections are stored and returned as an SDK wrote them; nothing dials the external key manager. " +
	"EXTERNAL_VPC keys, which use a connection, and Verify connectivity are not implemented."

func (p kmsEkmProvider) ekm() *kms.EkmServer {
	api := p.svc.API()
	if api == nil {
		return nil
	}
	return api.Ekm()
}

func (p kmsEkmProvider) List(ctx context.Context, project string) (console.Listing, error) {
	api := p.svc.API()
	if api == nil {
		return console.Listing{}, errors.New(kmsNotStarted)
	}
	listing := console.Listing{
		Columns:    []string{"Location", "Hostname", "Key management mode", "Created"},
		NameColumn: "EKM connection",
		Noun:       "EKM connections",
		Note:       kmsEkmNote,
	}
	if project == "" {
		listing.Prompt = "Cloud KMS lists EKM connections per project. Choose one in the toolbar."
		return listing, nil
	}
	conns, err := api.EkmConnectionsInProject(project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, c := range conns {
		host := "—"
		if rs := c.GetServiceResolvers(); len(rs) > 0 {
			host = rs[0].GetHostname()
		}
		listing.Items = append(listing.Items, console.Resource{
			Name: c.GetName(),
			Fields: map[string]string{
				"Location":            kmsLocation(c.GetName()),
				"Hostname":            host,
				"Key management mode": c.GetKeyManagementMode().String(),
				"Created":             kmsTime(c.GetCreateTime()),
			},
		})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (kmsEkmProvider) CreateForm() (string, []console.Field) {
	return "Create EKM connection", []console.Field{
		{Name: "ekmConnectionId", Label: "Connection name", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_\-]{1,63}$`,
			Help: "1-63 letters, digits, hyphens and underscores. " + kmsEkmNote},
		{Name: "location", Label: "Location", Type: "select", Required: true, Default: "us-central1", Options: kmsLocations()},
		{Name: "serviceDirectoryService", Label: "Service Directory service", Type: "text", Required: true,
			Help: "projects/PROJECT/locations/LOCATION/namespaces/NAMESPACE/services/SERVICE: where the EKM replica would be resolved."},
		{Name: "hostname", Label: "Hostname", Type: "text", Required: true, Help: "The EKM replica's hostname at the TLS and HTTP layers."},
		{Name: "certificate", Label: "Server certificate (PEM)", Type: "textarea", Required: true,
			Help: "The replica's leaf certificate. It is parsed as Cloud KMS parses it: issuer, subject, SANs, validity, serial number and fingerprint."},
		{Name: "keyManagementMode", Label: "Key management mode", Type: "select", Default: "MANUAL", Options: []string{"MANUAL", "CLOUD_KMS"}},
		{Name: "cryptoSpacePath", Label: "Crypto space path", Type: "text", Help: "Required when the key management mode is CLOUD_KMS."},
	}
}

// kmsCertificateDER reads one PEM certificate, or base64 DER.
func kmsCertificateDER(raw string) ([]byte, error) {
	if b, _ := pem.Decode([]byte(strings.TrimSpace(raw))); b != nil {
		return b.Bytes, nil
	}
	return kmsB64("the certificate", raw)
}

func kmsEkmMode(v string) kmspb.EkmConnection_KeyManagementMode {
	return kmspb.EkmConnection_KeyManagementMode(kmspb.EkmConnection_KeyManagementMode_value[strings.TrimSpace(v)])
}

func (p kmsEkmProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	e := p.ekm()
	if e == nil {
		return "", errors.New(kmsNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating an EKM connection")
	}
	der, err := kmsCertificateDER(values["certificate"])
	if err != nil {
		return "", err
	}
	location := strings.TrimSpace(values["location"])
	c, err := e.CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{
		Parent: "projects/" + project + "/locations/" + location, EkmConnectionId: strings.TrimSpace(values["ekmConnectionId"]),
		EkmConnection: &kmspb.EkmConnection{
			ServiceResolvers: []*kmspb.EkmConnection_ServiceResolver{{ServiceDirectoryService: strings.TrimSpace(values["serviceDirectoryService"]),
				Hostname: strings.TrimSpace(values["hostname"]), ServerCertificates: []*kmspb.Certificate{{RawDer: der}}}},
			KeyManagementMode: kmsEkmMode(values["keyManagementMode"]),
			CryptoSpacePath:   strings.TrimSpace(values["cryptoSpacePath"]),
		}})
	if err != nil {
		return "", err
	}
	return c.GetName(), nil
}

// kmsOwned checks that a full resource name belongs to the project the
// screen is scoped to.
func kmsOwned(project, name string) error {
	if project == "" {
		return fmt.Errorf("choose a project first")
	}
	if !strings.HasPrefix(name, "projects/"+project+"/") {
		return fmt.Errorf("%s is not in project %s", name, project)
	}
	return nil
}

func (p kmsEkmProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	e := p.ekm()
	if e == nil {
		return console.Detail{Unavailable: kmsNotStarted}, nil
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	c, err := e.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: path[0]})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the EKM connection: " + consoleErr(err)}, nil
	}
	isDefault := "No"
	if cfg, err := e.GetEkmConfig(ctx, &kmspb.GetEkmConfigRequest{Name: kmsLocationOf(c.GetName()) + "/ekmConfig"}); err == nil && cfg.GetDefaultEkmConnection() == c.GetName() {
		isDefault = "Yes"
	}
	props := []console.Property{
		{Label: "Resource name", Value: c.GetName()},
		{Label: "Key management mode", Value: c.GetKeyManagementMode().String()},
		{Label: "Default for its location", Value: isDefault},
		{Label: "Etag", Value: c.GetEtag()},
	}
	if c.GetCryptoSpacePath() != "" {
		props = append(props, console.Property{Label: "Crypto space path", Value: c.GetCryptoSpacePath()})
	}
	groups := []console.PropertyGroup{{Heading: "Connection", Properties: props}}
	for _, r := range c.GetServiceResolvers() {
		rp := []console.Property{
			{Label: "Service Directory service", Value: r.GetServiceDirectoryService()},
			{Label: "Hostname", Value: r.GetHostname()},
		}
		if r.GetEndpointFilter() != "" {
			rp = append(rp, console.Property{Label: "Endpoint filter", Value: r.GetEndpointFilter()})
		}
		for _, cert := range r.GetServerCertificates() {
			if !cert.GetParsed() {
				rp = append(rp, console.Property{Label: "Certificate", Value: "Not parsed"})
				continue
			}
			rp = append(rp,
				console.Property{Label: "Certificate subject", Value: cert.GetSubject()},
				console.Property{Label: "Certificate issuer", Value: cert.GetIssuer()},
				console.Property{Label: "Certificate valid until", Value: kmsTime(cert.GetNotAfterTime())},
				console.Property{Label: "Certificate SHA-256", Value: cert.GetSha256Fingerprint()})
		}
		groups = append(groups, console.PropertyGroup{Heading: "Service resolver", Properties: rp})
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Location", Value: kmsLocation(c.GetName())},
			{Label: "Key management mode", Value: c.GetKeyManagementMode().String()},
			{Label: "Created", Value: kmsTime(c.GetCreateTime())},
		},
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: groups, Note: kmsEkmNote}},
	}, nil
}

// kmsLocationOf is projects/P/locations/L of a resource name.
func kmsLocationOf(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) < 4 {
		return name
	}
	return strings.Join(parts[:4], "/")
}

func (p kmsEkmProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	e := p.ekm()
	if len(path) != 1 || e == nil || kmsOwned(project, path[0]) != nil {
		return nil
	}
	// Edit starts from the connection's own mode and path: both are in its
	// update mask, so a form left as it opened changes nothing, where a
	// MANUAL default used to reset a CLOUD_KMS connection and clear its path.
	mode, space := "MANUAL", ""
	if c, err := e.GetEkmConnection(ctx, &kmspb.GetEkmConnectionRequest{Name: path[0]}); err == nil {
		if c.GetKeyManagementMode() == kmspb.EkmConnection_CLOUD_KMS {
			mode = "CLOUD_KMS"
		}
		space = c.GetCryptoSpacePath()
	}
	return []console.Action{
		{ID: "edit", Label: "Edit", Fields: []console.Field{
			{Name: "keyManagementMode", Label: "Key management mode", Type: "select", Default: mode, Options: []string{"MANUAL", "CLOUD_KMS"}},
			{Name: "cryptoSpacePath", Label: "Crypto space path", Type: "text", Default: space, Help: "Required when the key management mode is CLOUD_KMS."},
		}},
		{ID: "setdefault", Label: "Set as default", Confirm: "The location's EkmConfig names this connection as its default."},
	}
}

func (p kmsEkmProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	e := p.ekm()
	if e == nil {
		return errors.New(kmsNotStarted)
	}
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return err
	}
	switch action {
	case "edit":
		_, err := e.UpdateEkmConnection(ctx, &kmspb.UpdateEkmConnectionRequest{
			EkmConnection: &kmspb.EkmConnection{Name: path[0], KeyManagementMode: kmsEkmMode(values["keyManagementMode"]), CryptoSpacePath: strings.TrimSpace(values["cryptoSpacePath"])},
			UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"key_management_mode", "crypto_space_path"}}})
		return err
	case "setdefault":
		_, err := e.UpdateEkmConfig(ctx, &kmspb.UpdateEkmConfigRequest{
			EkmConfig:  &kmspb.EkmConfig{Name: kmsLocationOf(path[0]) + "/ekmConfig", DefaultEkmConnection: path[0]},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_ekm_connection"}}})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

// kmsAutokeyProvider is the Autokey screen: the AutokeyConfigs the selected
// project resolves through (its own, then its folders'), each with Edit
// configuration; the project's row lists its key handles; Create key handle.
type kmsAutokeyProvider struct{ svc *kmsService }

func (kmsAutokeyProvider) ID() string    { return "kms-autokey" }
func (kmsAutokeyProvider) Title() string { return "Cloud KMS Autokey" }

const kmsAutokeyNote = "Autokey makes an HSM ENCRYPT_DECRYPT key, rotated every 365 days, in the key ring \"autokey\" of the key project, " +
	"when a key handle is created. " + kmsProtectionNote

func (p kmsAutokeyProvider) List(ctx context.Context, project string) (console.Listing, error) {
	api := p.svc.API()
	if api == nil {
		return console.Listing{}, errors.New(kmsNotStarted)
	}
	listing := console.Listing{
		Columns:    []string{"Key project", "Resolution mode", "State"},
		NameColumn: "Autokey configuration",
		Noun:       "Autokey configurations",
	}
	if project == "" {
		listing.Prompt = "Autokey resolves per project. Choose one in the toolbar."
		return listing, nil
	}
	admin := api.AutokeyAdmin()
	eff, err := admin.ShowEffectiveAutokeyConfig(ctx, &kmspb.ShowEffectiveAutokeyConfigRequest{Parent: "projects/" + project})
	if err != nil {
		return console.Listing{}, err
	}
	effective := eff.GetKeyProject()
	if effective == "" {
		effective = "none: Autokey is off for this project"
	}
	listing.Note = "Effective key project: " + effective + ". The nearest configured row wins; the project's row lists its key handles. " + kmsAutokeyNote
	for _, name := range api.AutokeyChain(project) {
		c, err := admin.GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: name})
		if err != nil {
			return console.Listing{}, err
		}
		kp := c.GetKeyProject()
		if kp == "" {
			kp = "—"
		}
		listing.Items = append(listing.Items, console.Resource{Name: name, Status: c.GetState().String(), Fields: map[string]string{
			"Key project": kp, "Resolution mode": c.GetKeyProjectResolutionMode().String(), "State": c.GetState().String()}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	listing.AlwaysStatus = true
	return listing, nil
}

func (kmsAutokeyProvider) CreateForm() (string, []console.Field) {
	return "Create key handle", []console.Field{
		{Name: "location", Label: "Location", Type: "select", Required: true, Default: "us-central1", Options: kmsLocations()},
		{Name: "resourceTypeSelector", Label: "Resource type", Type: "text", Required: true, Default: "storage.googleapis.com/Bucket",
			Help: "{SERVICE}.googleapis.com/{TYPE}, the kind of resource the key will protect. " + kmsAutokeyNote},
		{Name: "keyHandleId", Label: "Key handle name", Type: "text", Pattern: `^[A-Za-z0-9_\-]{0,63}$`,
			Help: "Optional: a UUID when empty."},
	}
}

func (p kmsAutokeyProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	api := p.svc.API()
	if api == nil {
		return "", errors.New(kmsNotStarted)
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a key handle")
	}
	_, err := api.Autokey().CreateKeyHandle(ctx, &kmspb.CreateKeyHandleRequest{
		Parent: "projects/" + project + "/locations/" + strings.TrimSpace(values["location"]), KeyHandleId: strings.TrimSpace(values["keyHandleId"]),
		KeyHandle: &kmspb.KeyHandle{ResourceTypeSelector: strings.TrimSpace(values["resourceTypeSelector"])}})
	if err != nil {
		return "", err
	}
	// The handle is listed on the project's own row.
	return "projects/" + project + "/autokeyConfig", nil
}

func (p kmsAutokeyProvider) inChain(project, name string) bool {
	api := p.svc.API()
	if api == nil || project == "" {
		return false
	}
	for _, n := range api.AutokeyChain(project) {
		if n == name {
			return true
		}
	}
	return false
}

func (p kmsAutokeyProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	api := p.svc.API()
	if api == nil {
		return console.Detail{Unavailable: kmsNotStarted}, nil
	}
	if !p.inChain(project, path[0]) {
		return console.Detail{Unavailable: fmt.Sprintf("%s is not an Autokey configuration project %s resolves through", path[0], project)}, nil
	}
	c, err := api.AutokeyAdmin().GetAutokeyConfig(ctx, &kmspb.GetAutokeyConfigRequest{Name: path[0]})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the Autokey configuration: " + consoleErr(err)}, nil
	}
	kp := c.GetKeyProject()
	if kp == "" {
		kp = "None"
	}
	d := console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: c.GetState().String()},
			{Label: "Key project", Value: kp},
			{Label: "Resolution mode", Value: c.GetKeyProjectResolutionMode().String()},
		},
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Autokey configuration", Properties: []console.Property{
				{Label: "Resource name", Value: c.GetName()},
				{Label: "Key project", Value: kp},
				{Label: "Resolution mode", Value: c.GetKeyProjectResolutionMode().String()},
				{Label: "Etag", Value: c.GetEtag()},
			}}}, Note: kmsAutokeyNote}},
	}
	if path[0] == "projects/"+project+"/autokeyConfig" {
		handles := console.Listing{Columns: []string{"Location", "Resource type", "Key"}, NameColumn: "Key handle", Noun: "key handles"}
		hs, err := api.KeyHandlesInProject(project)
		if err != nil {
			handles.Unavailable = "cannot list key handles: " + consoleErr(err)
		}
		for _, h := range hs {
			handles.Items = append(handles.Items, console.Resource{Name: lastSegment(h.GetName()), Fields: map[string]string{
				"Location": kmsLocation(h.GetName()), "Resource type": h.GetResourceTypeSelector(), "Key": h.GetKmsKey()}})
		}
		handles.Total = len(handles.Items)
		d.Sections = append([]console.Section{{ID: "keyhandles", Label: "Key handles", Listing: handles}}, d.Sections...)
	}
	return d, nil
}

func (p kmsAutokeyProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || !p.inChain(project, path[0]) {
		return nil
	}
	return []console.Action{{ID: "editconfig", Label: "Edit configuration", Fields: []console.Field{
		{Name: "keyProjectResolutionMode", Label: "Resolution mode", Type: "select", Default: "DEDICATED_KEY_PROJECT",
			Options: []string{"DEDICATED_KEY_PROJECT", "RESOURCE_PROJECT", "DISABLED"},
			Help:    "Where keys are made: a dedicated key project, the resource's own project, or nowhere (Autokey off below here)."},
		{Name: "keyProject", Label: "Key project", Type: "text",
			Help: "projects/PROJECT, for DEDICATED_KEY_PROJECT; empty otherwise. Empty with DEDICATED_KEY_PROJECT clears the configuration."},
	}}}
}

func (p kmsAutokeyProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	api := p.svc.API()
	if api == nil {
		return errors.New(kmsNotStarted)
	}
	if action != "editconfig" || len(path) != 1 || !p.inChain(project, path[0]) {
		return fmt.Errorf("unknown action %q", action)
	}
	mode := kmspb.AutokeyConfig_KeyProjectResolutionMode(kmspb.AutokeyConfig_KeyProjectResolutionMode_value[strings.TrimSpace(values["keyProjectResolutionMode"])])
	kp := strings.TrimSpace(values["keyProject"])
	paths := []string{"key_project"}
	if mode != kmspb.AutokeyConfig_DEDICATED_KEY_PROJECT || kp != "" {
		paths = append(paths, "key_project_resolution_mode")
	}
	_, err := api.AutokeyAdmin().UpdateAutokeyConfig(ctx, &kmspb.UpdateAutokeyConfigRequest{
		AutokeyConfig: &kmspb.AutokeyConfig{Name: path[0], KeyProject: kp, KeyProjectResolutionMode: mode},
		UpdateMask:    &fieldmaskpb.FieldMask{Paths: paths}})
	return err
}

var (
	_ console.Creator   = kmsEkmProvider{}
	_ console.Driller   = kmsEkmProvider{}
	_ console.PathActor = kmsEkmProvider{}
	_ console.Creator   = kmsAutokeyProvider{}
	_ console.Driller   = kmsAutokeyProvider{}
	_ console.PathActor = kmsAutokeyProvider{}
)

// The Permissions tab on an EKM connection's page (#1469): the policy the
// connection stores, read and written through the KMS port's IAMPolicy
// mixin, the methods an SDK's EkmClient reaches. It is stored, never
// enforced (ADR-0006). A location's EkmConfig has a policy too, which has no
// page here: gcloud kms ekm-config get-iam-policy and its siblings reach it
// from the console's terminal.

func (kmsEkmProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) != 1 {
		return nil
	}
	return &console.PolicyTarget{Link: compatibilityDoc + "cloud-kms--googlecloudkmsv1",
		RoleHelp: roleHelp("Cloud KMS", "roles/cloudkms.ekmConnectionsAdmin")}
}

func (p kmsEkmProvider) ekmIAM(project string, path []string) (iamServer, string, error) {
	api := p.svc.API()
	if api == nil {
		return nil, "", errors.New(kmsNotStarted)
	}
	if len(path) != 1 {
		return nil, "", fmt.Errorf("no IAM policy at %s", strings.Join(path, "/"))
	}
	if err := kmsOwned(project, path[0]); err != nil {
		return nil, "", err
	}
	return api.IAMPolicy(), path[0], nil
}

func (p kmsEkmProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	api, name, err := p.ekmIAM(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p kmsEkmProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	api, name, err := p.ekmIAM(project, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

func (p kmsEkmProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	api, name, err := p.ekmIAM(project, path)
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, api, name, perms)
}
