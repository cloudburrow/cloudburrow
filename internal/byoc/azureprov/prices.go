package azureprov

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// presetSizes are the wizard's presets as Azure VM sizes (Microsoft Learn,
// "Sizes for virtual machines in Azure"): the general-purpose Dsv5 series,
// and for the GPU preset the NCasT4_v3 series. Azure lists no VM with an
// NVIDIA L4, so the GPU preset is the nearest single-GPU inference size
// that runs NVIDIA's standard data-center driver: one NVIDIA T4.
var presetSizes = []byoc.Size{
	{Preset: byoc.Small, Name: "Standard_D4s_v5", VCPUs: 4, MemoryGiB: 16},
	{Preset: byoc.Medium, Name: "Standard_D16s_v5", VCPUs: 16, MemoryGiB: 64},
	{Preset: byoc.Large, Name: "Standard_D32s_v5", VCPUs: 32, MemoryGiB: 128},
	{Preset: byoc.GPU, Name: "Standard_NC4as_T4_v3", VCPUs: 4, MemoryGiB: 28, GPU: "NVIDIA T4", GPUs: 1,
		Note: "Azure offers no NVIDIA L4 VM; this is the nearest single-GPU size: one NVIDIA T4 (16 GB), 4 vCPUs, 28 GiB."},
}

// SizeFor is the preset's VM size.
func SizeFor(p byoc.Preset) (byoc.Size, bool) {
	for _, s := range presetSizes {
		if s.Preset == p {
			return s, true
		}
	}
	return byoc.Size{}, false
}

// sizeByName is a known size by its VM size name.
func sizeByName(name string) (byoc.Size, bool) {
	for _, s := range presetSizes {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return byoc.Size{}, false
}

// DiskGiB is the default OS disk: 128 GiB Premium SSD, a P10.
const DiskGiB = 128

// hoursPerMonth is how the Retail Prices API's monthly disk price becomes
// hourly: Azure bills managed disks per month of 730 hours.
const hoursPerMonth = 730

type priceItem struct {
	CurrencyCode  string  `json:"currencyCode"`
	RetailPrice   float64 `json:"retailPrice"`
	UnitPrice     float64 `json:"unitPrice"`
	ArmRegionName string  `json:"armRegionName"`
	ProductName   string  `json:"productName"`
	SkuName       string  `json:"skuName"`
	MeterName     string  `json:"meterName"`
	ServiceName   string  `json:"serviceName"`
	ArmSkuName    string  `json:"armSkuName"`
	Type          string  `json:"type"`
	UnitOfMeasure string  `json:"unitOfMeasure"`
}

type pricePage struct {
	Items        []priceItem `json:"Items"`
	NextPageLink string      `json:"NextPageLink"`
}

// prices reads every item filter selects, following NextPageLink.
func (p *Provider) prices(ctx context.Context, filter string) ([]priceItem, error) {
	u := p.opts.PricesURL + "?" + url.Values{"$filter": {filter}}.Encode()
	var out []priceItem
	for page := 0; u != "" && page < 20; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := p.opts.HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("the Azure Retail Prices API: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("the Azure Retail Prices API answered %s", resp.Status)
		}
		var pg pricePage
		if err := json.Unmarshal(body, &pg); err != nil {
			return nil, fmt.Errorf("the Azure Retail Prices API: %w", err)
		}
		out = append(out, pg.Items...)
		u = pg.NextPageLink
	}
	return out, nil
}

// vmHourly is a Linux pay-as-you-go VM size's price per hour in region.
func (p *Provider) vmHourly(ctx context.Context, region, sku string) (float64, error) {
	items, err := p.prices(ctx, fmt.Sprintf("serviceName eq 'Virtual Machines' and armRegionName eq '%s' and armSkuName eq '%s' and priceType eq 'Consumption'", region, sku))
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		// Windows meters carry the licence; Spot and Low Priority are
		// evictable capacity, not what Create asks for.
		if strings.Contains(it.ProductName, "Windows") || strings.Contains(it.SkuName, "Spot") ||
			strings.Contains(it.SkuName, "Low Priority") || it.CurrencyCode != "USD" || it.UnitOfMeasure != "1 Hour" {
			continue
		}
		return it.RetailPrice, nil
	}
	return 0, fmt.Errorf("the Azure Retail Prices API lists no Linux pay-as-you-go price for %s in %s", sku, region)
}

// diskHourly is a P10 Premium SSD's price per hour in region.
func (p *Provider) diskHourly(ctx context.Context, region string) (float64, error) {
	items, err := p.prices(ctx, fmt.Sprintf("serviceName eq 'Storage' and armRegionName eq '%s' and productName eq 'Premium SSD Managed Disks' and skuName eq 'P10 LRS' and priceType eq 'Consumption'", region))
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		if it.CurrencyCode == "USD" && strings.Contains(it.MeterName, "Disk") && strings.Contains(it.UnitOfMeasure, "Month") {
			return it.RetailPrice / hoursPerMonth, nil
		}
	}
	return 0, fmt.Errorf("the Azure Retail Prices API lists no P10 disk price in %s", region)
}

// Sizes implements byoc.Provider.
func (p *Provider) Sizes(ctx context.Context, region string) ([]byoc.Size, error) {
	if region == "" {
		return nil, fmt.Errorf("sizes: no region")
	}
	disk, diskErr := p.diskHourly(ctx, region)
	out := make([]byoc.Size, 0, len(presetSizes))
	for _, s := range presetSizes {
		s.PriceSource = "Azure Retail Prices API, Linux pay-as-you-go, " + region
		if h, err := p.vmHourly(ctx, region, s.Name); err != nil {
			s.PriceError = err.Error()
		} else {
			s.HourlyUSD = h
		}
		if diskErr == nil {
			s.DiskHourlyUSD = disk
		}
		out = append(out, s)
	}
	return out, nil
}
