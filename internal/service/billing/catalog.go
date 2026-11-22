package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	pb "cloud.google.com/go/billing/apiv1/billingpb"
	"google.golang.org/genproto/googleapis/type/money"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// API is one API the instance serves, by googleapis.com name and title.
type API struct {
	Name, Title string
}

// documentedServiceIDs are the Cloud Catalog service IDs Google's docs
// state (get-pricing-information-api's example response names Compute
// Engine's). Every other service's ID is local, derived from its API name
// in the same XXXX-XXXX-XXXX form; Google's IDs for them are not in its
// public docs.
var documentedServiceIDs = map[string]string{"compute.googleapis.com": "6F81-5844-456A"}

// ServiceID is the catalog ID of the API name.
func ServiceID(api string) string {
	if id, ok := documentedServiceIDs[api]; ok {
		return id
	}
	sum := sha256.Sum256([]byte("cloudburrow-catalog/" + api))
	h := strings.ToUpper(hex.EncodeToString(sum[:6]))
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12]
}

// skuID is the ID of the API's one local SKU.
func skuID(api string) string {
	sum := sha256.Sum256([]byte("cloudburrow-sku/" + api))
	h := strings.ToUpper(hex.EncodeToString(sum[:6]))
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12]
}

// Catalog is the Cloud Catalog API (CloudCatalog): the services the
// instance serves, each with one SKU, the API's requests, at the local list
// price of 0: CloudBurrow charges nothing, so the usage it measures costs
// nothing and every budget's spend is 0.
type Catalog struct {
	pb.UnimplementedCloudCatalogServer
	// APIs are the served APIs; nil serves an empty catalog.
	APIs func() []API
	// Since is the local SKUs' effective time.
	Since time.Time
}

func (c *Catalog) apis() []API {
	if c.APIs == nil {
		return nil
	}
	return c.APIs()
}

// Service is the catalog entry for the API.
func Service(a API) *pb.Service {
	id := ServiceID(a.Name)
	return &pb.Service{Name: "services/" + id, ServiceId: id, DisplayName: strings.TrimSuffix(a.Title, " API"),
		BusinessEntityName: "businessEntities/GCP"}
}

// Sku is the API's local SKU.
func (c *Catalog) Sku(a API, currency string) *pb.Sku {
	svc := Service(a)
	id := skuID(a.Name)
	if currency == "" {
		currency = Currency
	}
	return &pb.Sku{
		Name:        svc.GetName() + "/skus/" + id,
		SkuId:       id,
		Description: svc.GetDisplayName() + " requests (local)",
		Category: &pb.Category{ServiceDisplayName: svc.GetDisplayName(), ResourceFamily: "ApiRequest",
			ResourceGroup: "Requests", UsageType: "OnDemand"},
		ServiceRegions:      []string{"global"},
		ServiceProviderName: "Google",
		GeoTaxonomy:         &pb.GeoTaxonomy{Type: pb.GeoTaxonomy_GLOBAL},
		PricingInfo: []*pb.PricingInfo{{
			EffectiveTime: timestamppb.New(c.Since),
			Summary:       "CloudBurrow is local: requests cost nothing",
			PricingExpression: &pb.PricingExpression{UsageUnit: "count", UsageUnitDescription: "count", BaseUnit: "count",
				BaseUnitDescription: "count", BaseUnitConversionFactor: 1, DisplayQuantity: 1,
				TieredRates: []*pb.PricingExpression_TierRate{{StartUsageAmount: 0, UnitPrice: &money.Money{CurrencyCode: currency}}}},
			AggregationInfo:        &pb.AggregationInfo{AggregationLevel: pb.AggregationInfo_ACCOUNT, AggregationInterval: pb.AggregationInfo_MONTHLY, AggregationCount: 1},
			CurrencyConversionRate: 1,
		}},
	}
}

// ListServices is services.list.
func (c *Catalog) ListServices(_ context.Context, req *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	if req.GetPageSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	byName := map[string]*pb.Service{}
	var keys []string
	for _, a := range c.apis() {
		s := Service(a)
		byName[s.GetName()] = s
		keys = append(keys, s.GetName())
	}
	size := int(req.GetPageSize())
	if size == 0 || size > 5000 {
		size = 5000
	}
	page, next, err := paging.Page("catalogServices", keys, req.GetPageToken(), size)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &pb.ListServicesResponse{NextPageToken: next}
	for _, k := range page {
		out.Services = append(out.Services, byName[k])
	}
	return out, nil
}

// ListSkus is services.skus.list. Only USD is priced locally: Google
// converts to a requested currency, and 0 is 0 in every currency, but a
// code that is not ISO 4217's form is refused.
func (c *Catalog) ListSkus(_ context.Context, req *pb.ListSkusRequest) (*pb.ListSkusResponse, error) {
	id, ok := strings.CutPrefix(req.GetParent(), "services/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid parent %q: want services/{service_id}", req.GetParent())
	}
	cur := req.GetCurrencyCode()
	if cur != "" && (len(cur) != 3 || strings.ToUpper(cur) != cur) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid currency_code %q", cur)
	}
	if req.GetStartTime() != nil && req.GetEndTime() != nil && req.GetEndTime().AsTime().Before(req.GetStartTime().AsTime()) {
		return nil, status.Error(codes.InvalidArgument, "end_time must not be before start_time")
	}
	if req.GetPageSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	for _, a := range c.apis() {
		if ServiceID(a.Name) != id {
			continue
		}
		sku := c.Sku(a, cur)
		page, next, err := paging.Page("catalogSkus|"+id, []string{sku.GetName()}, req.GetPageToken(), int(req.GetPageSize()))
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		out := &pb.ListSkusResponse{NextPageToken: next}
		if len(page) == 1 {
			out.Skus = []*pb.Sku{sku}
		}
		return out, nil
	}
	return nil, status.Errorf(codes.NotFound, "Service %s not found.", req.GetParent())
}
