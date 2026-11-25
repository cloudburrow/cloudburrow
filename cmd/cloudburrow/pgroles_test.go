package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// A created instance's backup carries its users, CloudBurrow's own roles
// left out: the target keeps its management and replication roles.
func TestPGUserRolesLeavesOutCloudBurrowsRoles(t *testing.T) {
	dump := strings.Join([]string{
		"CREATE ROLE " + components.PGAdminUser + ";",
		"ALTER ROLE " + components.PGAdminUser + " WITH SUPERUSER LOGIN PASSWORD 'SCRAM-SHA-256$a';",
		`CREATE ROLE "` + components.SQLReplicaUser + `";`,
		"CREATE ROLE postgres;",
		"ALTER ROLE postgres WITH SUPERUSER LOGIN PASSWORD 'SCRAM-SHA-256$b';",
		"CREATE ROLE app;",
		"ALTER ROLE app WITH LOGIN PASSWORD 'md5c';",
		"CREATE ROLE " + components.PGAdminUser + "_fan;",
		"",
	}, "\n")
	got := string(pgUserRoles([]byte(dump)))
	want := strings.Join([]string{
		"CREATE ROLE postgres;",
		"ALTER ROLE postgres WITH SUPERUSER LOGIN PASSWORD 'SCRAM-SHA-256$b';",
		"CREATE ROLE app;",
		"ALTER ROLE app WITH LOGIN PASSWORD 'md5c';",
		"CREATE ROLE " + components.PGAdminUser + "_fan;",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("pgUserRoles:\n%s\nwant:\n%s", got, want)
	}
}
