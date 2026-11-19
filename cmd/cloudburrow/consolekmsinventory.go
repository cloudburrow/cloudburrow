package main

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// kmsInventoryProvider is Key Management's Key inventory page (#1851): every
// key of the project, across its key rings and locations, with the
// properties Google's page lists ("View keys by project",
// https://docs.cloud.google.com/kms/docs/view-keys-by-project): Key name,
// Status, Key ring, Location, Current rotation, Rotation frequency, Next
// rotation, Protection level, EKM via VPC connection, Purpose and Labels.
// Each row opens the key's page under Key management.
type kmsInventoryProvider struct{ svc *kmsService }

func (kmsInventoryProvider) ID() string    { return "kms-inventory" }
func (kmsInventoryProvider) Title() string { return "Key inventory" }

// kmsInventoryColumns are Google's, in the docs' order.
var kmsInventoryColumns = []string{"Key ring", "Location", "Current rotation", "Rotation frequency", "Next rotation",
	"Protection level", "EKM via VPC connection", "Purpose", "Labels"}

func (p kmsInventoryProvider) List(ctx context.Context, project string) (console.Listing, error) {
	api := p.svc.API()
	if api == nil {
		return console.Listing{}, errors.New(kmsNotStarted)
	}
	listing := console.Listing{Columns: kmsInventoryColumns, NameColumn: "Key name", Noun: "keys", AlwaysStatus: true}
	if project == "" {
		listing.Prompt = "Key inventory lists a project's keys. Choose one in the toolbar."
		return listing, nil
	}
	rings, err := api.KeyRingsInProject(project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, r := range rings {
		resp, err := api.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: r.GetName(), PageSize: detailLimit})
		if err != nil {
			continue
		}
		for _, k := range resp.GetCryptoKeys() {
			listing.Items = append(listing.Items, kmsInventoryRow(project, r.GetName(), k))
		}
	}
	listing.Total = len(listing.Items)
	listing.Note = "Status applies to symmetric keys, as on Google's page: Available when the primary version is enabled, " +
		"Not available when it is disabled or there is none. Google's key usage summaries come from the Cloud KMS " +
		"Inventory API, which CloudBurrow does not serve."
	return listing, nil
}

// kmsInventoryRow is one key's row.
func kmsInventoryRow(project, ring string, k *kmspb.CryptoKey) console.Resource {
	status := ""
	if k.GetPurpose() == kmspb.CryptoKey_ENCRYPT_DECRYPT {
		status = "Not available"
		if k.GetPrimary().GetState() == kmspb.CryptoKeyVersion_ENABLED {
			status = "Available"
		}
	}
	current := "—"
	if pv := k.GetPrimary(); pv != nil {
		current = kmsTime(pv.GetCreateTime())
	}
	frequency := "Never (manual rotation)"
	if rp := k.GetRotationPeriod(); rp != nil {
		frequency = kmsPeriodWords(rp.AsDuration())
	}
	ekm := ""
	if k.GetVersionTemplate().GetProtectionLevel() == kmspb.ProtectionLevel_EXTERNAL_VPC {
		ekm = k.GetCryptoKeyBackend()
	}
	labels := make([]string, 0, len(k.GetLabels()))
	for key, v := range k.GetLabels() {
		labels = append(labels, key+": "+v)
	}
	sort.Strings(labels)
	return console.Resource{
		Name:   lastSegment(k.GetName()),
		Status: status,
		Link:   "/kms/keyrings/" + kmsPathSegment(ring) + "/" + kmsPathSegment(lastSegment(k.GetName())) + "?project=" + url.QueryEscape(project),
		Fields: map[string]string{
			"Key ring":               lastSegment(ring),
			"Location":               kmsLocation(ring),
			"Current rotation":       current,
			"Rotation frequency":     frequency,
			"Next rotation":          kmsTime(k.GetNextRotationTime()),
			"Protection level":       kmsProtectionWords(k.GetVersionTemplate().GetProtectionLevel().String()),
			"EKM via VPC connection": ekm,
			"Purpose":                kmsPurposeWords(k.GetPurpose().String()),
			"Labels":                 strings.Join(labels, ", "),
		},
	}
}

// kmsPathSegment escapes one segment of a console address as the console's
// own detailHref does (encodeURIComponent): a ring's resource name keeps
// its slashes inside the one segment.
func kmsPathSegment(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

var _ console.Provider = kmsInventoryProvider{}
