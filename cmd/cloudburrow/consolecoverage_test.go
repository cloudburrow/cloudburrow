package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// consoleCoverage is where each selectable service is in the console, which
// is what console parity §3 records: "the console covers exactly what
// CloudBurrow supports". Every service in config.KnownServices() has exactly
// one entry, and TestEveryServiceHasAParityRow holds it to the registry and
// to §3, so a service cannot be enabled, titled on the dashboard and have no
// screen without a documented reason.
type consoleCoverage struct {
	// Providers are the IDs of the screens built for the service.
	Providers []string
	// Screen is a console screen that is not a provider but shows the
	// service's data, such as the Logs Explorer for Cloud Logging.
	Screen string
	// Excluded is why the service has no screen at all. §3 carries the same
	// reason in the service's row.
	Excluded string
}

var consoleCoverages = map[config.Service]consoleCoverage{
	config.ServiceStorage:   {Providers: []string{"storage", "storage-deleted", "storage-settings"}},
	config.ServicePubSub:    {Providers: []string{"pubsub", "pubsub-subscriptions", "pubsub-snapshots", "pubsub-schemas"}},
	config.ServiceTasks:     {Providers: []string{"tasks"}},
	config.ServiceRun:       {Providers: []string{"run", "run-jobs"}},
	config.ServiceSecrets:   {Providers: []string{"secrets"}},
	config.ServiceFirestore: {Providers: []string{"firestore"}},
	config.ServiceDatastore: {Providers: []string{"datastore"}},
	config.ServiceBigtable:  {Providers: []string{"bigtable"}},
	config.ServiceSpanner:   {Providers: []string{"spanner"}},
	config.ServiceCloudSQL:  {Providers: []string{"cloudsql"}},
	config.ServiceBigQuery:  {Providers: []string{"bigquery"}},
	config.ServiceScheduler: {Providers: []string{"scheduler"}},
	config.ServiceKMS:       {Providers: []string{"kms"}},
	config.ServiceLogging:   {Screen: "/logs"},
	config.ServiceMemorystore: {Excluded: "Memorystore has no admin API here: the backend is a " +
		"Valkey server, and the console reads only through a service's own API"},
	config.ServiceCloudSQLMySQL: {Providers: []string{"cloudsql-mysql"}},
}

// clusterScreens are the registry's screens that belong to no selectable
// service: Resource Manager, the AI catalogue and CloudBurrow's own cluster.
var clusterScreens = []string{
	"projects", "ai",
	"workloads", "pods", "k8sservices", "jobs", "nodes", "k8sstorage", "events",
}

// TestEveryServiceHasAParityRow enforces console parity §3 (#698): "The
// console covers exactly what CloudBurrow supports."
//
// The dashboard titles every service in config.KnownServices() from
// serviceTitles and shows an enabled one as enabled, so a service with a
// working backend and no screen read as "not implemented" — BigQuery and
// Memorystore were exactly that, with no §3 row either. Here every service
// must be one of: built by the registry (with every service enabled and every
// tunnel present), shown on a non-provider screen, or excluded with a reason;
// and §3 must have a row titled as the dashboard titles it, which says
// "**No screen**" exactly when the service is excluded.
func TestEveryServiceHasAParityRow(t *testing.T) {
	d := coverageDeps(t)
	registry := map[string]bool{}
	for _, p := range consoleProviders(d, clusterMetrics(d.cfg.KubeconfigPath()), console.NewSeries(console.SeriesLimit, nil)) {
		registry[p.ID()] = true
	}

	rows := paritySection3(t)
	claimed := map[string]bool{}
	for _, s := range config.KnownServices() {
		title, ok := serviceTitles[s]
		if !ok {
			t.Errorf("%s has no entry in serviceTitles", s)
			continue
		}
		cov, ok := consoleCoverages[s]
		if !ok {
			t.Errorf("%s (%s) has no entry in consoleCoverages: give it a screen or a documented exclusion", s, title)
			continue
		}
		kinds := 0
		for _, set := range []bool{len(cov.Providers) > 0, cov.Screen != "", cov.Excluded != ""} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			t.Errorf("%s: set exactly one of Providers, Screen and Excluded, got %+v", s, cov)
		}
		for _, id := range cov.Providers {
			claimed[id] = true
			if !registry[id] {
				t.Errorf("%s claims the %q screen, which consoleProviders does not build with every service enabled", s, id)
			}
		}
		if cov.Excluded != "" && registry[string(s)] {
			t.Errorf("%s is excluded, but the registry builds a %q screen", s, s)
		}

		row, ok := rows[title]
		if !ok {
			t.Errorf("console-parity.md §3 has no row for %q (%s)", title, s)
			continue
		}
		noScreen := strings.HasPrefix(row, "**No screen**")
		switch {
		case cov.Excluded != "" && !noScreen:
			t.Errorf("§3 row %q must begin **No screen**, since %s is excluded: %s", title, s, cov.Excluded)
		case cov.Excluded == "" && noScreen:
			t.Errorf("§3 row %q says **No screen**, but %s has one", title, s)
		}
	}
	for id := range registry {
		if !claimed[id] && !slices.Contains(clusterScreens, id) {
			t.Errorf("the registry builds %q, which no service in consoleCoverages claims", id)
		}
	}
}

// coverageDeps is an instance with every service enabled and a tunnel for
// each, so every screen the registry can build is built. Nothing is served:
// the registry is only constructed, never read.
func coverageDeps(t *testing.T) consoleDeps {
	t.Helper()
	cfg := config.Config{
		Name: "coverage", Project: "coverage", BindAddress: "127.0.0.1",
		StateDir: t.TempDir(), Mode: config.ModeEphemeral,
		Services: config.KnownServices(),
	}
	d := consoleDeps{
		cfg:       cfg,
		projects:  resourcemanager.New(store.NewMemory()),
		tasks:     &tasksService{},
		secrets:   &secretsService{},
		kms:       &kmsService{},
		scheduler: &schedulerService{},
	}
	for _, s := range config.KnownServices() {
		d.forwarders = append(d.forwarders, namedForwarder(t, string(s), "127.0.0.1:1"))
	}
	return d
}

// paritySection3 returns §3's table as the "In scope" cell of each row, keyed
// by every name in its Area cell ("Firestore / Datastore" is two).
func paritySection3(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../docs/console-parity.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "\n## 3.")
	if start < 0 {
		t.Fatal("console-parity.md has no §3")
	}
	section := doc[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	rows := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), " | ")
		if len(cells) < 2 || strings.TrimSpace(cells[0]) == "Area" {
			continue
		}
		for _, name := range strings.Split(cells[0], " / ") {
			rows[strings.TrimSpace(name)] = strings.TrimSpace(cells[1])
		}
	}
	if len(rows) == 0 {
		t.Fatal("console-parity.md §3 has no table rows")
	}
	return rows
}
