package spannerfront

import (
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFilterMatchesGoogleExamples(t *testing.T) {
	b := &databasepb.Backup{Name: "projects/p/instances/i/backups/howl", Database: "projects/p/instances/i/databases/prod",
		State: databasepb.Backup_READY, SizeBytes: 2048, CreateTime: timestamppb.New(mustTime(t, "2024-03-28T14:50:00Z"))}
	for filter, want := range map[string]bool{
		"":                                     true,
		"name:Howl":                            false,
		"name:howl":                            true,
		"database:prod":                        true,
		"state:READY":                          true,
		"state:CREATING":                       false,
		"size_bytes > 1024":                    true,
		"size_bytes > 10000000000":             false,
		`create_time < "2025-01-01T00:00:00Z"`: true,
		"(name:howl) AND (database:prod) AND (state:READY)": true,
		"(name:howl) AND (state:CREATING)":                  false,
		"(state:CREATING) OR (size_bytes >= 2048)":          true,
	} {
		f, err := parseFilter(filter)
		if err != nil {
			t.Errorf("parseFilter(%q): %v", filter, err)
			continue
		}
		if got := f.match(b); got != want {
			t.Errorf("filter %q matched %v, want %v", filter, got, want)
		}
	}
	meta, _ := anypb.New(&databasepb.CopyBackupMetadata{Name: "projects/p/instances/i/backups/c", SourceBackup: "projects/p/instances/i/backups/howl"})
	op := &longrunningpb.Operation{Name: "projects/p/instances/i/backups/c/operations/x", Metadata: meta, Done: true}
	f, err := parseFilter("(metadata.@type:type.googleapis.com/google.spanner.admin.database.v1.CopyBackupMetadata) AND (metadata.source_backup:howl) AND (done:true)")
	if err != nil || !f.match(op) {
		t.Errorf("an operation filter = %v, %v, want a match", f.match(op), err)
	}
	if _, err := parseFilter("name"); err == nil {
		t.Error(`parseFilter("name") = nil, want an error`)
	}
}

func TestManagedConfigsAreGooglesRegionsAndMultiRegions(t *testing.T) {
	c := managedConfig("projects/p", "regional-us-central1")
	if c == nil || len(c.GetReplicas()) != 3 || c.GetLeaderOptions()[0] != "us-central1" {
		t.Fatalf("regional-us-central1 = %v", c)
	}
	if managedConfig("projects/p", "regional-mars1") != nil || managedConfig("projects/p", "custom-x") != nil {
		t.Error("an unknown configuration is managed")
	}
	if n := managedConfig("projects/p", "nam3"); n == nil || len(n.GetReplicas()) != 5 {
		t.Errorf("nam3 = %v, want four read-write replicas and a witness", n)
	}
}

func mustTime(t *testing.T, s string) (out time.Time) {
	t.Helper()
	out, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
