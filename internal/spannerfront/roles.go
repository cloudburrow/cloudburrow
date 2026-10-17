package spannerfront

import (
	"context"
	"regexp"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Database roles and the database operations (#1498).
//
// The emulator accepts CREATE ROLE, DROP ROLE, GRANT and REVOKE and keeps
// none of them (measured: GetDatabaseDdl omits them). The front passes
// CreateDatabase and UpdateDatabaseDdl to the emulator and keeps, per
// database, the role statements the emulator accepted: GetDatabaseDdl
// answers with them after the emulator's own, as Google's does,
// ListDatabaseRoles lists the roles with Google's system roles, and a backup
// carries them. Roles are not enforced, as the emulator does not enforce
// fine-grained access control (ADR-0006).
//
// The two calls' operations, which the emulator answers by name alone, are
// kept by name for ListDatabaseOperations.

var (
	roleStmt   = regexp.MustCompile(`(?is)^\s*(CREATE|DROP)\s+ROLE\s+([` + "`" + `"]?)(\w+)([` + "`" + `"]?)\s*$`)
	grantStmt  = regexp.MustCompile(`(?is)^\s*(GRANT|REVOKE)\s`)
	systemRole = []string{"public", "spanner_info_reader", "spanner_sys_reader"}
)

// trackRoles keeps the role statements of stmts, which the emulator
// accepted for database.
func (s *Instances) trackRoles(database string, stmts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range stmts {
		st = strings.TrimSpace(st)
		if m := roleStmt.FindStringSubmatch(st); m != nil {
			role := m[3]
			if strings.EqualFold(m[1], "CREATE") {
				s.roleStmts[database] = append(s.roleStmts[database], st)
				continue
			}
			// DROP ROLE: forget the role's CREATE and its grants.
			var kept []string
			for _, old := range s.roleStmts[database] {
				if c := roleStmt.FindStringSubmatch(old); c != nil && c[3] == role {
					continue
				}
				if grantStmt.MatchString(old) && regexp.MustCompile(`(?i)\bROLE\s+[`+"`"+`"]?`+regexp.QuoteMeta(role)+`\b`).MatchString(old) {
					continue
				}
				kept = append(kept, old)
			}
			s.roleStmts[database] = kept
			continue
		}
		if grantStmt.MatchString(st) {
			s.roleStmts[database] = append(s.roleStmts[database], st)
		}
	}
}

// roleDDL is database's role statements, in the order they were run.
func (s *Instances) roleDDL(database string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.roleStmts[database]...)
}

func (s *Instances) dropRoles(database string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.roleStmts {
		if name == database || strings.HasPrefix(name, database+"/") {
			delete(s.roleStmts, name)
		}
	}
}

// roles are database's roles: Google's system roles and those created.
func (s *Instances) roles(database string) map[string]bool {
	out := map[string]bool{}
	for _, r := range systemRole {
		out[r] = true
	}
	for _, st := range s.roleDDL(database) {
		if m := roleStmt.FindStringSubmatch(st); m != nil {
			out[m[3]] = true
		}
	}
	return out
}

func (d databases) CreateDatabase(ctx context.Context, req *databasepb.CreateDatabaseRequest) (*longrunningpb.Operation, error) {
	op, err := d.s.emuDB.CreateDatabase(ctx, req)
	if err != nil {
		return nil, err
	}
	d.s.trackEmuOp(op.GetName())
	if m := regexp.MustCompile("(?is)^\\s*CREATE\\s+DATABASE\\s+[`\"]?([^`\"\\s]+)").FindStringSubmatch(req.GetCreateStatement()); m != nil {
		d.s.dropRoles(req.GetParent() + "/databases/" + m[1])
		d.s.trackRoles(req.GetParent()+"/databases/"+m[1], req.GetExtraStatements())
	}
	return op, nil
}

func (d databases) UpdateDatabaseDdl(ctx context.Context, req *databasepb.UpdateDatabaseDdlRequest) (*longrunningpb.Operation, error) {
	op, err := d.s.emuDB.UpdateDatabaseDdl(ctx, req)
	if err != nil {
		return nil, err
	}
	d.s.trackEmuOp(op.GetName())
	if op.GetError() == nil {
		d.s.trackRoles(req.GetDatabase(), req.GetStatements())
	}
	return op, nil
}

func (d databases) GetDatabaseDdl(ctx context.Context, req *databasepb.GetDatabaseDdlRequest) (*databasepb.GetDatabaseDdlResponse, error) {
	resp, err := d.s.emuDB.GetDatabaseDdl(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.Statements = append(resp.Statements, d.s.roleDDL(req.GetDatabase())...)
	return resp, nil
}

func (d databases) ListDatabaseRoles(ctx context.Context, req *databasepb.ListDatabaseRolesRequest) (*databasepb.ListDatabaseRolesResponse, error) {
	if _, err := d.s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: req.GetParent()}); err != nil {
		return nil, err
	}
	byName := map[string]bool{}
	for r := range d.s.roles(req.GetParent()) {
		byName[req.GetParent()+"/databaseRoles/"+r] = true
	}
	page, next, err := pageOf("roles:"+req.GetParent(), byName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &databasepb.ListDatabaseRolesResponse{NextPageToken: next}
	for _, n := range page {
		resp.DatabaseRoles = append(resp.DatabaseRoles, &databasepb.DatabaseRole{Name: n})
	}
	return resp, nil
}

// roleExists checks that the database role named name exists.
func (s *Instances) roleExists(ctx context.Context, name string) error {
	i := strings.Index(name, "/databaseRoles/")
	db, role := name[:i], name[i+len("/databaseRoles/"):]
	if _, err := s.emuDB.GetDatabase(ctx, &databasepb.GetDatabaseRequest{Name: db}); err != nil {
		return err
	}
	if !s.roles(db)[role] {
		return status.Errorf(codes.NotFound, "Database role not found: %s", name)
	}
	return nil
}
