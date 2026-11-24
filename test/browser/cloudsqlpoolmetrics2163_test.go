//go:build browser

package browser

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLPoolingMetricsThroughThePage (#2163), in the sql shard: a
// PostgreSQL instance with managed connection pooling on offers the
// pooler's metrics on its Overview chart (Client connections, Server
// connections, Number of connection pools, Average wait time, Client
// connection attempts count, Error count), picked by keyboard from the
// Chart list and drawn from Cloud Monitoring's readings; an instance
// without pooling offers none of them. Metrics explorer lists
// per_database/conn_pool/client_connections under the Cloud SQL Instance
// Database resource and tables its active and waiting series for the
// instance's database. Each screen passes the WCAG 2.1 AA audit,
// keyboard included.
func TestCloudSQLPoolingMetricsThroughThePage(t *testing.T) {
	needService(t, "cloudsql-instances")
	needService(t, "monitoring-explorer")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	svc := sqlAdminSDK(t)
	name := fmt.Sprintf("browser-2163-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: "us-central1",
		RootPassword: "Root-pw-2163", Settings: &sqladmin.Settings{Tier: "db-perf-optimized-N-2", Edition: "ENTERPRISE_PLUS",
			ConnectionPoolConfig: &sqladmin.ConnectionPoolConfig{ConnectionPoolingEnabled: true}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)

	p := open(t)
	page := "/cloudsql-instances/" + name + q
	p.navigate(page)
	p.waitForWithin(`document.querySelector("#overview-chart-metric") !== null`, 2*time.Minute)
	var options []string
	p.eval(`[...document.querySelectorAll("#overview-chart-metric option")].map((o) => o.textContent)`, &options)
	for _, w := range []string{"Client connections (active)", "Client connections (waiting)", "Server connections (active)",
		"Server connections (idle)", "Number of connection pools", "Average wait time", "Client connection attempts count", "Error count"} {
		if !contains(options, w) {
			t.Errorf("the Overview's chart list is %q; want %s", options, w)
		}
	}
	for _, o := range options {
		if strings.HasPrefix(o, "Connection pool threads") {
			t.Errorf("a PostgreSQL instance's chart list offers MySQL's %q", o)
		}
	}

	// Pick Number of connection pools by keyboard: Tab to the list, then
	// arrow down to it.
	p.run(reducedMotion())
	p.tabTo(`a.id === "overview-chart-metric"`)
	for i := 0; i < len(options); i++ {
		var label string
		p.eval(`document.querySelector("#overview-chart-metric").selectedOptions[0].textContent`, &label)
		if label == "Number of connection pools" {
			break
		}
		p.run(chromedp.KeyEvent(kb.ArrowDown))
	}
	// The pooler's readings: one at the first sample, a line after the next.
	readings := `(() => { const c = document.querySelector(".overview-chart-card");
		return !!c && c.querySelector("h3").textContent === "Number of connection pools" &&
			(c.querySelector("svg .chart-line, svg .chart-point") !== null || c.textContent.includes("one reading so far")); })()`
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var ok bool
		p.eval(readings, &ok)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			var text string
			p.eval(`(document.querySelector(".overview-chart-card") || document.body).textContent`, &text)
			t.Fatalf("the Overview's Number of connection pools chart has no reading after 3 minutes: %q", text)
		}
		// The page reads the series when it is drawn: read it again.
		time.Sleep(15 * time.Second)
		p.navigate(page)
		p.waitFor(`document.querySelector("#overview-chart-metric") !== null`)
		p.setField("#overview-chart-metric", fmt.Sprint(indexOf(options, "Number of connection pools")))
	}

	// `up`'s postgres pools nothing: none of the pooler's charts.
	p.navigate("/cloudsql-instances/postgres" + q)
	p.waitForWithin(`document.querySelector("#overview-chart-metric") !== null`, 2*time.Minute)
	p.eval(`[...document.querySelectorAll("#overview-chart-metric option")].map((o) => o.textContent)`, &options)
	if contains(options, "Client connections (active)") {
		t.Errorf("an instance without pooling offers the pooler's charts: %q", options)
	}

	// Metrics explorer: the metric under its resource, one row a status.
	metric := "cloudsql.googleapis.com/per_database/conn_pool/client_connections"
	filter := fmt.Sprintf(`resource.labels.resource_id = "%s:%s"`, project, name)
	explorer := "/monitoring/metrics-explorer?project=" + url.QueryEscape(project) + "&resource=cloudsql_instance_database&metric=" +
		url.QueryEscape(metric) + "&filter=" + url.QueryEscape(filter)
	p.navigate(explorer)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#explorer-resource option")].some((o) => o.value === "cloudsql_instance_database") &&
		[...document.querySelectorAll("#explorer-metric option")].some((o) => o.value === %q)`, metric))
	p.waitForWithin(`document.querySelectorAll("#view .explorer-table tbody tr").length === 2`, time.Minute)
	var rows string
	p.eval(`[...document.querySelectorAll("#view .explorer-table tbody tr")].map((r) => r.textContent).join("|")`, &rows)
	if !strings.Contains(rows, "active") || !strings.Contains(rows, "waiting") {
		t.Errorf("the explorer's rows are %q; want the active and waiting series", rows)
	}

	auditScreens(t, []a11yPage{
		{"pooled instance Overview", page, settled + ` && document.querySelector("#overview-chart-metric") !== null`, ""},
		{"pooling metric in Metrics explorer", explorer, settled + ` && document.querySelectorAll("#view .explorer-table tbody tr").length === 2`, ""},
	})
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
