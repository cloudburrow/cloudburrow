package main

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	pubsubadmin "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/kmsinventory"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// The Cloud KMS Inventory API's sources (#2053): what each served product
// protects with a customer-managed key, read through the product's own API
// (the calls an SDK makes) at each Inventory call, so the usage is real.

// inventoryTimeout bounds one product's answer.
const inventoryTimeout = 20 * time.Second

// inventoryMaxObjects bounds the objects one bucket's key versions are read
// from; Google documents a bucket's usage as possibly incomplete past 4,000
// key versions.
const inventoryMaxObjects = 10000

// kmsInventoryWiring is what the sources reach.
type kmsInventoryWiring struct {
	cfg      config.Config
	storage  func() *netfwd.Forwarder
	bigquery func() *netfwd.Forwarder
	pubsub   func() *netfwd.Forwarder
	secrets  *secretsService
}

func fwdAddr(f func() *netfwd.Forwarder) string {
	if f == nil {
		return ""
	}
	if fw := f(); fw != nil {
		return fw.HostAddr()
	}
	return ""
}

// sources are the products served now.
func (w kmsInventoryWiring) sources() []kmsinventory.Source {
	var out []kmsinventory.Source
	if serviceEnabled(w.cfg, config.ServiceStorage) {
		out = append(out, w.storageSource)
	}
	if serviceEnabled(w.cfg, config.ServiceBigQuery) {
		out = append(out, w.bigquerySource)
	}
	if serviceEnabled(w.cfg, config.ServicePubSub) {
		out = append(out, w.pubsubSource)
	}
	if w.secrets != nil {
		out = append(out, w.secretsSource)
	}
	return out
}

// cryptoKeyOf is a key or version name's CryptoKey.
func cryptoKeyOf(name string) string {
	if i := strings.Index(name, "/cryptoKeyVersions/"); i >= 0 {
		return name[:i]
	}
	return name
}

// storageSource is the project's buckets with a default key or objects
// written with one, rolled up to the bucket as Google documents.
func (w kmsInventoryWiring) storageSource(ctx context.Context, project string) ([]kmsinventory.Resource, error) {
	addr := fwdAddr(w.storage)
	if addr == "" {
		return nil, errors.New("Cloud Storage is not reachable")
	}
	ctx, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()
	c, err := storageProvider{endpoint: addr}.storageClient(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var out []kmsinventory.Resource
	it := c.Buckets(ctx, project)
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return out, err
		}
		keys := map[string]bool{}
		if b.Encryption != nil && b.Encryption.DefaultKMSKeyName != "" {
			keys[cryptoKeyOf(b.Encryption.DefaultKMSKeyName)] = true
		}
		versions := map[string]bool{}
		q := &storage.Query{}
		_ = q.SetAttrSelection([]string{"Name", "KMSKeyName"})
		oit := c.Bucket(b.Name).Objects(ctx, q)
		for n := 0; n < inventoryMaxObjects; n++ {
			o, err := oit.Next()
			if err != nil {
				break
			}
			if o.KMSKeyName != "" {
				keys[cryptoKeyOf(o.KMSKeyName)] = true
				if strings.Contains(o.KMSKeyName, "/cryptoKeyVersions/") {
					versions[o.KMSKeyName] = true
				}
			}
		}
		if len(keys) == 0 {
			continue
		}
		out = append(out, kmsinventory.Resource{Name: "//storage.googleapis.com/" + b.Name, ProjectID: project,
			CloudProduct: "storage", ResourceType: "storage.googleapis.com/Bucket", Location: strings.ToLower(b.Location),
			Labels: b.Labels, Keys: inventorySet(keys), Versions: inventorySet(versions), Created: b.Created})
	}
	return out, nil
}

// bigquerySource is the project's datasets with a default key and tables
// with a key. The engine serves the instance's default project only.
func (w kmsInventoryWiring) bigquerySource(ctx context.Context, project string) ([]kmsinventory.Resource, error) {
	addr := fwdAddr(w.bigquery)
	if addr == "" {
		return nil, errors.New("BigQuery is not reachable")
	}
	if project != w.cfg.DefaultProject() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()
	c, err := bigqueryProvider{endpoint: addr, project: project}.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var out []kmsinventory.Resource
	dit := c.Datasets(ctx)
	for {
		ds, err := dit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return out, err
		}
		md, err := ds.Metadata(ctx)
		if err != nil {
			continue
		}
		base := "//bigquery.googleapis.com/projects/" + project + "/datasets/" + ds.DatasetID
		if e := md.DefaultEncryptionConfig; e != nil && e.KMSKeyName != "" {
			out = append(out, kmsinventory.Resource{Name: base, ProjectID: project, CloudProduct: "bigquery",
				ResourceType: "bigquery.googleapis.com/Dataset", Location: strings.ToLower(md.Location), Labels: md.Labels,
				Keys: []string{cryptoKeyOf(e.KMSKeyName)}, Created: md.CreationTime})
		}
		tit := ds.Tables(ctx)
		for {
			t, err := tit.Next()
			if err != nil {
				break
			}
			tm, err := t.Metadata(ctx)
			if err != nil || tm.EncryptionConfig == nil || tm.EncryptionConfig.KMSKeyName == "" {
				continue
			}
			typ := "bigquery.googleapis.com/Table"
			out = append(out, kmsinventory.Resource{Name: base + "/tables/" + t.TableID, ProjectID: project, CloudProduct: "bigquery",
				ResourceType: typ, Location: strings.ToLower(tm.Location), Labels: tm.Labels,
				Keys: []string{cryptoKeyOf(tm.EncryptionConfig.KMSKeyName)}, Created: tm.CreationTime})
		}
	}
	return out, nil
}

// pubsubSource is the project's topics with a key.
func (w kmsInventoryWiring) pubsubSource(ctx context.Context, project string) ([]kmsinventory.Resource, error) {
	addr := fwdAddr(w.pubsub)
	if addr == "" {
		return nil, errors.New("Pub/Sub is not reachable")
	}
	ctx, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()
	c, err := pubsubadmin.NewTopicAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var out []kmsinventory.Resource
	it := c.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project})
	for {
		t, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return out, err
		}
		if t.GetKmsKeyName() == "" {
			continue
		}
		out = append(out, kmsinventory.Resource{Name: "//pubsub.googleapis.com/" + t.GetName(), ProjectID: project,
			CloudProduct: "pubsub", ResourceType: "pubsub.googleapis.com/Topic", Location: "global", Labels: t.GetLabels(),
			Keys: []string{cryptoKeyOf(t.GetKmsKeyName())}})
	}
	return out, nil
}

// secretsSource is the project's secrets with a key.
func (w kmsInventoryWiring) secretsSource(_ context.Context, project string) ([]kmsinventory.Resource, error) {
	st := w.secrets.Store()
	if st == nil {
		return nil, errors.New("Secret Manager has not started")
	}
	secrets, err := st.ListSecrets(project)
	if err != nil {
		return nil, err
	}
	var out []kmsinventory.Resource
	for _, sec := range secrets {
		keys := map[string]bool{}
		for _, k := range sec.KMSKeys {
			if k != "" {
				keys[cryptoKeyOf(k)] = true
			}
		}
		if len(keys) == 0 {
			continue
		}
		name := sec.Name
		if !strings.HasPrefix(name, "projects/") {
			name = "projects/" + project + "/secrets/" + name
		}
		out = append(out, kmsinventory.Resource{Name: "//secretmanager.googleapis.com/" + name, ProjectID: project,
			CloudProduct: "secretmanager", ResourceType: "secretmanager.googleapis.com/Secret", Location: "global",
			Labels: sec.Labels, Keys: inventorySet(keys), Created: sec.Created})
	}
	return out, nil
}

func inventorySet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inventoryProjects is kmsinventory.Projects over Resource Manager.
type inventoryProjects struct{ reg *resourcemanager.Registry }

func (p inventoryProjects) Resolve(idOrNumber string) (string, bool) {
	if _, err := p.reg.Get(idOrNumber); err == nil {
		return idOrNumber, true
	}
	if _, err := strconv.ParseInt(idOrNumber, 10, 64); err != nil {
		return "", false
	}
	all, err := p.reg.List()
	if err != nil {
		return "", false
	}
	for _, pr := range all {
		if n, ok := p.reg.NumberOf(pr.ProjectID); ok && strconv.FormatInt(n, 10) == idOrNumber {
			return pr.ProjectID, true
		}
	}
	return "", false
}

func (p inventoryProjects) Organization(projectID string) string {
	chain, err := p.reg.Ancestry(projectID)
	if err != nil || len(chain) == 0 {
		return ""
	}
	if last := chain[len(chain)-1]; last.Type == "organization" {
		return last.ID
	}
	return ""
}

func (p inventoryProjects) InOrganization(number string) []string {
	all, err := p.reg.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, pr := range all {
		if pr.State == resourcemanager.StateActive && p.Organization(pr.ProjectID) == number {
			out = append(out, pr.ProjectID)
		}
	}
	return out
}

// organizationOf is the number of project's organization, "" for none.
func (s *kmsService) organizationOf(project string) string {
	if s == nil || s.hierarchy == nil {
		return ""
	}
	return inventoryProjects{reg: s.hierarchy}.Organization(project)
}
