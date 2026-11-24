package alloydb

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func timeAfter(ms int) <-chan time.Time { return time.After(time.Duration(ms) * time.Millisecond) }

// pgConn connects to server's PostgreSQL as CloudBurrow's management
// role, to database ("postgres" when "").
func (s *Server) pgConn(ctx context.Context, server, user, database string) (*pgx.Conn, error) {
	return s.pgConnAs(ctx, server, user, "", database)
}

// iamRolePassword is the password of an IAM user's role (#2208): derived
// from the management role's, so the same on every server of every
// cluster, a replica's copy included, and known to no user.
func (s *Server) iamRolePassword(role string) string {
	m := hmac.New(sha256.New, []byte(s.opts.AdminPassword))
	m.Write([]byte("alloydb-iam-role\x00" + role))
	return hex.EncodeToString(m.Sum(nil))
}

// pgConnAs connects to server's PostgreSQL as user with password (the
// management role, or the superuser where there is none, when user is
// ""), to database ("postgres" when "").
func (s *Server) pgConnAs(ctx context.Context, server, user, password, database string) (*pgx.Conn, error) {
	if s.opts.Engine == nil {
		return nil, apierror.FailedPrecondition("no engine runs AlloyDB instances here")
	}
	addr := s.opts.Engine.Addr(server)
	if addr == "" {
		return nil, apierror.FailedPrecondition("the instance's server does not answer")
	}
	if user == "" {
		user = builtinUser
		if s.opts.AdminUser != "" && s.opts.AdminPassword != "" {
			user, password = s.opts.AdminUser, s.opts.AdminPassword
		}
	}
	if database == "" {
		database = "postgres"
	}
	cfg, err := pgx.ParseConfig("postgres://" + addr + "/?sslmode=disable&connect_timeout=10")
	if err != nil {
		return nil, apierror.Internal(err, "connection settings")
	}
	cfg.User, cfg.Database, cfg.Password = user, database, password
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, pgError(err)
	}
	return conn, nil
}

// pgError maps a PostgreSQL error to the API's: a role or object that
// exists or not, and the rest as FAILED_PRECONDITION with the server's
// message.
func pgError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "42710", "42P04":
			return apierror.AlreadyExists("%s", pe.Message)
		case "42704", "3D000", "28000":
			return apierror.NotFound("%s", pe.Message)
		case "42601", "42P01", "42703", "22P02":
			return apierror.InvalidArgument("%s", pe.Message)
		}
		return apierror.FailedPrecondition("%s", pe.Message)
	}
	return apierror.FailedPrecondition("%v", err)
}

func quoteIdent(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// roleName is a user's PostgreSQL role: an IAM service account's email
// without .gserviceaccount.com, as AlloyDB names it (manage-iam-authn), and
// any other user's ID as it is.
func roleName(id string) string {
	return strings.TrimSuffix(id, ".gserviceaccount.com")
}

// setupPrimary gives a new primary AlloyDB's roles and the cluster's
// recorded users.
func (s *Server) setupPrimary(ctx context.Context, cluster, server string) error {
	s.mu.Lock()
	password := s.passwords[cluster]
	var users []*alloydbpb.User
	for _, k := range sortedKeys(s.users, cluster+"/users/") {
		users = append(users, clone(s.users[k]))
	}
	s.mu.Unlock()
	conn, err := s.pgConn(ctx, server, "", "")
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	stmts := []string{`DO $$BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + SuperuserRole + `') THEN
    CREATE ROLE ` + SuperuserRole + ` WITH CREATEDB CREATEROLE NOLOGIN;
  END IF;
END$$`}
	for _, u := range users {
		id := u.GetName()[strings.LastIndex(u.GetName(), "/")+1:]
		pw := ""
		switch u.GetUserType() {
		case alloydbpb.User_ALLOYDB_BUILT_IN:
			pw = password
		case alloydbpb.User_ALLOYDB_IAM_USER:
			pw = s.iamRolePassword(roleName(id))
		}
		stmts = append(stmts, ensureRole(roleName(id), pw, u.GetUserType() == alloydbpb.User_ALLOYDB_BUILT_IN && id == builtinUser)...)
		for _, r := range u.GetDatabaseRoles() {
			stmts = append(stmts, "GRANT "+quoteIdent(r)+" TO "+quoteIdent(roleName(id)))
		}
	}
	for _, st := range stmts {
		if _, err := conn.Exec(ctx, st); err != nil {
			return fmt.Errorf("set up the primary's roles: %w", pgError(err))
		}
	}
	return nil
}

// ensureRole creates role name, LOGIN, when it does not exist, and sets
// its password when one is given; the superuser postgres exists.
func ensureRole(name, password string, exists bool) []string {
	var out []string
	if !exists {
		out = append(out, `DO $$BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = `+quoteLiteral(name)+`) THEN
    CREATE ROLE `+quoteIdent(name)+` WITH LOGIN;
  END IF;
END$$`)
	}
	if password != "" {
		out = append(out, "ALTER ROLE "+quoteIdent(name)+" WITH PASSWORD "+quoteLiteral(password))
	}
	return out
}

// primaryServer is cluster's ready primary's server; the caller holds mu.
func (s *Server) primaryServer(cluster string) (string, error) {
	p := s.primaryOf(cluster)
	if p == nil || p.GetState() != alloydbpb.Instance_READY {
		return "", apierror.FailedPrecondition("cluster %s has no READY primary instance", cluster)
	}
	return serverName(p.GetName()), nil
}

func (s *Server) CreateUser(ctx context.Context, req *alloydbpb.CreateUserRequest) (*alloydbpb.User, error) {
	u := clone(req.GetUser())
	if u == nil {
		u = &alloydbpb.User{}
	}
	if req.GetUserId() == "" {
		return nil, apierror.InvalidArgument("user_id is required")
	}
	switch u.GetUserType() {
	case alloydbpb.User_USER_TYPE_UNSPECIFIED:
		u.UserType = alloydbpb.User_ALLOYDB_BUILT_IN
	case alloydbpb.User_ALLOYDB_IAM_USER:
		if !strings.Contains(req.GetUserId(), "@") {
			return nil, apierror.InvalidArgument("an ALLOYDB_IAM_USER's user_id is its principal's email")
		}
		if u.GetPassword() != "" {
			return nil, apierror.InvalidArgument("an ALLOYDB_IAM_USER has no password")
		}
	}
	name := req.GetParent() + "/users/" + req.GetUserId()
	s.mu.Lock()
	if _, err := s.cluster(req.GetParent()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if _, ok := s.users[name]; ok {
		s.mu.Unlock()
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	server, err := s.primaryServer(req.GetParent())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		u.Name, u.Password = name, ""
		return u, nil
	}
	conn, err := s.pgConn(ctx, server, "", "")
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	role := roleName(req.GetUserId())
	stmts := []string{"CREATE ROLE " + quoteIdent(role) + " WITH LOGIN"}
	switch {
	case u.GetUserType() == alloydbpb.User_ALLOYDB_IAM_USER:
		// Its token is checked by the connectors' proxy, which then signs
		// in with the role's own password (#2208).
		stmts[0] += " PASSWORD " + quoteLiteral(s.iamRolePassword(role))
	case u.GetPassword() != "":
		stmts[0] += " PASSWORD " + quoteLiteral(u.GetPassword())
	}
	for _, r := range u.GetDatabaseRoles() {
		stmts = append(stmts, "GRANT "+quoteIdent(r)+" TO "+quoteIdent(role))
	}
	for _, st := range stmts {
		if _, err := conn.Exec(ctx, st); err != nil {
			return nil, pgError(err)
		}
	}
	u.Name, u.Password = name, ""
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[name] = u
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(u), nil
}

func (s *Server) GetUser(_ context.Context, req *alloydbpb.GetUserRequest) (*alloydbpb.User, error) {
	if userRE.FindStringSubmatch(req.GetName()) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/clusters/{cluster}/users/{user}", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return clone(u), nil
}

func (s *Server) ListUsers(_ context.Context, req *alloydbpb.ListUsersRequest) (*alloydbpb.ListUsersResponse, error) {
	s.mu.Lock()
	if _, err := s.cluster(req.GetParent()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var all []*alloydbpb.User
	for _, k := range sortedKeys(s.users, req.GetParent()+"/users/") {
		all = append(all, clone(s.users[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("alloydb-users:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &alloydbpb.ListUsersResponse{Users: out, NextPageToken: next}, nil
}

func (s *Server) UpdateUser(ctx context.Context, req *alloydbpb.UpdateUserRequest) (*alloydbpb.User, error) {
	in := req.GetUser()
	m := userRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("user.name %q must be projects/{project}/locations/{location}/clusters/{cluster}/users/{user}", in.GetName())
	}
	s.mu.Lock()
	cur, ok := s.users[in.GetName()]
	if !ok {
		s.mu.Unlock()
		if req.GetAllowMissing() {
			return s.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: m[1], UserId: m[5], User: in, ValidateOnly: req.GetValidateOnly()})
		}
		return nil, apierror.NotFound("Resource '%s' was not found", in.GetName())
	}
	cur = clone(cur)
	server, err := s.primaryServer(m[1])
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	paths := maskPaths(req.GetUpdateMask().GetPaths(), []string{"password", "database_roles"})
	next := clone(cur)
	var stmts []string
	role := roleName(m[5])
	for _, p := range paths {
		switch p {
		case "password":
			if cur.GetUserType() == alloydbpb.User_ALLOYDB_IAM_USER {
				if in.GetPassword() != "" {
					return nil, apierror.InvalidArgument("an ALLOYDB_IAM_USER has no password")
				}
				continue
			}
			if in.GetPassword() != "" {
				stmts = append(stmts, "ALTER ROLE "+quoteIdent(role)+" WITH PASSWORD "+quoteLiteral(in.GetPassword()))
			}
		case "database_roles":
			want := map[string]bool{}
			for _, r := range in.GetDatabaseRoles() {
				want[r] = true
				stmts = append(stmts, "GRANT "+quoteIdent(r)+" TO "+quoteIdent(role))
			}
			roles := in.GetDatabaseRoles()
			for _, r := range cur.GetDatabaseRoles() {
				if want[r] {
					continue
				}
				if in.GetKeepExtraRoles() {
					roles = append(roles, r)
					continue
				}
				stmts = append(stmts, "REVOKE "+quoteIdent(r)+" FROM "+quoteIdent(role))
			}
			next.DatabaseRoles = roles
		case "keep_extra_roles", "user_type":
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field UpdateUser can change", p)
		}
	}
	if req.GetValidateOnly() {
		return next, nil
	}
	if len(stmts) > 0 {
		conn, err := s.pgConn(ctx, server, "", "")
		if err != nil {
			return nil, err
		}
		defer conn.Close(ctx)
		for _, st := range stmts {
			if _, err := conn.Exec(ctx, st); err != nil {
				return nil, pgError(err)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.GetPassword() != "" && m[5] == builtinUser {
		s.passwords[m[1]] = in.GetPassword()
	}
	s.users[next.GetName()] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

func (s *Server) DeleteUser(ctx context.Context, req *alloydbpb.DeleteUserRequest) (*emptypb.Empty, error) {
	m := userRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/clusters/{cluster}/users/{user}", req.GetName())
	}
	s.mu.Lock()
	if _, ok := s.users[req.GetName()]; !ok {
		s.mu.Unlock()
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	server, err := s.primaryServer(m[1])
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return &emptypb.Empty{}, nil
	}
	conn, err := s.pgConn(ctx, server, "", "")
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+quoteIdent(roleName(m[5]))); err != nil {
		return nil, pgError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ListDatabases(ctx context.Context, req *alloydbpb.ListDatabasesRequest) (*alloydbpb.ListDatabasesResponse, error) {
	s.mu.Lock()
	if _, err := s.cluster(req.GetParent()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	server, err := s.primaryServer(req.GetParent())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	conn, err := s.pgConn(ctx, server, "", "")
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT datname, pg_encoding_to_char(encoding), datcollate, datctype, datistemplate
FROM pg_database WHERE datallowconn ORDER BY datname`)
	if err != nil {
		return nil, pgError(err)
	}
	var all []*alloydbpb.Database
	for rows.Next() {
		var name, charset, collate, ctype string
		var tmpl bool
		if err := rows.Scan(&name, &charset, &collate, &ctype, &tmpl); err != nil {
			return nil, pgError(err)
		}
		t := tmpl
		all = append(all, &alloydbpb.Database{Name: req.GetParent() + "/databases/" + name, Charset: charset,
			Collation: collate, CharacterType: ctype, IsTemplateDatabase: &t})
	}
	if err := rows.Err(); err != nil {
		return nil, pgError(err)
	}
	out, next, err := listPage("alloydb-databases:"+req.GetParent(), req.GetFilter(), "", all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &alloydbpb.ListDatabasesResponse{Databases: out, NextPageToken: next}, nil
}

// ExecuteSql runs sql_statement on an instance as user, with the
// password given (checked as the server checks it), and answers each
// statement's rows as text.
func (s *Server) ExecuteSql(ctx context.Context, req *alloydbpb.ExecuteSqlRequest) (*alloydbpb.ExecuteSqlResponse, error) {
	if req.GetDatabase() == "" || req.GetUser() == "" || strings.TrimSpace(req.GetSqlStatement()) == "" {
		return nil, apierror.InvalidArgument("instance, database, user and sql_statement are required")
	}
	s.mu.Lock()
	in, err := s.instance(req.GetInstance())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if in.GetState() != alloydbpb.Instance_READY {
		s.mu.Unlock()
		return nil, apierror.FailedPrecondition("instance %s is %s, not READY", in.GetName(), in.GetState())
	}
	server := serverName(in.GetName())
	s.mu.Unlock()
	if req.GetValidateOnly() {
		return &alloydbpb.ExecuteSqlResponse{}, nil
	}
	// Signed in as the user with the password given, which the server
	// checks (#2208).
	return s.executeAs(ctx, server, roleName(req.GetUser()), req.GetPassword(), req)
}

// StudioExecuteSql is AlloyDB Studio's run (#2208): ExecuteSql signed in
// with the password given, or, when iamEmail is set, with IAM database
// authentication as that principal, whose credentials the caller (the
// console, signed in as it) has checked. The principal must be one of the
// cluster's IAM users, as the connectors' IAM sign-in requires.
func (s *Server) StudioExecuteSql(ctx context.Context, req *alloydbpb.ExecuteSqlRequest, iamEmail string) (*alloydbpb.ExecuteSqlResponse, error) {
	if iamEmail == "" {
		return s.ExecuteSql(ctx, req)
	}
	if req.GetDatabase() == "" || strings.TrimSpace(req.GetSqlStatement()) == "" {
		return nil, apierror.InvalidArgument("instance, database and sql_statement are required")
	}
	s.mu.Lock()
	in, err := s.instance(req.GetInstance())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if in.GetState() != alloydbpb.Instance_READY {
		s.mu.Unlock()
		return nil, apierror.FailedPrecondition("instance %s is %s, not READY", in.GetName(), in.GetState())
	}
	server := serverName(in.GetName())
	s.mu.Unlock()
	cluster, _, _ := strings.Cut(req.GetInstance(), "/instances/")
	role := roleName(strings.ToLower(iamEmail))
	if !s.isIAMRole(cluster, role) {
		// UNVERIFIED: Google's words for a refused IAM sign-in, as the
		// connector listener's (listeners.go).
		return nil, apierror.FailedPrecondition("IAM authentication failed for user %q", role)
	}
	return s.executeAs(ctx, server, role, s.iamRolePassword(role), req)
}

// executeAs runs req's statements on server as user with password.
func (s *Server) executeAs(ctx context.Context, server, user, password string, req *alloydbpb.ExecuteSqlRequest) (*alloydbpb.ExecuteSqlResponse, error) {
	conn, err := s.pgConnAs(ctx, server, user, password, req.GetDatabase())
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	start := time.Now()
	results, err := conn.PgConn().Exec(ctx, req.GetSqlStatement()).ReadAll()
	md := &alloydbpb.ExecuteSqlMetadata{SqlStatementExecutionDuration: durationpb.New(time.Since(start)),
		Status: alloydbpb.ExecuteSqlMetadata_OK}
	resp := &alloydbpb.ExecuteSqlResponse{Metadata: md}
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		out := &alloydbpb.SqlResult{}
		for _, f := range r.FieldDescriptions {
			out.Columns = append(out.Columns, &alloydbpb.SqlResultColumn{Name: f.Name, Type: typeName(f.DataTypeOID)})
		}
		for _, row := range r.Rows {
			rr := &alloydbpb.SqlResultRow{}
			for _, v := range row {
				if v == nil {
					t := true
					rr.Values = append(rr.Values, &alloydbpb.SqlResultValue{NullValue: &t})
					continue
				}
				str := string(v)
				rr.Values = append(rr.Values, &alloydbpb.SqlResultValue{Value: &str})
			}
			out.Rows = append(out.Rows, rr)
		}
		resp.SqlResults = append(resp.SqlResults, out)
	}
	if err != nil {
		md.Status = alloydbpb.ExecuteSqlMetadata_ERROR
		md.Message = err.Error()
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			md.Message = pe.Severity + ": " + pe.Message
		}
		md.PartialResult = len(resp.SqlResults) > 0
	}
	return resp, nil
}

// typeName is a column type's name for the common built-in types, and its
// OID otherwise.
func typeName(oid uint32) string {
	switch oid {
	case 16:
		return "bool"
	case 20:
		return "int8"
	case 21:
		return "int2"
	case 23:
		return "int4"
	case 25:
		return "text"
	case 700:
		return "float4"
	case 701:
		return "float8"
	case 1043:
		return "varchar"
	case 1082:
		return "date"
	case 1114:
		return "timestamp"
	case 1184:
		return "timestamptz"
	case 1700:
		return "numeric"
	case 114:
		return "json"
	case 3802:
		return "jsonb"
	case 2950:
		return "uuid"
	case 19:
		return "name"
	case 26:
		return "oid"
	}
	return fmt.Sprint(oid)
}
