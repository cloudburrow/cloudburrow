package gke

import (
	_ "embed"
	"encoding/json"
	"slices"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
)

// GKE's release schedule (#2100): when each minor version reaches each
// release channel, when the channel's clusters are automatically upgraded
// to it, and when its standard and extended support end, as Google
// publishes them (releaseschedule.json names the page and the day it was
// read). ServerConfig's channels follow it, and the upgrade information
// reports the end of support of a cluster's and a pool's minor version.

//go:embed releaseschedule.json
var releaseScheduleJSON []byte

// scheduleMinor is one minor version's row.
type scheduleMinor struct {
	Minor                string   `json:"minor"`
	Approximate          []string `json:"approximate"`
	RapidAvailable       string   `json:"rapidAvailable"`
	RapidAutoUpgrade     string   `json:"rapidAutoUpgrade"`
	RegularAvailable     string   `json:"regularAvailable"`
	RegularAutoUpgrade   string   `json:"regularAutoUpgrade"`
	StableAvailable      string   `json:"stableAvailable"`
	StableAutoUpgrade    string   `json:"stableAutoUpgrade"`
	ExtendedAvailable    string   `json:"extendedAvailable"`
	ExtendedAutoUpgrade  string   `json:"extendedAutoUpgrade"`
	EndOfStandardSupport string   `json:"endOfStandardSupport"`
	EndOfExtendedSupport string   `json:"endOfExtendedSupport"`
}

var releaseSchedule = func() map[string]scheduleMinor {
	var doc struct {
		Minors []scheduleMinor `json:"minors"`
	}
	if err := json.Unmarshal(releaseScheduleJSON, &doc); err != nil {
		panic("gke: releaseschedule.json: " + err.Error())
	}
	out := map[string]scheduleMinor{}
	for _, m := range doc.Minors {
		out[m.Minor] = m
	}
	return out
}()

// scheduleDate is a field's date, false when it is not an exact one (a
// month, a quarter, or a date the page marks approximate).
func (m scheduleMinor) date(field, v string) (time.Time, bool) {
	if v == "" || slices.Contains(m.Approximate, field) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.DateOnly, v)
	return t, err == nil
}

// supportEnds are the end of standard and of extended support of version
// v's minor, as the upgrade information gives them (RFC 3339 at midnight
// UTC: the page gives days), "" when the schedule has no exact date.
func supportEnds(v string) (standard, extended string) {
	m, ok := releaseSchedule[minorVersion(v)]
	if !ok {
		return "", ""
	}
	if t, ok := m.date("endOfStandardSupport", m.EndOfStandardSupport); ok {
		standard = t.UTC().Format(time.RFC3339)
	}
	if t, ok := m.date("endOfExtendedSupport", m.EndOfExtendedSupport); ok {
		extended = t.UTC().Format(time.RFC3339)
	}
	return standard, extended
}

// channelDates are a channel's availability and auto-upgrade fields.
func channelDates(m scheduleMinor, ch containerpb.ReleaseChannel_Channel) (availField, avail, autoField, auto string) {
	switch ch {
	case containerpb.ReleaseChannel_RAPID:
		return "rapidAvailable", m.RapidAvailable, "rapidAutoUpgrade", m.RapidAutoUpgrade
	case containerpb.ReleaseChannel_STABLE:
		return "stableAvailable", m.StableAvailable, "stableAutoUpgrade", m.StableAutoUpgrade
	case containerpb.ReleaseChannel_EXTENDED:
		return "extendedAvailable", m.ExtendedAvailable, "extendedAutoUpgrade", m.ExtendedAutoUpgrade
	}
	return "regularAvailable", m.RegularAvailable, "regularAutoUpgrade", m.RegularAutoUpgrade
}

// channelConfig is a release channel's configuration on day now, from the
// versions served (newest first): its valid versions are those of the
// minors available in it by then (all of them when the schedule has no
// row for a minor, or before any is available); its default the newest
// valid one, as new clusters are made on it; its upgrade target the newest
// of the minors its clusters are automatically upgraded to by an exact
// date passed (the default when none is). A minor past the
// end of standard support stays valid only in the Extended channel.
func channelConfig(versions []string, ch containerpb.ReleaseChannel_Channel, now time.Time) *containerpb.ServerConfig_ReleaseChannelConfig {
	var valid, auto []string
	for _, v := range versions {
		m, ok := releaseSchedule[minorVersion(v)]
		if !ok {
			valid = append(valid, v)
			continue
		}
		if ch != containerpb.ReleaseChannel_EXTENDED {
			if end, ok := m.date("endOfStandardSupport", m.EndOfStandardSupport); ok && !now.Before(end) {
				continue
			}
		}
		af, a, uf, u := channelDates(m, ch)
		if t, ok := m.date(af, a); ok && now.Before(t) {
			continue
		}
		valid = append(valid, v)
		if t, ok := m.date(uf, u); ok && !now.Before(t) {
			auto = append(auto, v)
		}
	}
	if len(valid) == 0 {
		valid = versions
	}
	def, target := valid[0], valid[0]
	if len(auto) > 0 {
		target = auto[0]
	}
	return &containerpb.ServerConfig_ReleaseChannelConfig{Channel: ch, DefaultVersion: def, ValidVersions: valid, UpgradeTargetVersion: target}
}
