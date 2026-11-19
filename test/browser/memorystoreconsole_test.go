//go:build browser

package browser

import (
	"strings"
	"testing"
)

// TestMemorystoreConsoleFollowsGoogle (#1862), in the memorystore shard.
// Memorystore's menu is Google's: Valkey, Redis Cluster, Redis and
// Memcached, which says it is not available locally. Each list is titled
// "Memorystore for <product>". Create a Valkey instance is one page under
// Google's headings: Security is one choice whose IAM Auth checks and
// locks Enable In-Transit Encryption (TLS) and reveals Certificate
// authority management, where Customer-managed CA and Shared CA cannot be
// chosen, and Choose version draws Valkey 9.1 disabled. Create a Redis
// instance puts Tier before Region and ends in Create.
func TestMemorystoreConsoleFollowsGoogle(t *testing.T) {
	needService(t, "memorystore-valkey")
	p := open(t)

	p.navigate("/memorystore-valkey")
	p.waitFor(`document.querySelectorAll("#product-nav a").length >= 4`)
	var nav string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).join(",")`, &nav)
	if !strings.Contains(nav, "Valkey,Redis Cluster,Redis,Memcached") {
		t.Errorf("Memorystore's menu reads %q; want Valkey, Redis Cluster, Redis, Memcached", nav)
	}
	var title string
	p.eval(`document.querySelector("#view h1").textContent`, &title)
	if title != "Memorystore for Valkey" {
		t.Errorf("the Valkey list is titled %q", title)
	}

	p.navigate("/memorystore-memcached")
	p.waitFor(`document.querySelector("#view").textContent.includes("Not available locally")`)

	// Create a Valkey instance.
	p.navigate("/memorystore-valkey")
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === "Create instance")`)
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector("form.create-page #f-id") !== null`)
	var form struct {
		Title, Headings, Versions string
		CANotShown, TLSFree       bool
	}
	p.eval(`(() => ({
		Title: document.querySelector("#view h1").textContent,
		Headings: [...document.querySelectorAll("form.create-page .form-section-title")].map((h) => h.textContent).join(","),
		Versions: [...document.querySelectorAll("form.create-page [name='f-engineVersion-choice']")].map((r) => r.value + (r.disabled ? ":off" : "")).join(","),
		CANotShown: document.querySelector("form.create-page #f-caMode").closest(".form-row").hidden,
		TLSFree: !document.querySelector("form.create-page #f-transitEncryption").disabled,
	}))()`, &form)
	if form.Title != "Create a Valkey instance" {
		t.Errorf("the create page is titled %q", form.Title)
	}
	if want := "Set up connection,Security,Certificate authority management,Encryption,ACL policy,Data persistence strategy,Automated backups,Maintenance,Choose version"; !strings.HasPrefix(form.Headings, want) {
		t.Errorf("the create page's headings are %q; want %q first", form.Headings, want)
	}
	if form.Versions != "VALKEY_7_2,VALKEY_8_0,VALKEY_9_0,VALKEY_9_1:off" {
		t.Errorf("Choose version = %q; want 7.2, 8.0, 9.0 and 9.1 not available", form.Versions)
	}
	if !form.CANotShown || !form.TLSFree {
		t.Errorf("with Security Disabled: CA management hidden %v, TLS free %v; want both", form.CANotShown, form.TLSFree)
	}
	p.clickText("form.create-page label", "IAM Auth")
	var held struct{ TLS, CA, Shared bool }
	p.eval(`(() => {
		const b = document.querySelector("form.create-page #f-transitEncryption");
		const shared = [...document.querySelectorAll("form.create-page [name='f-caMode-choice']")].find((r) => r.value === "SHARED");
		return { TLS: b.checked && b.disabled, CA: !document.querySelector("form.create-page #f-caMode").closest(".form-row").hidden,
			Shared: shared.disabled };
	})()`, &held)
	if !held.TLS || !held.CA || held.Shared {
		t.Errorf("IAM Auth: TLS checked and locked %v, CA management shown %v, Shared CA disabled %v; want TLS locked, CA management shown and Shared CA choosable (#1918)", held.TLS, held.CA, held.Shared)
	}

	// Create a Redis instance.
	p.navigate("/memorystore")
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === "Create instance")`)
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector("form.create-page #f-id") !== null`)
	var redis struct{ Title, Order, Submit string }
	p.eval(`(() => ({
		Title: document.querySelector("#view h1").textContent,
		Order: [...document.querySelectorAll("form.create-page label, form.create-page legend")].map((l) => l.textContent.replace(/\s*\*$/, "").trim())
			.filter((l) => ["Tier", "Region", "Version", "Capacity"].includes(l)).join(","),
		Submit: document.querySelector("form.create-page button[type=submit]").textContent,
	}))()`, &redis)
	if redis.Title != "Create a Redis instance" || redis.Order != "Tier,Region,Version,Capacity" || redis.Submit != "Create" {
		t.Errorf("Create a Redis instance = %+v; want Google's title, Tier, Region, Version, Capacity, and Create", redis)
	}
}

// memorystoreTabsText is the text of a Memorystore page over every tab
// (#1862: Overview, Configuration, Connections, ...), each opened in turn.
func memorystoreTabsText(p *tab) string {
	p.t.Helper()
	var text string
	p.eval(`(() => {
		let out = document.querySelector("#view").textContent;
		for (const t of document.querySelectorAll("#view [role=tab]")) {
			t.click();
			out += " " + [...document.querySelectorAll("#view [role=tabpanel]")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ");
		}
		return out;
	})()`, &text)
	return text
}
