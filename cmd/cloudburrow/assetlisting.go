package main

import (
	"context"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// Cloud Asset's IAM policy analysis under a scope (#2112) enumerates every
// served resource that keeps an allow policy. The in-process services list
// theirs through the enforcer's Resources (iamenforce*.go); the in-cluster
// products are asked here, through their own APIs at the instance's
// forwarders, by the full resource names and asset types Cloud Asset
// Inventory documents
// (https://docs.cloud.google.com/asset-inventory/docs/supported-asset-types,
// read 2026-11-21):
//
//   - pubsub.googleapis.com/Topic, Subscription, Snapshot and Schema;
//   - bigquery.googleapis.com/Dataset and Table (a table under its
//     dataset); a dataset's policy is its access entries, the basic roles
//     READER, WRITER and OWNER read as roles/bigquery.dataViewer,
//     dataEditor and dataOwner, as BigQuery documents;
//   - spanner.googleapis.com/Instance, Database and Backup (under their
//     instance);
//   - bigtableadmin.googleapis.com/Instance and Table (under its instance).
//
// Cloud SQL instances keep no resource-level allow policy, and Analytics
// Hub's data exchanges and listings are "not available in the analysis
// APIs", so neither is listed.

// assetRemote is one in-cluster product's resources and policies.
type assetRemote struct {
	// forwarder names the product's forwarder ("pubsub", ...).
	forwarder string
	list      func(ctx context.Context, o *assetOutputs, addr, project string) []iameval.ChildResource
	policy    func(ctx context.Context, o *assetOutputs, addr, name string) *iampolicy.Stored
}

// assetRemoteTimeout bounds one product's listing.
const assetRemoteTimeout = 20 * time.Second

// assetRemotes are the in-cluster products, by service host.
var assetRemotes = map[string]assetRemote{
	"pubsub.googleapis.com":        {forwarder: "pubsub", list: pubsubAssets, policy: iamPolicyOver},
	"bigquery.googleapis.com":      {forwarder: "bigquery", list: bigqueryAssets, policy: bigqueryAssetPolicy},
	"spanner.googleapis.com":       {forwarder: "spanner", list: spannerAssets, policy: spannerAssetPolicy},
	"bigtableadmin.googleapis.com": {forwarder: "bigtable", list: bigtableAssets, policy: bigtableAssetPolicy},
}

// remoteAddr is a product's forwarder address, "" when it is not served.
func (o *assetOutputs) remoteAddr(name string) string {
	if o == nil || o.forwarded == nil {
		return ""
	}
	return o.forwarded(name)
}

// conn is a gRPC connection to addr, made once.
func (o *assetOutputs) conn(addr string) (*grpc.ClientConn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c := o.conns[addr]; c != nil {
		return c, nil
	}
	c, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	if o.conns == nil {
		o.conns = map[string]*grpc.ClientConn{}
	}
	o.conns[addr] = c
	return c, nil
}

// remoteResources are a project's resources in every in-cluster product
// served, by full resource name.
func (o *assetOutputs) remoteResources(ctx context.Context, project string, hosts []string) []analysisChild {
	var out []analysisChild
	for _, h := range hosts {
		r := assetRemotes[h]
		addr := o.remoteAddr(r.forwarder)
		if addr == "" {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, assetRemoteTimeout)
		for _, c := range r.list(lctx, o, addr, project) {
			ch := analysisChild{full: "//" + h + "/" + c.Name}
			if c.Parent != "" {
				ch.parent = "//" + h + "/" + c.Parent
			}
			out = append(out, ch)
		}
		cancel()
	}
	return out
}

// remotePolicy is an in-cluster resource's own policy; ok reports whether
// the host is one of them.
func (o *assetOutputs) remotePolicy(ctx context.Context, host, name string) (p *iampolicy.Stored, ok bool) {
	r, ok := assetRemotes[host]
	if !ok {
		return nil, false
	}
	addr := o.remoteAddr(r.forwarder)
	if addr == "" {
		return nil, true
	}
	return r.policy(ctx, o, addr, name), true
}

// remoteParents are the in-cluster resource's own name and its parents
// within the product that keep policies, the deepest first: a table's
// dataset, a database's instance.
func remoteParents(name string) []string {
	parts := strings.Split(name, "/")
	var out []string
	for n := len(parts); n >= 4; n -= 2 {
		out = append(out, strings.Join(parts[:n], "/"))
	}
	return out
}

func grpcPolicy(ctx context.Context, get func(context.Context, *iampb.GetIamPolicyRequest, ...grpc.CallOption) (*iampb.Policy, error), name string) *iampolicy.Stored {
	p, err := get(ctx, &iampb.GetIamPolicyRequest{Resource: name, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil {
		return nil
	}
	return iameval.FromProto(p)
}

// ---- Pub/Sub ----

func pubsubAssets(ctx context.Context, o *assetOutputs, addr, project string) []iameval.ChildResource {
	c, err := o.conn(addr)
	if err != nil {
		return nil
	}
	parent := "projects/" + project
	var out []iameval.ChildResource
	add := func(n string) { out = append(out, iameval.ChildResource{Name: n}) }
	pub, sub, sch := pubsubpb.NewPublisherClient(c), pubsubpb.NewSubscriberClient(c), pubsubpb.NewSchemaServiceClient(c)
	for tok, i := "", 0; i < 1000; i++ {
		r, err := pub.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: parent, PageSize: 1000, PageToken: tok})
		if err != nil {
			break
		}
		for _, t := range r.GetTopics() {
			add(t.GetName())
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	for tok, i := "", 0; i < 1000; i++ {
		r, err := sub.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: parent, PageSize: 1000, PageToken: tok})
		if err != nil {
			break
		}
		for _, s := range r.GetSubscriptions() {
			add(s.GetName())
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	for tok, i := "", 0; i < 1000; i++ {
		r, err := sub.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: parent, PageSize: 1000, PageToken: tok})
		if err != nil {
			break
		}
		for _, s := range r.GetSnapshots() {
			add(s.GetName())
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	for tok, i := "", 0; i < 1000; i++ {
		r, err := sch.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: parent, PageSize: 1000, PageToken: tok})
		if err != nil {
			break
		}
		for _, s := range r.GetSchemas() {
			add(s.GetName())
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	return out
}

// iamPolicyOver reads a policy through google.iam.v1.IAMPolicy at addr.
func iamPolicyOver(ctx context.Context, o *assetOutputs, addr, name string) *iampolicy.Stored {
	c, err := o.conn(addr)
	if err != nil {
		return nil
	}
	return grpcPolicy(ctx, iampb.NewIAMPolicyClient(c).GetIamPolicy, name)
}

// ---- BigQuery ----

func (o *assetOutputs) bigqueryClient(ctx context.Context, addr, project string) (*bigquery.Client, error) {
	return bigqueryProvider{endpoint: addr, project: project}.client(ctx)
}

func bigqueryAssets(ctx context.Context, o *assetOutputs, addr, project string) []iameval.ChildResource {
	c, err := o.bigqueryClient(ctx, addr, project)
	if err != nil {
		return nil
	}
	defer c.Close()
	var out []iameval.ChildResource
	dit := c.Datasets(ctx)
	for len(out) < 100000 {
		ds, err := dit.Next()
		if err != nil {
			break
		}
		dn := "projects/" + project + "/datasets/" + ds.DatasetID
		out = append(out, iameval.ChildResource{Name: dn})
		tit := ds.Tables(ctx)
		for {
			t, err := tit.Next()
			if err == iterator.Done || err != nil {
				break
			}
			out = append(out, iameval.ChildResource{Name: dn + "/tables/" + t.TableID, Parent: dn})
		}
	}
	return out
}

// bigqueryBasicRoles are a dataset access entry's basic roles as the IAM
// roles BigQuery documents them as.
var bigqueryBasicRoles = map[bigquery.AccessRole]string{
	bigquery.ReaderRole: "roles/bigquery.dataViewer",
	bigquery.WriterRole: "roles/bigquery.dataEditor",
	bigquery.OwnerRole:  "roles/bigquery.dataOwner",
}

func bigqueryAssetPolicy(ctx context.Context, o *assetOutputs, addr, name string) *iampolicy.Stored {
	parts := strings.Split(name, "/")
	if len(parts) < 4 || parts[0] != "projects" || parts[2] != "datasets" {
		return nil
	}
	c, err := o.bigqueryClient(ctx, addr, parts[1])
	if err != nil {
		return nil
	}
	defer c.Close()
	ds := c.Dataset(parts[3])
	switch {
	case len(parts) == 6 && parts[4] == "tables":
		p, err := ds.Table(parts[5]).IAM().Policy(ctx)
		if err != nil {
			return nil
		}
		return iameval.FromProto(p.InternalProto)
	case len(parts) != 4:
		return nil
	}
	md, err := ds.Metadata(ctx)
	if err != nil {
		return nil
	}
	return bigqueryAccessPolicy(parts[1], md.Access)
}

// bigqueryAccessPolicy is a dataset's access entries as an allow policy.
func bigqueryAccessPolicy(project string, access []*bigquery.AccessEntry) *iampolicy.Stored {
	byRole := map[string][]string{}
	var roles []string
	for _, a := range access {
		role := bigqueryBasicRoles[a.Role]
		if role == "" {
			role = string(a.Role)
		}
		member := ""
		switch a.EntityType {
		case bigquery.UserEmailEntity:
			member = "user:" + a.Entity
			if strings.HasSuffix(a.Entity, ".gserviceaccount.com") {
				member = "serviceAccount:" + a.Entity
			}
		case bigquery.GroupEmailEntity:
			member = "group:" + a.Entity
		case bigquery.DomainEntity:
			member = "domain:" + a.Entity
		case bigquery.IAMMemberEntity:
			member = a.Entity
		case bigquery.SpecialGroupEntity:
			member = map[string]string{"projectOwners": "projectOwner:" + project, "projectWriters": "projectEditor:" + project,
				"projectReaders": "projectViewer:" + project, "allAuthenticatedUsers": "allAuthenticatedUsers"}[a.Entity]
		}
		if member == "" || !strings.HasPrefix(role, "roles/") {
			continue
		}
		if _, ok := byRole[role]; !ok {
			roles = append(roles, role)
		}
		byRole[role] = append(byRole[role], member)
	}
	p := &iampolicy.Stored{}
	for _, r := range roles {
		p.Bindings = append(p.Bindings, iampolicy.Binding{Role: r, Members: byRole[r]})
	}
	return p
}

// ---- Spanner ----

func spannerAssets(ctx context.Context, o *assetOutputs, addr, project string) []iameval.ChildResource {
	c, err := o.conn(addr)
	if err != nil {
		return nil
	}
	ia, da := instancepb.NewInstanceAdminClient(c), databasepb.NewDatabaseAdminClient(c)
	var out []iameval.ChildResource
	for tok, i := "", 0; i < 100; i++ {
		r, err := ia.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: "projects/" + project, PageToken: tok})
		if err != nil {
			break
		}
		for _, in := range r.GetInstances() {
			out = append(out, iameval.ChildResource{Name: in.GetName()})
			for dtok, j := "", 0; j < 100; j++ {
				d, err := da.ListDatabases(ctx, &databasepb.ListDatabasesRequest{Parent: in.GetName(), PageToken: dtok})
				if err != nil {
					break
				}
				for _, db := range d.GetDatabases() {
					out = append(out, iameval.ChildResource{Name: db.GetName(), Parent: in.GetName()})
				}
				if dtok = d.GetNextPageToken(); dtok == "" {
					break
				}
			}
			for btok, j := "", 0; j < 100; j++ {
				b, err := da.ListBackups(ctx, &databasepb.ListBackupsRequest{Parent: in.GetName(), PageToken: btok})
				if err != nil {
					break
				}
				for _, bk := range b.GetBackups() {
					out = append(out, iameval.ChildResource{Name: bk.GetName(), Parent: in.GetName()})
				}
				if btok = b.GetNextPageToken(); btok == "" {
					break
				}
			}
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	return out
}

func spannerAssetPolicy(ctx context.Context, o *assetOutputs, addr, name string) *iampolicy.Stored {
	c, err := o.conn(addr)
	if err != nil {
		return nil
	}
	if strings.Contains(name, "/databases/") || strings.Contains(name, "/backups/") {
		return grpcPolicy(ctx, databasepb.NewDatabaseAdminClient(c).GetIamPolicy, name)
	}
	return grpcPolicy(ctx, instancepb.NewInstanceAdminClient(c).GetIamPolicy, name)
}

// ---- Bigtable ----

func bigtableAssets(ctx context.Context, o *assetOutputs, addr, project string) []iameval.ChildResource {
	c, err := o.conn(addr)
	if err != nil {
		return nil
	}
	ia, ta := btapb.NewBigtableInstanceAdminClient(c), btapb.NewBigtableTableAdminClient(c)
	r, err := ia.ListInstances(ctx, &btapb.ListInstancesRequest{Parent: "projects/" + project})
	if err != nil {
		return nil
	}
	var out []iameval.ChildResource
	for _, in := range r.GetInstances() {
		out = append(out, iameval.ChildResource{Name: in.GetName()})
		for tok, j := "", 0; j < 100; j++ {
			t, err := ta.ListTables(ctx, &btapb.ListTablesRequest{Parent: in.GetName(), View: btapb.Table_NAME_ONLY, PageToken: tok})
			if err != nil {
				break
			}
			for _, tb := range t.GetTables() {
				out = append(out, iameval.ChildResource{Name: tb.GetName(), Parent: in.GetName()})
			}
			if tok = t.GetNextPageToken(); tok == "" {
				break
			}
		}
	}
	return out
}

func bigtableAssetPolicy(ctx context.Context, o *assetOutputs, addr, name string) *iampolicy.Stored {
	c, err := o.conn(addr)
	if err != nil {
		return nil
	}
	if strings.Contains(name, "/tables/") {
		return grpcPolicy(ctx, btapb.NewBigtableTableAdminClient(c).GetIamPolicy, name)
	}
	return grpcPolicy(ctx, btapb.NewBigtableInstanceAdminClient(c).GetIamPolicy, name)
}
