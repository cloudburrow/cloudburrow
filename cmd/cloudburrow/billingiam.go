package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	billingpb "cloud.google.com/go/billing/apiv1/billingpb"
	budgetspb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	pubsubapi "cloud.google.com/go/pubsub/v2/apiv1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/billing"
)

// Cloud Billing's IAM enforcement (#2109). iameval.Billing maps the
// methods; this file checks projects.updateBillingInfo, whose permissions
// depend on the request, and gives the evaluator the billing accounts'
// own policies and ancestry (registrySource, iamenforce.go), which Cloud
// Logging's billing account resources use too.

// billingAccounts is the instance's Cloud Billing server, for IAM's and
// Logging's checks of billingAccounts/* resources; nil before it is served.
var billingAccounts atomic.Pointer[billing.Server]

const updateBillingInfo = "/google.cloud.billing.v1.CloudBilling/UpdateProjectBillingInfo"

// billingLinkCheck checks a project's link or unlink: linking needs
// resourcemanager.projects.createBillingAssignment on the project and
// billing.resourceAssociations.create on the account ("a combination of
// Project Billing Manager and Billing Account User"); unlinking
// resourcemanager.projects.deleteBillingAssignment on the project, or,
// as "an owner of the billing account for the project" may,
// billing.resourceAssociations.delete on its current account
// (https://docs.cloud.google.com/billing/docs/how-to/billing-access and
// https://docs.cloud.google.com/billing/docs/reference/rest/v1/projects/updateBillingInfo,
// read 2026-11-21).
func billingLinkCheck(ctx context.Context, e *iameval.Enforcer, srv *billing.Server, bearer, project, account string) error {
	project = strings.TrimSuffix(strings.TrimSuffix(project, "/"), "/billingInfo")
	if account != "" {
		if err := e.Authorize(ctx, iameval.Billing, bearer, "resourcemanager.projects.createBillingAssignment", project); err != nil {
			return err
		}
		return e.Authorize(ctx, iameval.Billing, bearer, "billing.resourceAssociations.create", account)
	}
	err := e.Authorize(ctx, iameval.Billing, bearer, "resourcemanager.projects.deleteBillingAssignment", project)
	if err == nil || srv == nil {
		return err
	}
	if cur, cerr := srv.ProjectBilling(strings.TrimPrefix(project, "projects/")); cerr == nil && cur.GetBillingAccountName() != "" {
		if e.Authorize(ctx, iameval.Billing, bearer, "billing.resourceAssociations.delete", cur.GetBillingAccountName()) == nil {
			return nil
		}
	}
	return err
}

// billingLinkInterceptor is billingLinkCheck over gRPC.
func billingLinkInterceptor(e *iameval.Enforcer, srv *billing.Server) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		in, ok := req.(*billingpb.UpdateProjectBillingInfoRequest)
		if !ok || info.FullMethod != updateBillingInfo || e == nil || e.On == nil || !e.On() {
			return handler(ctx, req)
		}
		if err := billingLinkCheck(ctx, e, srv, iameval.IncomingBearer(ctx), in.GetName(), in.GetProjectBillingInfo().GetBillingAccountName()); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// billingLinkJSON is billingLinkCheck over JSON: PUT
// /v1/projects/{p}/billingInfo, with or without a trailing slash.
func billingLinkJSON(e *iameval.Enforcer, srv *billing.Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := strings.CutPrefix(r.URL.Path, "/v1/")
		if !ok || r.Method != http.MethodPut || !strings.HasPrefix(p, "projects/") ||
			!strings.HasSuffix(strings.TrimSuffix(p, "/"), "/billingInfo") || e == nil || e.On == nil || !e.On() {
			next.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			BillingAccountName string `json:"billingAccountName"`
		}
		_ = json.Unmarshal(raw, &body)
		if err := billingLinkCheck(r.Context(), e, srv, iameval.Bearer(r.Header.Get("Authorization")), p, body.BillingAccountName); err != nil {
			iameval.WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// billingJSONEnforced wraps Cloud Billing's JSON handler in the table's
// checks, the link check and the budget topic check, when enforcement is
// on.
func billingJSONEnforced(e *iameval.Enforcer, srv *billing.Server, pubsub func() *netfwd.Forwarder, h http.Handler) http.Handler {
	if e == nil || e.On == nil || !e.On() {
		return h
	}
	return jsonEnforcer(e, iameval.Billing, nil)(billingLinkJSON(e, srv, budgetTopicJSON(e, pubsub, h)))
}

// budgetTopicCheck checks that the caller of bearer may grant on topic,
// as a budget's Pub/Sub topic needs: "You must have permission to grant
// the Pub/Sub Publisher role on the topic", pubsub.topics.setIamPolicy
// (https://docs.cloud.google.com/billing/docs/how-to/budgets-programmatic-notifications,
// read 2026-11-21). The topic's policy is Pub/Sub's, so Pub/Sub answers:
// its TestIamPermissions as the caller. A topic Pub/Sub does not hold
// grants nothing, and is refused the same way.
func budgetTopicCheck(ctx context.Context, pubsub func() *netfwd.Forwarder, bearer, topic string) error {
	const permission = "pubsub.topics.setIamPolicy"
	if topic == "" || bearer == "" || pubsub == nil {
		return nil
	}
	denied := iameval.Billing.Deny(permission, topic)
	f := pubsub()
	if f == nil || f.HostAddr() == "" {
		return denied
	}
	c, err := pubsubapi.NewTopicAdminClient(ctx, option.WithEndpoint(f.HostAddr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		option.WithGRPCDialOption(grpc.WithPerRPCCredentials(callerToken(bearer))))
	if err != nil {
		return denied
	}
	defer c.Close()
	r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: topic, Permissions: []string{permission}})
	if err != nil || len(r.GetPermissions()) != 1 {
		return denied
	}
	return nil
}

// callerToken sends a caller's own bearer token on.
type callerToken string

func (t callerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

func (callerToken) RequireTransportSecurity() bool { return false }

// budgetTopicInterceptor is budgetTopicCheck over gRPC, for CreateBudget
// and UpdateBudget.
func budgetTopicInterceptor(e *iameval.Enforcer, pubsub func() *netfwd.Forwarder) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if e == nil || e.On == nil || !e.On() {
			return handler(ctx, req)
		}
		var topic string
		switch in := req.(type) {
		case *budgetspb.CreateBudgetRequest:
			topic = in.GetBudget().GetNotificationsRule().GetPubsubTopic()
		case *budgetspb.UpdateBudgetRequest:
			topic = in.GetBudget().GetNotificationsRule().GetPubsubTopic()
		default:
			return handler(ctx, req)
		}
		if err := budgetTopicCheck(ctx, pubsub, iameval.IncomingBearer(ctx), topic); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// budgetTopicJSON is budgetTopicCheck over JSON: a budget's create (POST)
// or patch, whose body is the budget.
func budgetTopicJSON(e *iameval.Enforcer, pubsub func() *netfwd.Forwarder, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e == nil || e.On == nil || !e.On() || !isBudgetsPath(r) || (r.Method != http.MethodPost && r.Method != http.MethodPatch) {
			next.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			NotificationsRule struct {
				PubsubTopic string `json:"pubsubTopic"`
			} `json:"notificationsRule"`
		}
		_ = json.Unmarshal(raw, &body)
		if err := budgetTopicCheck(r.Context(), pubsub, iameval.Bearer(r.Header.Get("Authorization")), body.NotificationsRule.PubsubTopic); err != nil {
			iameval.WriteJSONStatus(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}
