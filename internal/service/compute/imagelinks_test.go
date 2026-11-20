package compute

import (
	"strings"
	"testing"
)

// gcloud compute instances create --image-family sends a zonal image
// family view as the source image, and Terraform recognises a boot disk's
// public image only under www.googleapis.com (#1965).
func TestImageFamilyViewsAndGoogleImageLinks(t *testing.T) {
	for _, ref := range []string{
		"projects/debian-cloud/zones/-/imageFamilyViews/debian-12",
		"https://compute.googleapis.com/compute/v1/projects/debian-cloud/zones/us-central1-a/imageFamilyViews/debian-12",
	} {
		if p, err := LookupImage(ref); err != nil || p.Family != "debian-12" {
			t.Errorf("%s: %v %v", ref, p.Family, err)
		}
	}
	if _, err := LookupImage("projects/debian-cloud/zones/nowhere-1/imageFamilyViews/debian-12"); err == nil {
		t.Error("a family view in a zone that does not exist was accepted")
	}
	out := render(obj{"sourceImage": "projects/debian-cloud/global/images/debian-12-bookworm-v20261119", "source": "projects/p/zones/z/disks/d"},
		"http://127.0.0.1:9031/compute/v1/")
	if out["sourceImage"] != "https://www.googleapis.com/compute/v1/projects/debian-cloud/global/images/debian-12-bookworm-v20261119" ||
		!strings.HasPrefix(out["source"].(string), "http://127.0.0.1:9031/") {
		t.Errorf("rendered: %v", out)
	}
}
