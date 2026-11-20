package sqladmin

import (
	"net/http"
	"strings"
	"testing"
)

// TestMySQLSystemAccounts (#1993): users.list hides Cloud SQL's system
// accounts and CloudBurrow's, a write to one is refused, and 'root'@'%'
// is the user's on a created instance but not on `up`'s server.
func TestMySQLSystemAccounts(t *testing.T) {
	for _, c := range []struct {
		name, host string
		hidden     bool
	}{
		{"cloudsqladmin", "127.0.0.1", true}, {"cloudsqladmin", "%", true}, {"root", "localhost", true},
		{"root", "::1", true}, {"cloudsqlreplica", "%", true}, {"mysql.sys", "localhost", true},
		{"root", "%", false}, {"app", "%", false}, {"root", "10.0.0.1", false},
	} {
		if got := mysqlHidden(c.name, c.host); got != c.hidden {
			t.Errorf("mysqlHidden(%s@%s) = %v", c.name, c.host, got)
		}
	}
	created := &MySQL{Admin: MySQLAdminUser}
	if err := created.system(User{Name: "root", Host: "%"}); err != nil {
		t.Errorf("a created instance's root: %v", err)
	}
	if err := created.system(User{Name: "cloudsqladmin"}); err == nil || err.(*apiError).code != http.StatusBadRequest {
		t.Errorf("cloudsqladmin: %v, want 400", err)
	}
	if err := (&MySQL{}).system(User{Name: "root", Host: "%"}); err == nil {
		t.Error("the root of `up`'s server, which CloudBurrow signs in as, is not refused")
	}

	s := MySQLSystemAccountsSQL("MYSQL_8_4", "it's")
	if !strings.HasPrefix(s, "SET SESSION sql_log_bin = 0;\n") || strings.Count(s, "CREATE USER IF NOT EXISTS 'cloudsqladmin'@") != 3 ||
		!strings.Contains(s, "'cloudsqladmin'@'::1' IDENTIFIED BY 'it''s'") || !strings.Contains(s, "WITH GRANT OPTION") {
		t.Errorf("8.4's statements:\n%s", s)
	}
	if s := MySQLSystemAccountsSQL("MYSQL_5_6", "p"); strings.Contains(s, "CREATE USER") || strings.Count(s, "GRANT ALL PRIVILEGES ON *.* TO 'cloudsqladmin'@") != 3 {
		t.Errorf("5.6's statements:\n%s", s)
	}
}
