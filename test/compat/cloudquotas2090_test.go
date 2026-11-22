//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	gaxerr "github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestCloudQuotasSetParameterManagerRequestQuotas (#2090), with
// cloud.google.com/go/cloudquotas and the Parameter Manager client, over
// gRPC and over REST: Parameter Manager's read, write and render requests
// are QuotaInfos of parametermanager.googleapis.com (quota ID the limit's
// name, the metric, no dimensions, refreshed each minute) at Google's
// defaults; a QuotaPreference created with a preferred value, empty
// dimensions and a justification sets the project's read limit, which the
// QuotaInfo then reports; lowering it with UpdateQuotaPreference takes
// effect on the next calls, the call past it RESOURCE_EXHAUSTED with
// Google's wording and the new limit in its ErrorInfo; raising it again
// restores access.
func TestCloudQuotasSetParameterManagerRequestQuotas(t *testing.T) {
	h := New(t)
	quotas, _ := cloudQuotasClients(t, h)
	pms := parameterManagerClients(t, h)
	ctx := h.Context()
	const (
		service = "parametermanager.googleapis.com"
		readID  = "ReadRequestsPerMinutePerProject"
		metric  = service + "/read_requests"
	)
	for transport, c := range pms {
		q := quotas[transport]
		t.Run(transport, func(t *testing.T) {
			project := fmt.Sprintf("cq-pm-%s-%d", transport, time.Now().UnixNano()%1e6)
			parent := "projects/" + project + "/locations/global"
			infoName := parent + "/services/" + service + "/quotaInfos/" + readID
			value := func() int64 {
				t.Helper()
				qi, err := q.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: infoName})
				if err != nil {
					t.Fatalf("GetQuotaInfo: %v", err)
				}
				return qi.GetDimensionsInfos()[0].GetDetails().GetValue()
			}
			it := q.ListQuotaInfos(ctx, &pb.ListQuotaInfosRequest{Parent: parent + "/services/" + service})
			byID := map[string]*pb.QuotaInfo{}
			for {
				qi, err := it.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					t.Fatalf("ListQuotaInfos: %v", err)
				}
				byID[qi.GetQuotaId()] = qi
			}
			for id, def := range map[string]int64{readID: 1200, "WriteRequestsPerMinutePerProject": 1200, "RenderRequestsPerMinutePerProject": 1800} {
				qi := byID[id]
				if qi == nil {
					t.Fatalf("ListQuotaInfos lacks %s: %v", id, byID)
				}
				if qi.GetService() != service || qi.GetRefreshInterval() != "minute" || len(qi.GetDimensions()) != 0 || qi.GetIsFixed() ||
					qi.GetMetricUnit() != "1/min/{project}" || qi.GetDimensionsInfos()[0].GetDetails().GetValue() != def {
					t.Errorf("QuotaInfo %s = %v", id, qi)
				}
			}
			if qi := byID[readID]; qi.GetMetric() != metric || qi.GetQuotaDisplayName() != "Read requests per minute per project" {
				t.Errorf("read QuotaInfo = %v", qi)
			}
			if v := value(); v != 1200 {
				t.Fatalf("read quota = %d, want Google's default 1200", v)
			}

			// Not across a minute's end, where the quota refills.
			if s := time.Now().Second(); s > 45 {
				time.Sleep(time.Duration(61-s) * time.Second)
			}
			read := func() error {
				_, err := c.ListParameters(ctx, &pmpb.ListParametersRequest{Parent: parent}).Next()
				if errors.Is(err, iterator.Done) {
					return nil
				}
				return err
			}
			ignore := []pb.QuotaSafetyCheck{pb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH}
			pref, err := q.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "pm-reads",
				QuotaPreference: &pb.QuotaPreference{Service: service, QuotaId: readID, Dimensions: map[string]string{},
					Justification: "compat test", QuotaConfig: &pb.QuotaConfig{PreferredValue: 5}},
				IgnoreSafetyChecks: ignore})
			if err != nil {
				t.Fatalf("CreateQuotaPreference: %v", err)
			}
			if pref.GetQuotaConfig().GetGrantedValue().GetValue() != 5 || pref.GetJustification() != "compat test" {
				t.Errorf("preference = %v", pref)
			}
			if v := value(); v != 5 {
				t.Errorf("QuotaInfo after the preference = %d, want 5", v)
			}
			if err := read(); err != nil {
				t.Fatalf("read 1: %v", err)
			}
			// Lowered to 2: the next read passes, the one after is refused.
			pref.QuotaConfig = &pb.QuotaConfig{PreferredValue: 2}
			pref, err = q.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{QuotaPreference: pref,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"quota_config.preferred_value"}}, IgnoreSafetyChecks: ignore})
			if err != nil {
				t.Fatalf("UpdateQuotaPreference to 2: %v", err)
			}
			if v := value(); v != 2 {
				t.Errorf("QuotaInfo after lowering = %d, want 2", v)
			}
			if err := read(); err != nil {
				t.Fatalf("read 2: %v", err)
			}
			err = read()
			var ae *gaxerr.APIError
			ok := errors.As(err, &ae)
			if !ok {
				ae, ok = gaxerr.FromError(err)
			}
			if !ok || pmCode(err) != codes.ResourceExhausted {
				t.Fatalf("read 3 = %v; want RESOURCE_EXHAUSTED", err)
			}
			if transport == "rest" && ae.HTTPCode() != http.StatusTooManyRequests {
				t.Errorf("HTTP status %d; want 429", ae.HTTPCode())
			}
			if !strings.Contains(err.Error(), "Quota exceeded for quota metric 'Read requests' and limit 'Read requests per minute per project' of service 'parametermanager.googleapis.com' for consumer 'project_number:") {
				t.Errorf("message = %v", err)
			}
			if ae.Reason() != "RATE_LIMIT_EXCEEDED" || ae.Metadata()["quota_metric"] != metric || ae.Metadata()["quota_limit"] != readID ||
				ae.Metadata()["quota_limit_value"] != "2" {
				t.Errorf("ErrorInfo = %s %v", ae.Reason(), ae.Metadata())
			}
			if _, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: "w"}); err != nil {
				t.Errorf("a write (another quota) = %v", err)
			}
			_ = c.DeleteParameter(ctx, &pmpb.DeleteParameterRequest{Name: parent + "/parameters/w"})
			// Raised again: the project reads again.
			pref.QuotaConfig = &pb.QuotaConfig{PreferredValue: 1200}
			if _, err := q.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{QuotaPreference: pref,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"quota_config.preferred_value"}}}); err != nil {
				t.Fatalf("UpdateQuotaPreference to 1200: %v", err)
			}
			if err := read(); err != nil {
				t.Errorf("a read after raising the quota = %v", err)
			}
			if v := value(); v != 1200 {
				t.Errorf("QuotaInfo after raising = %d, want 1200", v)
			}
		})
	}
}
