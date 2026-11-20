package aipfilter

import (
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func jobs() []*schedulerpb.Job {
	t0 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	return []*schedulerpb.Job{
		{
			Name: "projects/p/locations/us-central1/jobs/alpha-prod", Description: "Nightly Backup",
			State: schedulerpb.Job_ENABLED, Schedule: "0 1 * * *", TimeZone: "UTC",
			UserUpdateTime: timestamppb.New(t0),
			RetryConfig:    &schedulerpb.RetryConfig{RetryCount: 3, MaxRetryDuration: durationpb.New(30 * time.Second)},
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
				Uri: "https://example.com/a", Headers: map[string]string{"env": "prod", "team": "core"},
			}},
		},
		{
			Name: "projects/p/locations/us-central1/jobs/beta-dev", Description: "hourly ping",
			State: schedulerpb.Job_PAUSED, Schedule: "0 * * * *", TimeZone: "Europe/Paris",
			UserUpdateTime: timestamppb.New(t0.Add(48 * time.Hour)),
			RetryConfig:    &schedulerpb.RetryConfig{RetryCount: 0, MaxRetryDuration: durationpb.New(2 * time.Minute)},
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
				Uri: "https://example.com/b", Headers: map[string]string{"env": "dev"},
			}},
		},
		{
			Name: "projects/p/locations/europe-west1/jobs/gamma", Description: "",
			State: schedulerpb.Job_DISABLED, Schedule: "*/5 * * * *",
		},
	}
}

func names(t *testing.T, filter string) string {
	t.Helper()
	got, err := Apply(filter, jobs())
	if err != nil {
		t.Fatalf("Apply(%q): %v", filter, err)
	}
	var out []string
	for _, j := range got {
		out = append(out, j.GetName()[strings.LastIndexByte(j.GetName(), '/')+1:])
	}
	return strings.Join(out, ",")
}

func TestFilterMatches(t *testing.T) {
	for _, tc := range []struct{ filter, want string }{
		{"", "alpha-prod,beta-dev,gamma"},
		{"   ", "alpha-prod,beta-dev,gamma"},
		{`state = ENABLED`, "alpha-prod"},
		{`state=ENABLED`, "alpha-prod"},
		{`state != ENABLED`, "beta-dev,gamma"},
		{`state = "PAUSED"`, "beta-dev"},
		{`state:(ENABLED OR PAUSED)`, "alpha-prod,beta-dev"},
		{`state = ENABLED OR state = DISABLED`, "alpha-prod,gamma"},
		{`NOT state = ENABLED`, "beta-dev,gamma"},
		{`-state = ENABLED`, "beta-dev,gamma"},
		{`NOT (state = ENABLED OR state = PAUSED)`, "gamma"},
		{`name = "*-prod"`, "alpha-prod"},
		{`name = "projects/p/locations/us-central1/*"`, "alpha-prod,beta-dev"},
		{`name = projects/p/locations/europe-west1/jobs/gamma`, "gamma"},
		{`name:beta`, "beta-dev"},
		{`description:backup`, "alpha-prod"},
		{`description:*`, "alpha-prod,beta-dev"},
		{`description = ""`, "gamma"},
		{`timeZone = UTC`, "alpha-prod"},
		{`time_zone = "Europe/Paris"`, "beta-dev"},
		{`retry_config.retry_count > 0`, "alpha-prod"},
		{`retry_config.retry_count >= 0`, "alpha-prod,beta-dev,gamma"},
		{`retry_config.retry_count <= 0`, "beta-dev,gamma"},
		{`retry_config.retry_count < 3.5`, "alpha-prod,beta-dev,gamma"},
		{`retry_config:*`, "alpha-prod,beta-dev"},
		{`retry_config.max_retry_duration > "60s"`, "beta-dev"},
		{`retry_config.max_retry_duration <= 30s`, "alpha-prod"},
		{`retry_config.max_retry_duration = 2m`, "beta-dev"},
		{`user_update_time > "2024-01-03T00:00:00Z"`, "beta-dev"},
		{`user_update_time < 2024-01-03T00:00:00Z`, "alpha-prod"},
		{`user_update_time >= 2024-01-02`, "alpha-prod,beta-dev"},
		{`user_update_time:*`, "alpha-prod,beta-dev"},
		{`http_target.headers.env = prod`, "alpha-prod"},
		{`http_target.headers:team`, "alpha-prod"},
		{`http_target.headers:*`, "alpha-prod,beta-dev"},
		{`http_target.uri = "https://example.com/*"`, "alpha-prod,beta-dev"},
		{`http_target.headers.env:*`, "alpha-prod,beta-dev"},
		{`state = ENABLED AND retry_config.retry_count = 3`, "alpha-prod"},
		{`state = ENABLED retry_config.retry_count = 0`, ""},
		{`(state = PAUSED OR state = ENABLED) AND name:alpha`, "alpha-prod"},
		{`name:alpha OR name:beta AND state = PAUSED`, "beta-dev"},
		{`state = 1`, "alpha-prod"},
		{`retry_config:retry_count`, "alpha-prod"},
		{`retry_config:max_retry_duration`, "alpha-prod,beta-dev"},
	} {
		if got := names(t, tc.filter); got != tc.want {
			t.Errorf("filter %q = %q, want %q", tc.filter, got, tc.want)
		}
	}
}

func TestFilterRepeatedAndMaps(t *testing.T) {
	secrets := []*secretmanagerpb.Secret{
		{Name: "a", Topics: []*secretmanagerpb.Topic{{Name: "projects/p/topics/t1"}, {Name: "projects/p/topics/t2"}},
			Labels: map[string]string{"env": "prod"}, VersionAliases: map[string]int64{"current": 3}},
		{Name: "b", Topics: []*secretmanagerpb.Topic{{Name: "projects/p/topics/t3"}}},
		{Name: "c"},
	}
	for _, tc := range []struct{ filter, want string }{
		{`topics.name = "projects/p/topics/t2"`, "a"},
		{`topics.name:t3`, "b"},
		{`topics:*`, "a,b"},
		{`NOT topics:*`, "c"},
		{`topics.name != "projects/p/topics/t1"`, "b,c"},
		{`labels.env = prod`, "a"},
		{`labels:env`, "a"},
		{`labels.["env"] = prod`, "a"},
		{`labels["env"]:prod`, "a"},
		{`version_aliases.current = 3`, "a"},
		{`version_aliases.current > 1`, "a"},
	} {
		got, err := Apply(tc.filter, secrets)
		if err != nil {
			t.Fatalf("%q: %v", tc.filter, err)
		}
		var n []string
		for _, s := range got {
			n = append(n, s.GetName())
		}
		if s := strings.Join(n, ","); s != tc.want {
			t.Errorf("filter %q = %q, want %q", tc.filter, s, tc.want)
		}
	}
}

func TestFilterErrors(t *testing.T) {
	for _, tc := range []struct {
		filter string
		code   codes.Code
		msg    string
	}{
		{`nosuch = 1`, codes.InvalidArgument, `unknown field "nosuch"`},
		{`retry_config.nosuch = 1`, codes.InvalidArgument, `unknown field "retry_config.nosuch"`},
		{`state = BOGUS`, codes.InvalidArgument, "not a value of enum"},
		{`state > ENABLED`, codes.InvalidArgument, "does not apply"},
		{`retry_config.retry_count = many`, codes.InvalidArgument, "not a number"},
		{`user_update_time > yesterday`, codes.InvalidArgument, "RFC 3339"},
		{`retry_config.max_retry_duration > soon`, codes.InvalidArgument, "duration"},
		{`retry_config = 1`, codes.InvalidArgument, "is a message"},
		{`http_target.headers = x`, codes.InvalidArgument, "map field"},
		{`(state = ENABLED`, codes.InvalidArgument, `missing ")"`},
		{`state = "ENABLED`, codes.InvalidArgument, "unterminated"},
		{`state =`, codes.InvalidArgument, "needs a value"},
		{`state = ENABLED AND`, codes.InvalidArgument, "expected a restriction"},
		{`prod`, codes.InvalidArgument, "global search"},
		{`state:()`, codes.InvalidArgument, "empty value list"},
		{`name ~ "a.*"`, codes.Unimplemented, "regular-expression"},
		{`name !~ "a.*"`, codes.Unimplemented, "regular-expression"},
		{`timestamp(name) = 1`, codes.Unimplemented, "function"},
		{`state = ENABLED)`, codes.InvalidArgument, "unexpected"},
	} {
		_, err := Apply(tc.filter, jobs())
		if err == nil {
			t.Errorf("filter %q: no error", tc.filter)
			continue
		}
		e := apierror.From(err)
		if e.Code != tc.code || !strings.Contains(e.Message, tc.msg) {
			t.Errorf("filter %q: %v %q, want %v containing %q", tc.filter, e.Code, e.Message, tc.code, tc.msg)
		}
	}
}

func TestFilterFieldsAndAliases(t *testing.T) {
	if _, err := Apply(`description:x`, jobs(), Fields("name", "state")); err == nil ||
		!strings.Contains(err.Error(), "not a filterable field") {
		t.Fatalf("restricted field: %v", err)
	}
	if _, err := Apply(`http_target.headers.env = prod`, jobs(), Fields("http_target")); err != nil {
		t.Fatalf("subpath of an allowed field: %v", err)
	}
	got, err := Apply(`env = dev`, jobs(), Alias("env", "http_target.headers.env"))
	if err != nil || len(got) != 1 || !strings.HasSuffix(got[0].GetName(), "beta-dev") {
		t.Fatalf("alias: %v %v", got, err)
	}
}

func TestWildcard(t *testing.T) {
	for _, tc := range []struct {
		p, s string
		want bool
	}{
		{"*", "", true}, {"a*", "abc", true}, {"*c", "abc", true}, {"a*c", "abc", true},
		{"a*c", "ab", false}, {"*b*", "abc", true}, {"a*a", "a", false}, {"x/*/z", "x/y/w/z", true},
		{"abc", "abc", true}, {"abc", "abd", false},
	} {
		if got := Wildcard(tc.p, tc.s); got != tc.want {
			t.Errorf("Wildcard(%q, %q) = %v", tc.p, tc.s, got)
		}
	}
}

func orderNames(t *testing.T, orderBy string) string {
	t.Helper()
	js := jobs()
	if err := SortBy(orderBy, js); err != nil {
		t.Fatalf("SortBy(%q): %v", orderBy, err)
	}
	var out []string
	for _, j := range js {
		out = append(out, j.GetName()[strings.LastIndexByte(j.GetName(), '/')+1:])
	}
	return strings.Join(out, ",")
}

func TestOrderBy(t *testing.T) {
	for _, tc := range []struct{ orderBy, want string }{
		{"", "alpha-prod,beta-dev,gamma"},
		{"name desc", "beta-dev,alpha-prod,gamma"},   // europe-west1 < us-central1
		{"description", "gamma,alpha-prod,beta-dev"}, // "" < "N" < "h"
		{"description desc", "beta-dev,alpha-prod,gamma"},
		{"state", "alpha-prod,beta-dev,gamma"},
		{"state desc", "gamma,beta-dev,alpha-prod"},
		{"user_update_time desc", "beta-dev,alpha-prod,gamma"},
		{"user_update_time", "gamma,alpha-prod,beta-dev"},
		{"retry_config.retry_count desc, name", "alpha-prod,gamma,beta-dev"},
		{"retry_config.max_retry_duration DESC", "beta-dev,alpha-prod,gamma"},
		{"http_target.headers.env", "gamma,beta-dev,alpha-prod"},
		{"timeZone asc", "gamma,beta-dev,alpha-prod"},
	} {
		if got := orderNames(t, tc.orderBy); got != tc.want {
			t.Errorf("order_by %q = %q, want %q", tc.orderBy, got, tc.want)
		}
	}
}

func TestOrderByErrors(t *testing.T) {
	for _, ob := range []string{"nosuch", "name up", "name desc extra", "retry_config", "http_target.headers", ",", "name,"} {
		err := SortBy(ob, jobs())
		if err == nil || apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("order_by %q: %v, want INVALID_ARGUMENT", ob, err)
		}
	}
	if err := SortBy("description", jobs(), Fields("name")); err == nil {
		t.Errorf("order_by on a field outside Fields: no error")
	}
	secrets := []*secretmanagerpb.Secret{{Name: "a"}}
	if err := SortBy("topics.name", secrets); err == nil {
		t.Errorf("order_by through a repeated field: no error")
	}
}

func TestList(t *testing.T) {
	got, err := List(`state != DISABLED`, "user_update_time desc", jobs())
	if err != nil || len(got) != 2 || !strings.HasSuffix(got[0].GetName(), "beta-dev") {
		t.Fatalf("List: %v %v", got, err)
	}
}

func TestLocationIDs(t *testing.T) {
	mk := func(id string) (*locationpb.Location, error) {
		return &locationpb.Location{Name: "projects/p/locations/" + id, LocationId: id, DisplayName: id,
			Labels: map[string]string{"cloud.googleapis.com/region": id}}, nil
	}
	ids := []string{"europe-west1", "us-central1", "us-east1"}
	got, err := LocationIDs(`locationId = "us-*"`, ids, mk)
	if err != nil || strings.Join(got, ",") != "us-central1,us-east1" {
		t.Fatalf("LocationIDs: %v %v", got, err)
	}
	got, err = LocationIDs(`labels["cloud.googleapis.com/region"] = us-east1`, ids, mk)
	if err != nil || strings.Join(got, ",") != "us-east1" {
		t.Fatalf("LocationIDs labels: %v %v", got, err)
	}
	got, err = LocationIDs("", ids, mk)
	if err != nil || len(got) != 3 {
		t.Fatalf("LocationIDs empty: %v %v", got, err)
	}
	if _, err := LocationIDs(`region = x`, ids, mk); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestPage(t *testing.T) {
	items := []int{5, 4, 3, 2, 1}
	p1, tok, err := Page("s", items, "", 2)
	if err != nil || len(p1) != 2 || p1[0] != 5 || tok == "" {
		t.Fatalf("page 1: %v %q %v", p1, tok, err)
	}
	p2, tok2, err := Page("s", items, tok, 2)
	if err != nil || len(p2) != 2 || p2[0] != 3 {
		t.Fatalf("page 2: %v %v", p2, err)
	}
	p3, tok3, err := Page("s", items, tok2, 2)
	if err != nil || len(p3) != 1 || tok3 != "" {
		t.Fatalf("page 3: %v %q %v", p3, tok3, err)
	}
	if _, _, err := Page("other", items, tok, 2); err == nil {
		t.Fatal("token replayed on another scope")
	}
	if _, _, err := Page("s", items, "garbage", 2); err == nil {
		t.Fatal("garbage token accepted")
	}
	if _, _, err := Page("s", items, "", -1); err == nil {
		t.Fatal("negative page size accepted")
	}
	js, next, err := ListPage("jobs", "state != DISABLED", "name desc", jobs(), "", 1)
	if err != nil || len(js) != 1 || !strings.HasSuffix(js[0].GetName(), "beta-dev") || next == "" {
		t.Fatalf("ListPage: %v %q %v", js, next, err)
	}
	js, next, err = ListPage("jobs", "state != DISABLED", "name desc", jobs(), next, 1)
	if err != nil || len(js) != 1 || !strings.HasSuffix(js[0].GetName(), "alpha-prod") || next != "" {
		t.Fatalf("ListPage 2: %v %q %v", js, next, err)
	}
}
