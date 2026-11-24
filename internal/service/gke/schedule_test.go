package gke

import (
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// The support dates and channels follow Google's release schedule.
func TestReleaseSchedule(t *testing.T) {
	std, ext := supportEnds("1.36.4-gke.0")
	if std != "2027-08-09T00:00:00Z" || ext != "2028-06-09T00:00:00Z" {
		t.Errorf("1.36's support ends %q and %q", std, ext)
	}
	if std, ext := supportEnds("1.99.0-gke.0"); std != "" || ext != "" {
		t.Errorf("a minor the schedule lacks has support ends %q, %q", std, ext)
	}
	versions := []string{"1.36.4-gke.0", "1.35.8-gke.0"}
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	// 2026-11-22: 1.36 is in every channel; Stable's clusters are
	// upgraded to it only in 2027 (an approximate date), Rapid's already.
	now := day("2026-11-22")
	if c := channelConfig(versions, containerpb.ReleaseChannel_STABLE, now); len(c.ValidVersions) != 2 || c.DefaultVersion != "1.36.4-gke.0" || c.UpgradeTargetVersion != "1.35.8-gke.0" {
		t.Errorf("Stable on %s: %v", now.Format(time.DateOnly), c)
	}
	if c := channelConfig(versions, containerpb.ReleaseChannel_RAPID, now); c.UpgradeTargetVersion != "1.36.4-gke.0" {
		t.Errorf("Rapid on %s: %v", now.Format(time.DateOnly), c)
	}
	// 2026-10-01: Stable does not have 1.36 yet.
	if c := channelConfig(versions, containerpb.ReleaseChannel_STABLE, day("2026-10-01")); len(c.ValidVersions) != 1 || c.DefaultVersion != "1.35.8-gke.0" {
		t.Errorf("Stable on 2026-10-01: %v", c)
	}
	// 2027-05-01: 1.35's standard support is over, except in Extended.
	if c := channelConfig(versions, containerpb.ReleaseChannel_REGULAR, day("2027-05-01")); len(c.ValidVersions) != 1 {
		t.Errorf("Regular on 2027-05-01: %v", c)
	}
	if c := channelConfig(versions, containerpb.ReleaseChannel_EXTENDED, day("2027-05-01")); len(c.ValidVersions) != 2 {
		t.Errorf("Extended on 2027-05-01: %v", c)
	}
}
