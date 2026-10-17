package run

import (
	"sort"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ListDockerImages' order_by (#1304): "update_time desc", as Terraform's
// google_artifact_registry_docker_image sends it, puts the newest first;
// several keys break ties in turn; an unknown field or direction is
// refused INVALID_ARGUMENT; an empty order keeps the listing's.
func TestDockerImageOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	im := func(name string, upd time.Duration, size int64) *arpb.DockerImage {
		return &arpb.DockerImage{Name: name, UpdateTime: timestamppb.New(t0.Add(upd)), ImageSizeBytes: size}
	}
	names := func(l []*arpb.DockerImage) string {
		s := ""
		for _, x := range l {
			s += x.GetName()
		}
		return s
	}
	for order, want := range map[string]string{
		"update_time desc":                "cab",
		"update_time":                     "bac",
		"image_size_bytes, name desc":     "cba",
		"image_size_bytes asc,name":       "bca",
		" name  DESC ":                    "cba",
		"upload_time,build_time,name asc": "abc",
	} {
		l := []*arpb.DockerImage{im("a", time.Hour, 2), im("b", 0, 1), im("c", 2*time.Hour, 1)}
		less, err := dockerImageOrder(order)
		if err != nil || less == nil {
			t.Fatalf("%q: %v", order, err)
		}
		sort.SliceStable(l, func(i, j int) bool { return less(l[i], l[j]) })
		if got := names(l); got != want {
			t.Errorf("order_by %q = %s; want %s", order, got, want)
		}
	}
	if less, err := dockerImageOrder(""); less != nil || err != nil {
		t.Errorf("an empty order_by = %v, %v; want the listing's order", less != nil, err)
	}
	for _, bad := range []string{"tags", "name sideways", "name desc extra", "name,,"} {
		if _, err := dockerImageOrder(bad); err == nil {
			t.Errorf("order_by %q was accepted", bad)
		}
	}
}
