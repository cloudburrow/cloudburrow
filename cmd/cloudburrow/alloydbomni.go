package main

// The AlloyDB Omni engine, opt-in (#2207, internal/service/alloydb's
// omni.go): a cluster whose record says who accepted Google's AlloyDB Omni
// licence terms runs Google's google/alloydbomni image of its major
// version; every other cluster runs PostgreSQL, as before.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// omniTermsOf is the acceptance of the AlloyDB Omni terms cfg was started
// with (`up --accept-alloydb-omni-terms`,
// CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y or a named configuration file's
// alloydb.acceptOmniTerms), recorded as made by the user running `up`,
// now; nil when it was not.
func omniTermsOf(cfg config.Config, now string) *alloydb.OmniAcceptance {
	if !cfg.AlloyDB.AcceptOmniTerms {
		return nil
	}
	via := alloydb.OmniViaFile
	switch cfg.Source.OmniTerms {
	case "flag":
		via = alloydb.OmniViaFlag
	case "env":
		via = alloydb.OmniViaEnv
	}
	return &alloydb.OmniAcceptance{AcceptedBy: sqladmin.LocalUser(), AcceptedAt: now, Via: via}
}

// omniTermsFile is where `up` records the acceptance it was started with,
// in the instance's directory, for `status`; it is removed when `up` is
// started without one.
const omniTermsFile = "alloydb-omni-terms.json"

func recordOmniTerms(cfg config.Config, a *alloydb.OmniAcceptance) error {
	path := filepath.Join(cfg.InstanceDir(), omniTermsFile)
	if a == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// omniTermsStatus is what `status` says of the AlloyDB Omni terms: the
// acceptance `up` was started with, and that of each cluster that runs
// Omni.
type omniTermsStatus struct {
	Up       *alloydb.OmniAcceptance            `json:"up,omitempty"`
	Clusters map[string]*alloydb.OmniAcceptance `json:"clusters,omitempty"`
}

func readOmniTermsStatus(cfg config.Config) omniTermsStatus {
	var st omniTermsStatus
	if b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), omniTermsFile)); err == nil {
		var a alloydb.OmniAcceptance
		if json.Unmarshal(b, &a) == nil {
			st.Up = &a
		}
	}
	// A persistent instance's clusters (alloydb.json, the API's records).
	if b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), "alloydb.json")); err == nil {
		var recs struct {
			Omni map[string]*alloydb.OmniAcceptance `json:"omni"`
		}
		if json.Unmarshal(b, &recs) == nil && len(recs.Omni) > 0 {
			st.Clusters = recs.Omni
		}
	}
	return st
}

// omniTermsLines are status's lines for st.
func omniTermsLines(st omniTermsStatus) []string {
	var out []string
	if st.Up != nil {
		out = append(out, fmt.Sprintf("AlloyDB Omni terms: accepted by %s at %s via %s", st.Up.AcceptedBy, st.Up.AcceptedAt, st.Up.Via))
	} else {
		out = append(out, "AlloyDB Omni terms: not accepted; AlloyDB clusters run PostgreSQL "+
			"(cloudburrow up --accept-alloydb-omni-terms accepts them for every new cluster)")
	}
	names := make([]string, 0, len(st.Clusters))
	for n := range st.Clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if a := st.Clusters[n]; a.Accepted() {
			out = append(out, fmt.Sprintf("  %s: runs AlloyDB Omni, accepted by %s at %s via %s", n, a.AcceptedBy, a.AcceptedAt, a.Via))
		}
	}
	return out
}

// omniTermsReport is status --format json's alloydb_omni_terms: nil when
// there is no acceptance to report.
func omniTermsReport(cfg config.Config) *omniTermsStatus {
	st := readOmniTermsStatus(cfg)
	if st.Up == nil && len(st.Clusters) == 0 {
		return nil
	}
	return &st
}

// omniEngineLabel is the console's Database engine of a cluster that runs
// Omni under a (nil: PostgreSQL).
func omniEngineLabel(a *alloydb.OmniAcceptance) string {
	if !a.Accepted() {
		return "PostgreSQL (AlloyDB Omni's terms were not accepted for this cluster)"
	}
	return fmt.Sprintf("AlloyDB Omni (terms accepted by %s at %s via %s)", a.AcceptedBy, a.AcceptedAt, a.Via)
}
