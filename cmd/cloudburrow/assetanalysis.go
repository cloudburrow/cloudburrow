package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	asset "google.golang.org/api/cloudasset/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Cloud Asset Inventory's IAM policy analysis (#2112), on the Resource
// Manager port: analyzeIamPolicy with every analysisQuery option over the
// policies of the scope and its descendants (internal/iameval's Analyzer),
// analyzeIamPolicyLongrunning writing to the instance's Cloud Storage or
// BigQuery, its operations, and saved queries. The references read
// 2026-11-21: https://docs.cloud.google.com/asset-inventory/docs/reference/rest/v1/TopLevel/analyzeIamPolicyLongrunning,
// https://docs.cloud.google.com/asset-inventory/docs/reference/rest/v1/savedQueries,
// https://docs.cloud.google.com/asset-inventory/docs/analyzing-iam-policy-longrunning-bigquery.
//
// The resources under a project are every served resource that keeps an
// allow policy and that Cloud Asset Inventory's supported asset types page
// lists as available in the analysis APIs: its service accounts, its
// Cloud Storage buckets, the in-process services' (Enforcer.Resources:
// Secret Manager's secrets, Cloud KMS's key rings, keys, import jobs and
// EKM connections, Cloud Tasks's queues, Cloud Run's services and jobs,
// Cloud Run functions, Artifact Registry repositories, Certificate
// Authority Service's CA pools, certificate templates and CRLs, Compute
// Engine's instances, disks, snapshots and subnetworks) and the
// in-cluster products' (assetlisting.go: Pub/Sub, BigQuery, Spanner,
// Bigtable), by Cloud Asset Inventory's full resource names; under a
// project or the organization, the tag keys it parents, and under a key
// its values. Cloud Run worker pools, Logging log views, Secure Source
// Manager instances and workforce and workload identity pools are "Not
// available in the analysis APIs", and Secure Source Manager repositories
// and Container Analysis notes are not asset types, so none is listed.
//
// The BigQuery tables' columns are the API's JSON field names, as the
// page's example queries read them (opName, requestTime, analysisId,
// analysis.analysisQuery, analysisResult.identityList.identities.name, ...):
// Google publishes no column list beyond those
// (UNVERIFIED, docs re-read 2026-11-21:
// https://docs.cloud.google.com/asset-inventory/docs/analyzing-iam-policy-longrunning-bigquery).

// assetOutputs are the destinations analyzeIamPolicyLongrunning writes to.
type assetOutputs struct {
	bigquery func() *netfwd.Forwarder
	// storage is the instance's Cloud Storage base URL, "" when it is not
	// served.
	storage func() string
	// forwarded is an in-cluster product's forwarder address ("pubsub",
	// "bigquery", "spanner", "bigtable"), "" when it is not served.
	forwarded func(name string) string
	mu        sync.Mutex
	conns     map[string]*grpc.ClientConn
}

// storageBuckets are a project's buckets, read through the instance's
// Cloud Storage JSON API, for the analysis's resources under a project.
func (o *assetOutputs) storageBuckets(ctx context.Context, project string) []iameval.ChildResource {
	if o == nil || o.storage == nil || o.storage() == "" {
		return nil
	}
	var out []iameval.ChildResource
	token := ""
	for i := 0; i < 100; i++ {
		u := o.storage() + "/storage/v1/b?maxResults=1000&project=" + url.QueryEscape(project)
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		var page struct {
			Items         []struct{ Name string } `json:"items"`
			NextPageToken string                  `json:"nextPageToken"`
		}
		if !assetGetJSON(ctx, u, &page) {
			return out
		}
		for _, b := range page.Items {
			out = append(out, iameval.ChildResource{Name: "projects/_/buckets/" + b.Name})
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	return out
}

// storagePolicy is a bucket's IAM policy, read through the instance's
// Cloud Storage JSON API.
func (o *assetOutputs) storagePolicy(ctx context.Context, name string) *iampolicy.Stored {
	bucket, ok := strings.CutPrefix(name, "projects/_/buckets/")
	if o == nil || o.storage == nil || o.storage() == "" || !ok || strings.Contains(bucket, "/") {
		return nil
	}
	var pol struct {
		Bindings []iampolicy.Binding `json:"bindings"`
	}
	if !assetGetJSON(ctx, o.storage()+"/storage/v1/b/"+url.PathEscape(bucket)+"/iam?optionsRequestedPolicyVersion=3", &pol) {
		return nil
	}
	return &iampolicy.Stored{Bindings: pol.Bindings}
}

func assetGetJSON(ctx context.Context, u string, out any) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out) == nil
}

const (
	longrunningSuffix = ":analyzeIamPolicyLongrunning"
	analyzeSuffix     = ":analyzeIamPolicy"
	assetOpMarker     = "/operations/AnalyzeIamPolicyLongrunning/"
	savedQueriesPart  = "/savedQueries"
	savedQueryKey     = "cloudasset/savedqueries/"
	assetOpKey        = "cloudasset/operations/"
)

var savedQueryID = regexp.MustCompile(`^[a-z0-9-]{4,63}$`)

// isAssetPath is whether r is one of Cloud Asset's methods served here.
func isAssetPath(r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/v1/") {
		return false
	}
	return strings.HasSuffix(p, analyzeSuffix) || strings.HasSuffix(p, longrunningSuffix) ||
		strings.Contains(p, assetOpMarker) || strings.Contains(p, savedQueriesPart)
}

func (a iamAnalysis) serveAsset(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	var err error
	var out any
	switch {
	case strings.HasSuffix(p, analyzeSuffix) && r.Method == http.MethodGet:
		out, err = a.analyzeHTTP(r, strings.TrimSuffix(p, analyzeSuffix))
	case strings.HasSuffix(p, longrunningSuffix) && r.Method == http.MethodPost:
		out, err = a.longrunning(r, strings.TrimSuffix(p, longrunningSuffix))
	case strings.Contains(p, assetOpMarker) && r.Method == http.MethodGet:
		out, err = a.operation(p)
	case strings.Contains(p, savedQueriesPart):
		out, err = a.savedQueries(r, p)
	default:
		err = apierror.NotFound("%s %s is not served", r.Method, r.URL.Path)
	}
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	if m, ok := out.(proto.Message); ok {
		b, err := protojson.Marshal(m)
		if err != nil {
			apierror.WriteJSON(w, apierror.Internal(err, "encode the response"))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(b)
		return
	}
	writeIAMJSON(w, out)
}

// store is the registry's backing store, which keeps saved queries and
// operations.
func (a iamAnalysis) store() (store.Store, error) {
	reg := a.registry()
	if reg == nil {
		return nil, assetUnavailable("Resource Manager has not started on this instance")
	}
	return reg.Backing(), nil
}

func (a iamAnalysis) registry() *resourcemanager.Registry {
	if a.reg == nil {
		return nil
	}
	return a.reg()
}

// canonicalParent is a scope or saved queries' parent by number:
// projects/{number}, folders/{number} or organizations/{number}, after
// checking the instance holds it.
func (a iamAnalysis) canonicalParent(parent string) (string, error) {
	reg := a.registry()
	kind, id, ok := strings.Cut(parent, "/")
	if !ok || id == "" || strings.Contains(id, "/") || reg == nil {
		return "", apierror.InvalidArgument("Invalid parent %q: it must be projects/{PROJECT_ID}, projects/{PROJECT_NUMBER}, folders/{FOLDER_NUMBER} or organizations/{ORGANIZATION_NUMBER}.", parent)
	}
	switch kind {
	case "projects":
		pid := registrySource{reg: a.reg}.projectID(reg, id)
		p, err := reg.Get(pid)
		if err != nil || p.State != resourcemanager.StateActive {
			return "", assetDenied("The caller does not have permission, or %s does not exist.", parent)
		}
		return "projects/" + strconv.FormatInt(resource.ProjectNumber(pid), 10), nil
	case "folders":
		f, err := reg.Folder(parent)
		if err != nil || f.State != resourcemanager.StateActive {
			return "", assetDenied("The caller does not have permission, or %s does not exist.", parent)
		}
		return parent, nil
	case "organizations":
		if parent != resourcemanager.OrganizationName {
			return "", assetDenied("The caller does not have permission, or %s does not exist.", parent)
		}
		return parent, nil
	}
	return "", apierror.InvalidArgument("Invalid parent %q: it must be projects/{PROJECT_ID}, projects/{PROJECT_NUMBER}, folders/{FOLDER_NUMBER} or organizations/{ORGANIZATION_NUMBER}.", parent)
}

// ---- analyzeIamPolicy ----

// queryFromURL is analyzeIamPolicy's analysisQuery from its query
// parameters.
func queryFromURL(v url.Values) (*assetpb.IamPolicyAnalysisQuery, error) {
	q := &assetpb.IamPolicyAnalysisQuery{}
	if s := v.Get("analysisQuery.resourceSelector.fullResourceName"); s != "" {
		q.ResourceSelector = &assetpb.IamPolicyAnalysisQuery_ResourceSelector{FullResourceName: s}
	}
	if s := strings.TrimSpace(v.Get("analysisQuery.identitySelector.identity")); s != "" {
		q.IdentitySelector = &assetpb.IamPolicyAnalysisQuery_IdentitySelector{Identity: s}
	}
	perms, roles := v["analysisQuery.accessSelector.permissions"], v["analysisQuery.accessSelector.roles"]
	if len(perms) > 0 || len(roles) > 0 {
		q.AccessSelector = &assetpb.IamPolicyAnalysisQuery_AccessSelector{Permissions: perms, Roles: roles}
	}
	opts := &assetpb.IamPolicyAnalysisQuery_Options{}
	for name, set := range map[string]*bool{
		"expandGroups": &opts.ExpandGroups, "expandRoles": &opts.ExpandRoles, "expandResources": &opts.ExpandResources,
		"outputResourceEdges": &opts.OutputResourceEdges, "outputGroupEdges": &opts.OutputGroupEdges,
		"analyzeServiceAccountImpersonation": &opts.AnalyzeServiceAccountImpersonation,
	} {
		s := v.Get("analysisQuery.options." + name)
		if s == "" {
			continue
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, apierror.InvalidArgument("Invalid value at 'analysis_query.options.%s' (TYPE_BOOL), %q", name, s)
		}
		*set = b
	}
	if proto.Size(opts) > 0 {
		q.Options = opts
	}
	if s := v.Get("analysisQuery.conditionContext.accessTime"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, apierror.InvalidArgument("Invalid value at 'analysis_query.condition_context.access_time' (type.googleapis.com/google.protobuf.Timestamp), Field 'accessTime', Invalid time format: %v", err)
		}
		q.ConditionContext = &assetpb.IamPolicyAnalysisQuery_ConditionContext{
			TimeContext: &assetpb.IamPolicyAnalysisQuery_ConditionContext_AccessTime{AccessTime: timestamppb.New(t)}}
	}
	return q, nil
}

// mergeQuery is a saved query with q's overrides: "the saved_analysis_query
// as base and the analysis_query as overrides", where a field at its
// default value overrides nothing.
func mergeQuery(base, q *assetpb.IamPolicyAnalysisQuery) *assetpb.IamPolicyAnalysisQuery {
	out := proto.Clone(base).(*assetpb.IamPolicyAnalysisQuery)
	if q.GetScope() != "" {
		out.Scope = q.GetScope()
	}
	if q.ResourceSelector != nil {
		out.ResourceSelector = q.ResourceSelector
	}
	if q.IdentitySelector != nil {
		out.IdentitySelector = q.IdentitySelector
	}
	if q.AccessSelector != nil {
		out.AccessSelector = q.AccessSelector
	}
	if q.ConditionContext != nil {
		out.ConditionContext = q.ConditionContext
	}
	if o := q.GetOptions(); o != nil {
		if out.Options == nil {
			out.Options = &assetpb.IamPolicyAnalysisQuery_Options{}
		}
		proto.Merge(out.Options, o)
	}
	return out
}

// resolveQuery is the query to run: q merged over the saved query named
// saved, if any, at scope.
func (a iamAnalysis) resolveQuery(scope, saved string, q *assetpb.IamPolicyAnalysisQuery, now time.Time) (iameval.AnalysisQuery, error) {
	if q == nil {
		q = &assetpb.IamPolicyAnalysisQuery{}
	}
	if saved != "" {
		sq, err := a.getSavedQuery(saved)
		if err != nil {
			return iameval.AnalysisQuery{}, err
		}
		base := sq.GetContent().GetIamPolicyAnalysisQuery()
		if base == nil {
			return iameval.AnalysisQuery{}, apierror.InvalidArgument("The saved query %s does not hold an IAM policy analysis query.", saved)
		}
		q = mergeQuery(base, q)
	}
	if scope != "" {
		q.Scope = scope
	}
	out := iameval.AnalysisQuery{Scope: q.GetScope(), Resource: q.GetResourceSelector().GetFullResourceName(),
		Identity: q.GetIdentitySelector().GetIdentity(), Permissions: q.GetAccessSelector().GetPermissions(),
		Roles: q.GetAccessSelector().GetRoles()}
	if o := q.GetOptions(); o != nil {
		out.Options = iameval.AnalysisOptions{ExpandGroups: o.GetExpandGroups(), ExpandRoles: o.GetExpandRoles(),
			ExpandResources: o.GetExpandResources(), OutputResourceEdges: o.GetOutputResourceEdges(),
			OutputGroupEdges: o.GetOutputGroupEdges(), AnalyzeServiceAccountImpersonation: o.GetAnalyzeServiceAccountImpersonation()}
	}
	if at := q.GetConditionContext().GetAccessTime(); at != nil {
		out.AccessTime = at.AsTime()
		if out.AccessTime.Before(now) {
			// "This time must be in the future."
			return out, apierror.InvalidArgument("analysis_query.condition_context.access_time %s must not be earlier than the current time.",
				out.AccessTime.UTC().Format(time.RFC3339))
		}
	}
	return out, out.Validate()
}

// run answers q with the given per-resource expansion limit.
func (a iamAnalysis) run(ctx context.Context, q iameval.AnalysisQuery, limit int) (*asset.AnalyzeIamPolicyResponse, error) {
	if a.e == nil || a.e.Eval == nil {
		return nil, assetUnavailable("IAM evaluation is not available on this instance")
	}
	an := &iameval.Analyzer{Eval: a.e.Eval, Inv: newAnalysisInventory(ctx, a), ResourceLimit: limit}
	main, derived, err := an.Analyze(q)
	if err != nil {
		return nil, err
	}
	out := &asset.AnalyzeIamPolicyResponse{MainAnalysis: main, ServiceAccountImpersonationAnalysis: derived, FullyExplored: main.FullyExplored}
	for _, d := range derived {
		out.FullyExplored = out.FullyExplored && d.FullyExplored
	}
	return out, nil
}

func (a iamAnalysis) analyzeHTTP(r *http.Request, scope string) (any, error) {
	v := r.URL.Query()
	q, err := queryFromURL(v)
	if err != nil {
		return nil, err
	}
	if s := v.Get("executionTimeout"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return nil, apierror.InvalidArgument("Invalid value at 'execution_timeout' (type.googleapis.com/google.protobuf.Duration), %q", s)
		}
	}
	aq, err := a.resolveQuery(scope, v.Get("savedAnalysisQuery"), q, time.Now())
	if err != nil {
		return nil, err
	}
	return a.run(r.Context(), aq, iameval.ResourceExpansionLimit)
}

// ---- analyzeIamPolicyLongrunning ----

func (a iamAnalysis) longrunning(r *http.Request, scope string) (any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, apierror.InvalidArgument("read the request: %v", err)
	}
	var req assetpb.AnalyzeIamPolicyLongrunningRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := protojson.Unmarshal(body, &req); err != nil {
			return nil, apierror.InvalidArgument("Invalid JSON payload received. %v", err)
		}
	}
	now := time.Now()
	aq, err := a.resolveQuery(scope, req.GetSavedAnalysisQuery(), req.GetAnalysisQuery(), now)
	if err != nil {
		return nil, err
	}
	oc := req.GetOutputConfig()
	if oc == nil || (oc.GetGcsDestination() == nil && oc.GetBigqueryDestination() == nil) {
		return nil, apierror.InvalidArgument("output_config is required: set output_config.gcs_destination or output_config.bigquery_destination.")
	}
	var write func(ctx context.Context, opName string, resp *asset.AnalyzeIamPolicyResponse) error
	if g := oc.GetGcsDestination(); g != nil {
		bucket, object, ok := strings.Cut(strings.TrimPrefix(g.GetUri(), "gs://"), "/")
		if !strings.HasPrefix(g.GetUri(), "gs://") || !ok || bucket == "" || object == "" {
			return nil, apierror.InvalidArgument("Invalid output_config.gcs_destination.uri %q: it must be a Cloud Storage URI such as gs://bucket_name/object_name.", g.GetUri())
		}
		if a.out == nil || a.out.storage == nil || a.out.storage() == "" {
			return nil, apierror.InvalidArgument("Cloud Storage is not served on this instance, so gs://%s cannot be written.", bucket)
		}
		write = func(ctx context.Context, _ string, resp *asset.AnalyzeIamPolicyResponse) error {
			b, err := json.Marshal(resp)
			if err != nil {
				return err
			}
			return storageObjectWriter{src: storageSourceFetcher{addr: func() string { return "" }}, client: &http.Client{Timeout: 2 * time.Minute}}.
				writeAt(ctx, a.out.storage(), bucket, object, "application/json", b)
		}
	} else {
		bd := oc.GetBigqueryDestination()
		w, err := a.bigQueryWriter(r.Context(), bd)
		if err != nil {
			return nil, err
		}
		write = func(ctx context.Context, opName string, resp *asset.AnalyzeIamPolicyResponse) error {
			return w.write(ctx, opName, now, resp)
		}
	}
	canon, err := a.canonicalParent(aq.Scope)
	if err != nil {
		return nil, err
	}
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	name := canon + assetOpMarker + n.String()
	meta, _ := anypb.New(&assetpb.AnalyzeIamPolicyLongrunningMetadata{CreateTime: timestamppb.New(now)})
	op := &longrunningpb.Operation{Name: name, Metadata: meta}
	if err := putOp(st, op); err != nil {
		return nil, err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		done := proto.Clone(op).(*longrunningpb.Operation)
		done.Done = true
		resp, err := a.run(ctx, aq, iameval.LongrunningResourceExpansionLimit)
		if err == nil {
			err = write(ctx, name, resp)
		}
		if err != nil {
			done.Result = &longrunningpb.Operation_Error{Error: status.Convert(apierror.Wrap(err)).Proto()}
		} else {
			r, _ := anypb.New(&assetpb.AnalyzeIamPolicyLongrunningResponse{})
			done.Result = &longrunningpb.Operation_Response{Response: r}
		}
		_ = putOp(st, done)
	}()
	return op, nil
}

func putOp(st store.Store, op *longrunningpb.Operation) error {
	b, err := protojson.Marshal(op)
	if err != nil {
		return apierror.Internal(err, "encode the operation")
	}
	if err := st.Put(assetOpKey+op.GetName(), b); err != nil {
		return apierror.Internal(err, "store the operation")
	}
	return nil
}

func (a iamAnalysis) operation(name string) (any, error) {
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	b, err := st.Get(assetOpKey + name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierror.NotFound("Operation %s not found.", name)
		}
		return nil, apierror.Internal(err, "read the operation")
	}
	var op longrunningpb.Operation
	if err := protojson.Unmarshal(b, &op); err != nil {
		return nil, apierror.Internal(err, "decode the operation")
	}
	return &op, nil
}

// writeAt uploads an object to the Cloud Storage at base.
func (w storageObjectWriter) writeAt(ctx context.Context, base, bucket, name, contentType string, data []byte) error {
	u := base + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	_, msg, _, _ := apierror.DecodeJSON(io.LimitReader(resp.Body, 1<<16))
	if msg == "" {
		msg = resp.Status
	}
	if resp.StatusCode == http.StatusNotFound {
		return apierror.InvalidArgument("Cannot write gs://%s/%s: %s", bucket, name, msg)
	}
	return apierror.FailedPrecondition("Cannot write gs://%s/%s: %s", bucket, name, msg)
}

// analysisBigQuery writes an analysis to {prefix}_analysis and
// {prefix}_analysis_result.
type analysisBigQuery struct {
	tunnel           func() *netfwd.Forwarder
	project, dataset string
	prefix           string
	partition        bool
	disposition      string
}

var bqTablePrefix = regexp.MustCompile(`^[A-Za-z0-9_]{1,1000}$`)

func (a iamAnalysis) bigQueryWriter(ctx context.Context, bd *assetpb.IamPolicyAnalysisOutputConfig_BigQueryDestination) (*analysisBigQuery, error) {
	f := strings.Split(bd.GetDataset(), "/")
	if len(f) != 4 || f[0] != "projects" || f[2] != "datasets" || f[1] == "" || f[3] == "" {
		return nil, apierror.InvalidArgument("Invalid output_config.bigquery_destination.dataset %q: it must be projects/projectId/datasets/datasetId.", bd.GetDataset())
	}
	if !bqTablePrefix.MatchString(bd.GetTablePrefix()) {
		return nil, apierror.InvalidArgument("Invalid output_config.bigquery_destination.table_prefix %q: it is required, and may contain only letters, numbers and underscores.", bd.GetTablePrefix())
	}
	switch bd.GetWriteDisposition() {
	case "", "WRITE_APPEND", "WRITE_TRUNCATE", "WRITE_EMPTY":
	default:
		return nil, apierror.InvalidArgument("Invalid output_config.bigquery_destination.write_disposition %q: it must be WRITE_TRUNCATE, WRITE_APPEND or WRITE_EMPTY.", bd.GetWriteDisposition())
	}
	if a.out == nil || a.out.bigquery == nil {
		return nil, apierror.InvalidArgument("BigQuery is not served on this instance, so %s cannot be written.", bd.GetDataset())
	}
	w := &analysisBigQuery{tunnel: a.out.bigquery, project: registrySource{reg: a.reg}.projectID(a.registry(), f[1]), dataset: f[3], prefix: bd.GetTablePrefix(),
		partition:   bd.GetPartitionKey() == assetpb.IamPolicyAnalysisOutputConfig_BigQueryDestination_REQUEST_TIME,
		disposition: bd.GetWriteDisposition()}
	c, err := w.client(ctx)
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	defer c.Close()
	if _, err := c.Dataset(w.dataset).Metadata(ctx); err != nil {
		return nil, apierror.InvalidArgument("The dataset %s does not exist.", bd.GetDataset())
	}
	return w, nil
}

func (w *analysisBigQuery) client(ctx context.Context) (*bigquery.Client, error) {
	f := w.tunnel()
	if f == nil || f.HostAddr() == "" {
		return nil, errors.New("BigQuery has not started on this instance")
	}
	return bigquery.NewClient(ctx, w.project, option.WithEndpoint("http://"+f.HostAddr()),
		option.WithHTTPClient(&http.Client{Timeout: 2 * time.Minute}))
}

func bqStrings(names ...string) bigquery.Schema {
	var s bigquery.Schema
	for _, n := range names {
		s = append(s, &bigquery.FieldSchema{Name: n, Type: bigquery.StringFieldType})
	}
	return s
}

func bqRecord(name string, repeated bool, fields bigquery.Schema) *bigquery.FieldSchema {
	return &bigquery.FieldSchema{Name: name, Type: bigquery.RecordFieldType, Repeated: repeated, Schema: fields}
}

func bqRepeatedString(name string) *bigquery.FieldSchema {
	return &bigquery.FieldSchema{Name: name, Type: bigquery.StringFieldType, Repeated: true}
}

func bqBool(name string) *bigquery.FieldSchema {
	return &bigquery.FieldSchema{Name: name, Type: bigquery.BooleanFieldType}
}

// analysisState and edges as the API's JSON names them.
var (
	bqState = bqRecord("analysisState", false, bqStrings("code", "cause"))
	bqEdge  = bqStrings("sourceNode", "targetNode")
)

// analysisTableSchema is {prefix}_analysis's: the query and metadata.
func analysisTableSchema() bigquery.Schema {
	options := bigquery.Schema{bqBool("expandGroups"), bqBool("expandRoles"), bqBool("expandResources"),
		bqBool("outputResourceEdges"), bqBool("outputGroupEdges"), bqBool("analyzeServiceAccountImpersonation")}
	query := bigquery.Schema{
		{Name: "scope", Type: bigquery.StringFieldType},
		bqRecord("resourceSelector", false, bqStrings("fullResourceName")),
		bqRecord("identitySelector", false, bqStrings("identity")),
		bqRecord("accessSelector", false, bigquery.Schema{bqRepeatedString("roles"), bqRepeatedString("permissions")}),
		bqRecord("options", false, options),
		bqRecord("conditionContext", false, bigquery.Schema{{Name: "accessTime", Type: bigquery.TimestampFieldType}}),
	}
	return bigquery.Schema{
		{Name: "requestTime", Type: bigquery.TimestampFieldType},
		{Name: "opName", Type: bigquery.StringFieldType},
		{Name: "analysisId", Type: bigquery.IntegerFieldType},
		bqRecord("analysis", false, bigquery.Schema{
			bqRecord("analysisQuery", false, query),
			bqBool("fullyExplored"),
			bqRecord("nonCriticalErrors", true, bqStrings("code", "cause")),
		}),
	}
}

// analysisResultTableSchema is {prefix}_analysis_result's: one row per
// IamPolicyAnalysisResult.
func analysisResultTableSchema() bigquery.Schema {
	acl := bigquery.Schema{
		bqRecord("resources", true, append(bqStrings("fullResourceName"), bqState)),
		bqRecord("accesses", true, append(bqStrings("role", "permission"), bqState)),
		bqRecord("resourceEdges", true, bqEdge),
		bqRecord("conditionEvaluation", false, bqStrings("evaluationValue")),
	}
	return bigquery.Schema{
		{Name: "requestTime", Type: bigquery.TimestampFieldType},
		{Name: "opName", Type: bigquery.StringFieldType},
		{Name: "analysisId", Type: bigquery.IntegerFieldType},
		bqRecord("analysisResult", false, bigquery.Schema{
			{Name: "attachedResourceFullName", Type: bigquery.StringFieldType},
			bqRecord("iamBinding", false, bigquery.Schema{
				{Name: "role", Type: bigquery.StringFieldType},
				bqRepeatedString("members"),
				bqRecord("condition", false, bqStrings("expression", "title", "description", "location")),
			}),
			bqRecord("accessControlLists", true, acl),
			bqRecord("identityList", false, bigquery.Schema{
				bqRecord("identities", true, append(bqStrings("name"), bqState)),
				bqRecord("groupEdges", true, bqEdge),
			}),
			bqBool("fullyExplored"),
		}),
	}
}

// bqRow is a ValueSaver over a JSON object.
type bqRow map[string]bigquery.Value

func (r bqRow) Save() (map[string]bigquery.Value, string, error) { return r, "", nil }

// bqValue is a JSON value as a BigQuery value: objects as records, arrays
// as repeated values, accessTime as a timestamp.
func bqValue(key string, v any) bigquery.Value {
	switch x := v.(type) {
	case map[string]any:
		m := map[string]bigquery.Value{}
		for k, e := range x {
			m[k] = bqValue(k, e)
		}
		return m
	case []any:
		out := make([]bigquery.Value, 0, len(x))
		for _, e := range x {
			out = append(out, bqValue(key, e))
		}
		return out
	case string:
		if key == "accessTime" {
			if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return t
			}
		}
		return x
	}
	return v
}

func bqObject(v any) (map[string]bigquery.Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return bqValue("", m).(map[string]bigquery.Value), nil
}

// table makes (or, for WRITE_TRUNCATE, empties) one table, and checks
// WRITE_EMPTY's.
func (w *analysisBigQuery) table(ctx context.Context, c *bigquery.Client, name string, schema bigquery.Schema) (*bigquery.Table, error) {
	t := c.Dataset(w.dataset).Table(name)
	md := &bigquery.TableMetadata{Schema: schema}
	if w.partition {
		md.TimePartitioning = &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "requestTime"}
	}
	_, err := t.Metadata(ctx)
	exists := err == nil
	if exists {
		switch w.disposition {
		case "WRITE_TRUNCATE":
			if err := t.Delete(ctx); err != nil {
				return nil, err
			}
			exists = false
		case "WRITE_EMPTY":
			it := t.Read(ctx)
			var row []bigquery.Value
			if err := it.Next(&row); err == nil {
				return nil, apierror.FailedPrecondition("The table %s.%s already exists and contains data (write disposition WRITE_EMPTY).", w.dataset, name)
			} else if !errors.Is(err, iterator.Done) {
				return nil, err
			}
		}
	}
	if !exists {
		if err := t.Create(ctx, md); err != nil {
			var ge *googleapi.Error
			if !errors.As(err, &ge) || ge.Code != http.StatusConflict {
				return nil, err
			}
		}
	}
	return t, nil
}

func (w *analysisBigQuery) write(ctx context.Context, opName string, at time.Time, resp *asset.AnalyzeIamPolicyResponse) error {
	c, err := w.client(ctx)
	if err != nil {
		return apierror.FailedPrecondition("%v", err)
	}
	defer c.Close()
	analyses, err := w.table(ctx, c, w.prefix+"_analysis", analysisTableSchema())
	if err != nil {
		return err
	}
	results, err := w.table(ctx, c, w.prefix+"_analysis_result", analysisResultTableSchema())
	if err != nil {
		return err
	}
	all := append([]*asset.IamPolicyAnalysis{resp.MainAnalysis}, resp.ServiceAccountImpersonationAnalysis...)
	var aRows, rRows []bigquery.ValueSaver
	for id, an := range all {
		obj, err := bqObject(struct {
			AnalysisQuery     *asset.IamPolicyAnalysisQuery   `json:"analysisQuery,omitempty"`
			FullyExplored     bool                            `json:"fullyExplored"`
			NonCriticalErrors []*asset.IamPolicyAnalysisState `json:"nonCriticalErrors,omitempty"`
		}{an.AnalysisQuery, an.FullyExplored, an.NonCriticalErrors})
		if err != nil {
			return err
		}
		aRows = append(aRows, bqRow{"requestTime": at, "opName": opName, "analysisId": int64(id), "analysis": obj})
		for _, r := range an.AnalysisResults {
			obj, err := bqObject(r)
			if err != nil {
				return err
			}
			rRows = append(rRows, bqRow{"requestTime": at, "opName": opName, "analysisId": int64(id), "analysisResult": obj})
		}
	}
	if err := analyses.Inserter().Put(ctx, aRows); err != nil {
		return apierror.Internal(err, "write %s_analysis", w.prefix)
	}
	for i := 0; i < len(rRows); i += 500 {
		end := min(i+500, len(rRows))
		if err := results.Inserter().Put(ctx, rRows[i:end]); err != nil {
			return apierror.Internal(err, "write %s_analysis_result", w.prefix)
		}
	}
	return nil
}

// ---- saved queries ----

func savedKey(name string) string { return savedQueryKey + name }

func (a iamAnalysis) getSavedQuery(name string) (*assetpb.SavedQuery, error) {
	parent, id, ok := strings.Cut(name, savedQueriesPart+"/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return nil, apierror.InvalidArgument("Invalid saved query name %q: it must be projects/{PROJECT_NUMBER}/savedQueries/{SAVED_QUERY_ID}, folders/{FOLDER_NUMBER}/savedQueries/{SAVED_QUERY_ID} or organizations/{ORGANIZATION_NUMBER}/savedQueries/{SAVED_QUERY_ID}.", name)
	}
	canon, err := a.canonicalParent(parent)
	if err != nil {
		return nil, err
	}
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	b, err := st.Get(savedKey(canon + savedQueriesPart + "/" + id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, apierror.NotFound("Saved query %s not found.", name)
		}
		return nil, apierror.Internal(err, "read the saved query")
	}
	var sq assetpb.SavedQuery
	if err := protojson.Unmarshal(b, &sq); err != nil {
		return nil, apierror.Internal(err, "decode the saved query")
	}
	return &sq, nil
}

func putSavedQuery(st store.Store, sq *assetpb.SavedQuery) error {
	b, err := protojson.Marshal(sq)
	if err != nil {
		return apierror.Internal(err, "encode the saved query")
	}
	if err := st.Put(savedKey(sq.GetName()), b); err != nil {
		return apierror.Internal(err, "store the saved query")
	}
	return nil
}

// callerEmail is the account the request's bearer token names, the
// instance's own account without one.
func (a iamAnalysis) callerEmail(r *http.Request) string {
	if a.e == nil {
		return ""
	}
	if b := iameval.Bearer(r.Header.Get("Authorization")); b != "" && a.e.CallerOf != nil {
		if c := a.e.CallerOf(b); c.Email != "" {
			return c.Email
		}
	}
	return a.e.OwnerEmail
}

func checkSavedQuery(sq *assetpb.SavedQuery) error {
	if len(sq.GetLabels()) > 10 {
		return apierror.InvalidArgument("saved_query.labels must not contain more than 10 entries.")
	}
	return nil
}

func (a iamAnalysis) savedQueries(r *http.Request, p string) (any, error) {
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	parent, rest, _ := strings.Cut(p, savedQueriesPart)
	now := timestamppb.Now()
	switch {
	case rest == "" && r.Method == http.MethodPost:
		canon, err := a.canonicalParent(parent)
		if err != nil {
			return nil, err
		}
		id := r.URL.Query().Get("savedQueryId")
		if !savedQueryID.MatchString(id) {
			return nil, apierror.InvalidArgument("Invalid saved_query_id %q: it should be 4-63 characters, and valid characters are [a-z][0-9]-.", id)
		}
		var sq assetpb.SavedQuery
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(bytes.TrimSpace(body)) > 0 {
			if err := protojson.Unmarshal(body, &sq); err != nil {
				return nil, apierror.InvalidArgument("Invalid JSON payload received. %v", err)
			}
		}
		if err := checkSavedQuery(&sq); err != nil {
			return nil, err
		}
		sq.Name = canon + savedQueriesPart + "/" + id
		if _, err := st.Get(savedKey(sq.Name)); err == nil {
			return nil, apierror.AlreadyExists("Saved query %s already exists.", sq.Name)
		}
		email := a.callerEmail(r)
		sq.CreateTime, sq.LastUpdateTime, sq.Creator, sq.LastUpdater = now, now, email, email
		return &sq, putSavedQuery(st, &sq)
	case rest == "" && r.Method == http.MethodGet:
		canon, err := a.canonicalParent(parent)
		if err != nil {
			return nil, err
		}
		keys, err := st.List(savedKey(canon + savedQueriesPart + "/"))
		if err != nil {
			return nil, apierror.Internal(err, "list saved queries")
		}
		var all []*assetpb.SavedQuery
		for _, k := range keys {
			b, err := st.Get(k)
			if err != nil {
				continue
			}
			var sq assetpb.SavedQuery
			if protojson.Unmarshal(b, &sq) == nil {
				all = append(all, &sq)
			}
		}
		q := r.URL.Query()
		size := 50
		if s := q.Get("pageSize"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return nil, apierror.InvalidArgument("Invalid page_size %q.", s)
			}
			if n > 0 {
				size = min(n, 1000)
			}
		}
		page, next, err := aipfilter.ListPage("savedQueries|"+canon, q.Get("filter"), "", all, q.Get("pageToken"), size)
		if err != nil {
			return nil, err
		}
		return &assetpb.ListSavedQueriesResponse{SavedQueries: page, NextPageToken: next}, nil
	}
	name := parent + savedQueriesPart + rest
	switch r.Method {
	case http.MethodGet:
		return a.getSavedQuery(name)
	case http.MethodDelete:
		sq, err := a.getSavedQuery(name)
		if err != nil {
			return nil, err
		}
		if err := st.Delete(savedKey(sq.GetName())); err != nil {
			return nil, apierror.Internal(err, "delete the saved query")
		}
		return map[string]any{}, nil
	case http.MethodPatch:
		sq, err := a.getSavedQuery(name)
		if err != nil {
			return nil, err
		}
		mask := r.URL.Query().Get("updateMask")
		if mask == "" {
			return nil, apierror.InvalidArgument("update_mask is required: the list of fields to update.")
		}
		var in assetpb.SavedQuery
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err := protojson.Unmarshal(body, &in); err != nil {
			return nil, apierror.InvalidArgument("Invalid JSON payload received. %v", err)
		}
		fm := &fieldmaskpb.FieldMask{}
		for _, f := range strings.Split(mask, ",") {
			f = strings.TrimSpace(f)
			switch f {
			case "description":
				sq.Description = in.GetDescription()
			case "labels":
				sq.Labels = in.GetLabels()
			case "content":
				sq.Content = in.GetContent()
			default:
				return nil, apierror.InvalidArgument("Invalid update_mask path %q: only description, labels and content can be updated.", f)
			}
			fm.Paths = append(fm.Paths, f)
		}
		if err := checkSavedQuery(sq); err != nil {
			return nil, err
		}
		sq.LastUpdateTime, sq.LastUpdater = now, a.callerEmail(r)
		return sq, putSavedQuery(st, sq)
	}
	return nil, apierror.NotFound("%s %s is not served", r.Method, r.URL.Path)
}

// ---- the inventory ----

// analysisInventory is iameval.Inventory over the instance.
type analysisInventory struct {
	ctx context.Context
	a   iamAnalysis
	src registrySource
	// byProject are each project's service resources, read once.
	byProject map[string][]analysisChild
}

type analysisChild struct{ full, parent string }

func newAnalysisInventory(ctx context.Context, a iamAnalysis) *analysisInventory {
	return &analysisInventory{ctx: ctx, a: a, src: registrySource{reg: a.reg}, byProject: map[string][]analysisChild{}}
}

func splitFull(full string) (host, name string) {
	host, name, _ = strings.Cut(strings.TrimPrefix(full, "//"), "/")
	return host, name
}

func (v *analysisInventory) Root(scope string) (string, error) {
	canon, err := v.a.canonicalParent(scope)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(canon, "projects/") {
		return iameval.CRMPrefix + "projects/" + v.src.projectID(v.a.registry(), strings.TrimPrefix(canon, "projects/")), nil
	}
	return iameval.CRMPrefix + canon, nil
}

// project's service resources: its service accounts, then each lister's.
func (v *analysisInventory) projectResources(project string) []analysisChild {
	if out, ok := v.byProject[project]; ok {
		return out
	}
	var out []analysisChild
	if reg := v.a.registry(); reg != nil {
		token := ""
		for {
			resp, err := reg.ListServiceAccounts(&adminpb.ListServiceAccountsRequest{Name: "projects/" + project, PageSize: 100, PageToken: token})
			if err != nil {
				break
			}
			for _, sa := range resp.GetAccounts() {
				out = append(out, analysisChild{full: "//iam.googleapis.com/projects/" + project + "/serviceAccounts/" + sa.GetEmail()})
			}
			if token = resp.GetNextPageToken(); token == "" {
				break
			}
		}
	}
	for _, b := range v.a.out.storageBuckets(v.ctx, project) {
		out = append(out, analysisChild{full: "//storage.googleapis.com/" + b.Name})
	}
	remote := make([]string, 0, len(assetRemotes))
	for h := range assetRemotes {
		remote = append(remote, h)
	}
	sort.Strings(remote)
	out = append(out, v.a.out.remoteResources(v.ctx, project, remote)...)
	if v.a.e != nil {
		hosts := make([]string, 0, len(v.a.e.Resources))
		for h := range v.a.e.Resources {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		for _, h := range hosts {
			for _, c := range v.a.e.Resources[h](v.ctx, project) {
				ch := analysisChild{full: "//" + h + "/" + c.Name}
				if c.Parent != "" {
					ch.parent = "//" + h + "/" + c.Parent
				}
				out = append(out, ch)
			}
		}
	}
	v.byProject[project] = out
	return out
}

func (v *analysisInventory) Children(full string) []string {
	host, name := splitFull(full)
	reg := v.a.registry()
	if reg == nil {
		return nil
	}
	var out []string
	if host == "cloudresourcemanager.googleapis.com" {
		if strings.HasPrefix(name, "tagKeys/") {
			// A tag key's values (cloudresourcemanager.googleapis.com/TagValue).
			vals, _ := reg.TagValues(name)
			for _, tv := range vals {
				out = append(out, iameval.CRMPrefix+tv.GetName())
			}
			return out
		}
		out = append(out, v.tagKeysUnder(reg, name)...)
		if p, ok := strings.CutPrefix(name, "projects/"); ok {
			for _, c := range v.projectResources(p) {
				if c.parent == "" {
					out = append(out, c.full)
				}
			}
			return out
		}
		if folders, err := reg.Folders(); err == nil {
			for _, f := range folders {
				if f.Parent == name && f.State == resourcemanager.StateActive {
					out = append(out, iameval.CRMPrefix+f.Name)
				}
			}
		}
		if projects, err := reg.List(); err == nil {
			for _, p := range projects {
				if p.Parent == name && p.State == resourcemanager.StateActive {
					out = append(out, iameval.CRMPrefix+"projects/"+p.ProjectID)
				}
			}
		}
		return out
	}
	project := iameval.Project(name)
	if project == "" {
		return nil
	}
	for _, c := range v.projectResources(project) {
		if c.parent == full {
			out = append(out, c.full)
		}
	}
	return out
}

func (v *analysisInventory) Policy(full string) *iampolicy.Stored {
	host, name := splitFull(full)
	switch host {
	case "cloudresourcemanager.googleapis.com", iameval.IAM.Name:
		return v.src.Policy(name)
	case "storage.googleapis.com":
		return v.a.out.storagePolicy(v.ctx, name)
	}
	if p, ok := v.a.out.remotePolicy(v.ctx, host, name); ok {
		return p
	}
	if v.a.e != nil {
		if get := v.a.e.Policies[host]; get != nil {
			return get(v.ctx, name)
		}
	}
	return nil
}

func (v *analysisInventory) Chain(full string) ([]iameval.Attached, error) {
	host, name := splitFull(full)
	if host == "cloudresourcemanager.googleapis.com" {
		if own, parent, ok := v.tagChain(name); ok {
			// A tag value's own policy and its key's, then its key's
			// parent's chain.
			rest, err := v.Chain(iameval.CRMPrefix + parent)
			if err != nil {
				return nil, err
			}
			return append(own, rest...), nil
		}
		var rs []string
		if p, ok := strings.CutPrefix(name, "projects/"); ok {
			rs = v.src.Ancestry(v.src.projectID(v.a.registry(), p))
		} else {
			rs = v.src.HierarchyAncestry(name)
		}
		out := make([]iameval.Attached, 0, len(rs))
		for _, r := range rs {
			out = append(out, iameval.Attached{Resource: iameval.CRMPrefix + r, Policy: v.src.Policy(r)})
		}
		return out, nil
	}
	req, err := v.a.requestFor(v.ctx, "", "", full, time.Now())
	if err != nil {
		return nil, err
	}
	chain := v.a.e.Eval.Chain(req)
	for i := range chain {
		chain[i].Resource = fullName(chain[i].Resource)
	}
	if _, remote := assetRemotes[host]; remote {
		// An in-cluster product's resource: its own policy and its
		// parents' within the product, then the project's chain.
		var own []iameval.Attached
		for _, r := range remoteParents(name) {
			p, _ := v.a.out.remotePolicy(v.ctx, host, r)
			own = append(own, iameval.Attached{Resource: "//" + host + "/" + r, Policy: p})
		}
		chain = append(own, chain...)
	}
	return chain, nil
}

func (v *analysisInventory) Attributes(full string) iamcond.Attributes {
	host, name := splitFull(full)
	if host == "cloudresourcemanager.googleapis.com" {
		typ := "cloudresourcemanager.googleapis.com/Project"
		switch {
		case strings.HasPrefix(name, "tagKeys/"):
			typ = "cloudresourcemanager.googleapis.com/TagKey"
		case strings.HasPrefix(name, "tagValues/"):
			typ = "cloudresourcemanager.googleapis.com/TagValue"
		case strings.HasPrefix(name, "folders/"):
			typ = "cloudresourcemanager.googleapis.com/Folder"
		case strings.HasPrefix(name, "organizations/"):
			typ = "cloudresourcemanager.googleapis.com/Organization"
		}
		return iamcond.Attributes{ResourceName: name, ResourceType: typ, ResourceService: host}
	}
	req, err := v.a.requestFor(v.ctx, "", "", full, time.Now())
	if err != nil {
		return iamcond.Attributes{ResourceName: name, ResourceService: host}
	}
	return req.Attributes()
}

// tagKeysUnder are the tag keys whose parent is the organization or
// project name names (a project's by its ID or number), by full name:
// cloudresourcemanager.googleapis.com/TagKey assets.
func (v *analysisInventory) tagKeysUnder(reg *resourcemanager.Registry, name string) []string {
	parents := map[string]bool{name: true}
	if id, ok := strings.CutPrefix(name, "projects/"); ok {
		if p, err := reg.Get(v.src.projectID(reg, id)); err == nil {
			parents["projects/"+p.ProjectID] = true
			parents["projects/"+strconv.FormatInt(p.Number, 10)] = true
		}
	} else if !strings.HasPrefix(name, "organizations/") {
		return nil
	}
	keys, err := reg.AllTagKeys()
	if err != nil {
		return nil
	}
	var out []string
	for _, k := range keys {
		if parents[k.GetParent()] {
			out = append(out, iameval.CRMPrefix+k.GetName())
		}
	}
	return out
}

// tagChain is a tag key's or value's own policies (a value's, then its
// key's) and the key's parent; ok is false for any other name.
func (v *analysisInventory) tagChain(name string) (own []iameval.Attached, parent string, ok bool) {
	reg := v.a.registry()
	if reg == nil || !(strings.HasPrefix(name, "tagKeys/") || strings.HasPrefix(name, "tagValues/")) {
		return nil, "", false
	}
	key := name
	if strings.HasPrefix(name, "tagValues/") {
		tv, err := reg.TagValue(name)
		if err != nil {
			return nil, "", false
		}
		own = append(own, iameval.Attached{Resource: iameval.CRMPrefix + name, Policy: v.src.Policy(name)})
		key = tv.GetParent()
	}
	k, err := reg.TagKey(key)
	if err != nil {
		return nil, "", false
	}
	own = append(own, iameval.Attached{Resource: iameval.CRMPrefix + key, Policy: v.src.Policy(key)})
	return own, k.GetParent(), true
}

func (v *analysisInventory) GroupMembers(email string) []string {
	reg := v.a.registry()
	if reg == nil {
		return nil
	}
	name, err := reg.LookupGroup(email, "")
	if err != nil {
		return nil
	}
	g, err := reg.ListMemberships(name)
	if err != nil || g == nil {
		return nil
	}
	now := time.Now()
	var out []string
	for _, m := range g.Members {
		if m.ExpireTime != nil && !now.Before(*m.ExpireTime) {
			continue
		}
		switch m.Type {
		case "GROUP":
			out = append(out, "group:"+m.Email)
		case "SERVICE_ACCOUNT":
			out = append(out, "serviceAccount:"+m.Email)
		default:
			out = append(out, "user:"+m.Email)
		}
	}
	return out
}

func (v *analysisInventory) Principals(member string) []string {
	kind, email, _ := strings.Cut(member, ":")
	ps := v.src.Principals(email)
	if strings.EqualFold(kind, "group") && len(ps) > 0 {
		ps[0] = "group:" + strings.ToLower(email)
	}
	return ps
}

// ---- the console ----

// consoleAnalyzeOptions are the console's Advanced options.
type consoleAnalyzeOptions struct {
	ExpandGroups        bool   `json:"expandGroups"`
	ExpandRoles         bool   `json:"expandRoles"`
	ExpandResources     bool   `json:"expandResources"`
	OutputResourceEdges bool   `json:"outputResourceEdges"`
	OutputGroupEdges    bool   `json:"outputGroupEdges"`
	Impersonation       bool   `json:"analyzeServiceAccountImpersonation"`
	AccessTime          string `json:"accessTime"`
	SavedQuery          string `json:"savedQuery"`
}

// consoleAnalyzeURL is analyzeIamPolicy's URL for the console's query.
func consoleAnalyzeURL(scope string, in consoleAnalyze) string {
	q := url.Values{}
	if in.Identity != "" {
		q.Set("analysisQuery.identitySelector.identity", in.Identity)
	}
	for _, x := range in.Permissions {
		q.Add("analysisQuery.accessSelector.permissions", x)
	}
	for _, x := range in.Roles {
		q.Add("analysisQuery.accessSelector.roles", x)
	}
	if in.Resource != "" {
		q.Set("analysisQuery.resourceSelector.fullResourceName", in.Resource)
	}
	for name, on := range map[string]bool{"expandGroups": in.ExpandGroups, "expandRoles": in.ExpandRoles,
		"expandResources": in.ExpandResources, "outputResourceEdges": in.OutputResourceEdges,
		"outputGroupEdges": in.OutputGroupEdges, "analyzeServiceAccountImpersonation": in.Impersonation} {
		if on {
			q.Set("analysisQuery.options."+name, "true")
		}
	}
	if in.AccessTime != "" {
		q.Set("analysisQuery.conditionContext.accessTime", in.AccessTime)
	}
	if in.SavedQuery != "" {
		q.Set("savedAnalysisQuery", in.SavedQuery)
	}
	return "/v1/" + scope + analyzeSuffix + "?" + q.Encode()
}

// consoleQuery is the console's query as an IamPolicyAnalysisQuery.
func consoleQuery(scope string, in consoleAnalyze) *assetpb.IamPolicyAnalysisQuery {
	r, _ := http.NewRequest(http.MethodGet, consoleAnalyzeURL(scope, in), nil)
	q, _ := queryFromURL(r.URL.Query())
	if q == nil {
		q = &assetpb.IamPolicyAnalysisQuery{}
	}
	q.Scope = scope
	return q
}

// consoleExport is the console's Export: a destination for the query.
type consoleExport struct {
	consoleAnalyze
	GCSURI           string `json:"gcsUri"`
	Dataset          string `json:"dataset"`
	TablePrefix      string `json:"tablePrefix"`
	PartitionKey     string `json:"partitionKey"`
	WriteDisposition string `json:"writeDisposition"`
}

func consoleExportBody(scope string, in consoleExport) ([]byte, error) {
	req := &assetpb.AnalyzeIamPolicyLongrunningRequest{AnalysisQuery: consoleQuery(scope, in.consoleAnalyze), SavedAnalysisQuery: in.SavedQuery,
		OutputConfig: &assetpb.IamPolicyAnalysisOutputConfig{}}
	if in.GCSURI != "" {
		req.OutputConfig.Destination = &assetpb.IamPolicyAnalysisOutputConfig_GcsDestination_{
			GcsDestination: &assetpb.IamPolicyAnalysisOutputConfig_GcsDestination{Uri: in.GCSURI}}
	} else {
		bd := &assetpb.IamPolicyAnalysisOutputConfig_BigQueryDestination{Dataset: in.Dataset, TablePrefix: in.TablePrefix, WriteDisposition: in.WriteDisposition}
		if in.PartitionKey == "REQUEST_TIME" {
			bd.PartitionKey = assetpb.IamPolicyAnalysisOutputConfig_BigQueryDestination_REQUEST_TIME
		}
		req.OutputConfig.Destination = &assetpb.IamPolicyAnalysisOutputConfig_BigqueryDestination{BigqueryDestination: bd}
	}
	return protojson.Marshal(req)
}

// consoleSaved is the console's Save query.
type consoleSaved struct {
	consoleAnalyze
	ID          string `json:"id"`
	Description string `json:"description"`
	Name        string `json:"name"`
}

// consoleAssetRequest is the request for one of the console's Cloud Asset
// calls.
func consoleAssetRequest(ctx context.Context, project, call string, body json.RawMessage) (*http.Request, error) {
	var in consoleSaved
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("malformed request: %v", err)
		}
	}
	scope := in.Scope
	if scope == "" {
		scope = "projects/" + project
	}
	mk := func(method, u string, b []byte) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(b))
		return r
	}
	switch call {
	case "analyze":
		return mk(http.MethodGet, consoleAnalyzeURL(scope, in.consoleAnalyze), nil), nil
	case "analyze-export":
		var ex consoleExport
		if err := json.Unmarshal(body, &ex); err != nil {
			return nil, fmt.Errorf("malformed request: %v", err)
		}
		b, err := consoleExportBody(scope, ex)
		if err != nil {
			return nil, err
		}
		return mk(http.MethodPost, "/v1/"+scope+longrunningSuffix, b), nil
	case "analyze-operation":
		return mk(http.MethodGet, "/v1/"+in.Name, nil), nil
	case "saved-list":
		return mk(http.MethodGet, "/v1/"+scope+savedQueriesPart+"?pageSize=1000", nil), nil
	case "saved-create":
		sq := &assetpb.SavedQuery{Description: in.Description, Content: &assetpb.SavedQuery_QueryContent{
			QueryContent: &assetpb.SavedQuery_QueryContent_IamPolicyAnalysisQuery{IamPolicyAnalysisQuery: consoleQuery(scope, in.consoleAnalyze)}}}
		b, _ := protojson.Marshal(sq)
		return mk(http.MethodPost, "/v1/"+scope+savedQueriesPart+"?savedQueryId="+url.QueryEscape(in.ID), b), nil
	case "saved-delete":
		return mk(http.MethodDelete, "/v1/"+in.Name, nil), nil
	}
	return nil, fmt.Errorf("no such call %q", call)
}

func assetUnavailable(format string, a ...any) *apierror.Error {
	return &apierror.Error{Code: codes.Unavailable, Message: fmt.Sprintf(format, a...), Reason: "backendError"}
}

func assetDenied(format string, a ...any) *apierror.Error {
	return &apierror.Error{Code: codes.PermissionDenied, Message: fmt.Sprintf(format, a...), Reason: "forbidden"}
}
