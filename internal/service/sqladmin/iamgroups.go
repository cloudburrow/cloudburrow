package sqladmin

// IAM group authentication (#1524), for PostgreSQL and MySQL 8.0 and
// later, as Google documents it
// (https://docs.cloud.google.com/sql/docs/postgres/iam-authentication,
// .../mysql/iam-authentication, .../postgres/add-manage-iam-users,
// .../mysql/add-manage-iam-users, read 2026-11-25; Google's
// announcement, https://cloud.google.com/blog/products/databases/introducing-cloud-sql-iam-group-authentication,
// gives MySQL 8.0 and later):
//
//   - users.insert of type CLOUD_IAM_GROUP adds a Cloud Identity group
//     (gcloud sql users create GROUP_EMAIL --type=cloud_iam_group). On
//     PostgreSQL it is a role named by the group's email, at most 63
//     characters; on MySQL a role 'NAME'@'DOMAIN', NAME the part of the
//     email before the @, at most 32 characters, as Google's
//     `grant ... to "GROUP_NAME"@"HOSTNAME"` names it. It has no
//     privileges until they are granted to it. At most 200 groups.
//   - A member of the group (directly or through nested groups, which
//     reach the instance through a parent group added to it) signs in
//     with its own IAM account and token, named as an IAM user is, and
//     needs cloudsql.instances.login, which the group's binding of
//     roles/cloudsql.instanceUser gives it. Its first successful sign-in
//     to the primary creates its account, of type CLOUD_IAM_GROUP_USER or
//     CLOUD_IAM_GROUP_SERVICE_ACCOUNT, a member of the role of each of the
//     instance's groups it belongs to, whose privileges it inherits (on
//     MySQL as default roles; SET ROLE ALL takes new ones in a session).
//     On a read replica a member must have signed in to the primary first.
//   - Membership is read from this instance's Cloud Identity at each
//     sign-in, users.list and users.get, and when a group is removed: an
//     account gains and loses the groups' roles then, and is dropped once
//     it belongs to none of the instance's groups, unless it owns objects
//     ("you must reassign ownership of the object before you can drop the
//     user manually"). Google's take "about 15 minutes" to propagate;
//     here it is at the next of those.
//   - A group deleted from Cloud Identity is INACTIVE (iamStatus) and its
//     privileges pass to no one; one that exists is ACTIVE.
//   - Removing a group takes its privileges from its members, and removes
//     the members that belong to no other group of the instance.
//   - A group member's account is neither created nor deleted through
//     the API, and an individual CLOUD_IAM_USER or
//     CLOUD_IAM_SERVICE_ACCOUNT identical to one is refused; an existing
//     individual user "can't be used for IAM group authentication".

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The IAM group user types, as the Admin API names them.
const (
	TypeIAMGroupUser           = "CLOUD_IAM_GROUP_USER"
	TypeIAMGroupServiceAccount = "CLOUD_IAM_GROUP_SERVICE_ACCOUNT"
)

// maxIAMGroups is Google's limit: "A maximum of 200 IAM groups can exist
// on a Cloud SQL instance at any time", inactive ones included.
const maxIAMGroups = 200

// The iamStatus values of a group.
const (
	iamStatusActive   = "ACTIVE"
	iamStatusInactive = "INACTIVE"
)

// isGroupMemberType is whether typ is a group member's account.
func isGroupMemberType(typ string) bool {
	return typ == TypeIAMGroupUser || typ == TypeIAMGroupServiceAccount
}

// isIAMLoginType is whether a user of type typ signs in with IAM.
func isIAMLoginType(typ string) bool {
	return typ == TypeIAMUser || typ == TypeIAMServiceAccount || isGroupMemberType(typ)
}

// iamGroupEngine is an engine that keeps IAM groups and their members'
// accounts.
type iamGroupEngine interface {
	// CreateIAMGroup creates the role of the group g (Name, Host,
	// IAMEmail as iamGroupUser makes them).
	CreateIAMGroup(ctx context.Context, g User) error
	// DeleteIAMGroup drops the group's role and what was granted to it.
	DeleteIAMGroup(ctx context.Context, g User) error
	// SyncGroupMember makes m's account, signing in with password, if it
	// has none, and a member of exactly groups among the instance's.
	SyncGroupMember(ctx context.Context, m User, password string, groups []User) error
	// DropGroupMember takes m out of every group and drops it, keeping it
	// (outside every group) when it owns objects.
	DropGroupMember(ctx context.Context, m User) error
}

// iamGroupUser is the user an IAM group of email is on engine e.
func iamGroupUser(e Engine, email string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !iamEmailRE.MatchString(email) {
		return User{}, Errorf(http.StatusBadRequest, "Invalid request: %s is not the email of a Cloud Identity group.", email)
	}
	if strings.HasSuffix(email, saSuffix) {
		return User{}, Errorf(http.StatusBadRequest, "Invalid request: %s is a service account; create it as type CLOUD_IAM_SERVICE_ACCOUNT.", email)
	}
	u := User{Name: email, Type: TypeIAMGroup, IAMEmail: email}
	limit := 63
	what := "PostgreSQL identifiers"
	if isMySQLEngine(e) {
		u.Name, u.Host, _ = strings.Cut(email, "@")
		limit, what = 32, "MySQL usernames"
	}
	if len(u.Name) > limit {
		return User{}, Errorf(http.StatusBadRequest, "Invalid request: the IAM group name %s is longer than %d characters, the limit of %s; "+
			"nest the group in a parent group with a shorter name and add that one.", u.Name, limit, what)
	}
	return u, nil
}

// insertIAMGroup is users.insert of a CLOUD_IAM_GROUP.
func (s *Server) insertIAMGroup(ctx context.Context, base, project string, e Engine, email, host, password string) (any, error) {
	if legacyMySQL(e.DatabaseVersion()) {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: IAM group authentication is available for MySQL 8.0 and later; instance %s is %s.",
			e.Instance(), e.DatabaseVersion())
	}
	ge, ok := e.(iamGroupEngine)
	if !ok {
		return nil, unimplemented("users of type CLOUD_IAM_GROUP on "+e.DatabaseVersion(), "IAM group authentication is served for PostgreSQL and MySQL")
	}
	if password != "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a CLOUD_IAM_GROUP user has no password; its members sign in with IAM access tokens.")
	}
	if host != "" && host != "%" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a CLOUD_IAM_GROUP user has no host.")
	}
	g, err := iamGroupUser(e, email)
	if err != nil {
		return nil, err
	}
	us, err := e.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, u := range us {
		if u.Type == TypeIAMGroup {
			n++
			if u.IAMEmail == g.IAMEmail {
				return nil, Errorf(http.StatusConflict, "The IAM group %s already exists on instance %s.", g.IAMEmail, e.Instance())
			}
		}
	}
	if n >= maxIAMGroups {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance %s already has %d IAM groups, the most an instance can have.", e.Instance(), maxIAMGroups)
	}
	if err := ge.CreateIAMGroup(ctx, g); err != nil {
		return nil, err
	}
	return s.done(base, project, e, "CREATE_USER", g.Name), nil
}

// deleteIAMGroup is users.delete of a CLOUD_IAM_GROUP: its role goes, and
// its members lose what it gave them, or their accounts when they are in
// no other group of the instance.
func (s *Server) deleteIAMGroup(ctx context.Context, project string, e Engine, g User) error {
	ge, ok := e.(iamGroupEngine)
	if !ok {
		return unimplemented("users of type CLOUD_IAM_GROUP on "+e.DatabaseVersion(), "IAM group authentication is served for PostgreSQL and MySQL")
	}
	if err := ge.DeleteIAMGroup(ctx, g); err != nil {
		return err
	}
	return s.syncGroupMembers(ctx, project, e)
}

// groupStatus is a group's iamStatus: ACTIVE while Cloud Identity has it.
func (s *Server) groupStatus(email string) string {
	src := s.iamSource()
	if src != nil && src.GroupExists != nil && src.GroupExists(email) {
		return iamStatusActive
	}
	return iamStatusInactive
}

// withGroupStatus sets the iamStatus of each group in us.
func (s *Server) withGroupStatus(us []User) []User {
	for i := range us {
		if us[i].Type == TypeIAMGroup {
			us[i].IAMStatus = s.groupStatus(us[i].IAMEmail)
		}
	}
	return us
}

// memberGroups are the instance's active groups (of groups) that email
// belongs to, directly or through nested groups.
func (s *Server) memberGroups(email string, groups []User) []User {
	src := s.iamSource()
	if src == nil || src.Groups == nil {
		return nil
	}
	in := map[string]bool{}
	for _, g := range src.Groups(strings.ToLower(email)) {
		in[strings.ToLower(g)] = true
	}
	var out []User
	for _, g := range groups {
		if in[g.IAMEmail] && s.groupStatus(g.IAMEmail) == iamStatusActive {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IAMEmail < out[j].IAMEmail })
	return out
}

// isReplica is whether project's instance named name replicates from a
// primary, where no account can be made.
func (s *Server) isReplica(project, name string) bool {
	c := s.createdNamed(project, name)
	if c == nil {
		return false
	}
	s.emu.RLock()
	defer s.emu.RUnlock()
	return c.spec.MasterInstanceName != ""
}

// syncGroupMembers brings every group member's account of e up to date
// with Cloud Identity: the groups' roles it holds, or the account itself
// when it is in none of the instance's groups. Nothing is written on a
// replica, which follows its primary.
func (s *Server) syncGroupMembers(ctx context.Context, project string, e Engine) error {
	ge, ok := e.(iamGroupEngine)
	if !ok || s.isReplica(project, e.Instance()) {
		return nil
	}
	us, err := e.ListUsers(ctx)
	if err != nil {
		return err
	}
	var groups []User
	for _, u := range us {
		if u.Type == TypeIAMGroup {
			groups = append(groups, u)
		}
	}
	for _, u := range us {
		if !isGroupMemberType(u.Type) {
			continue
		}
		in := s.memberGroups(u.IAMEmail, groups)
		if len(in) == 0 {
			err = ge.DropGroupMember(ctx, u)
		} else {
			err = ge.SyncGroupMember(ctx, u, s.rolePassword(u.Name), in)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// iamLoginUser is the IAM user a sign-in to e as name is: an IAM user,
// service account or group member's account; or, when the instance has
// IAM groups and no account is named name, a group member signing in for
// the first time (Type TypeIAMGroupUser until its token names it). ok is
// false for every other name, which the server checks itself.
func iamLoginUser(ctx context.Context, e Engine, name string) (User, bool) {
	us, err := e.ListUsers(ctx)
	if err != nil {
		return User{}, false
	}
	taken, groups := false, false
	for _, u := range us {
		if u.Name == name && u.Type != TypeIAMGroup {
			if isIAMLoginType(u.Type) {
				return u, true
			}
			taken = true
		}
		if u.Type == TypeIAMGroup {
			groups = true
			// A group's PostgreSQL role signs in to nothing; on MySQL
			// it is at the group's domain, beside 'NAME'@'%'.
			if u.Name == name && !isMySQLEngine(e) {
				taken = true
			}
		}
	}
	if taken || !groups || name == "" || (!isMySQLEngine(e) && !strings.Contains(name, "@")) {
		return User{}, false
	}
	return User{Name: name, Type: TypeIAMGroupUser}, true
}

// checkGroupLogin checks a sign-in by email, whose credentials the caller
// has checked, as the group member u of project's instance e, making or
// bringing up to date its account on the primary.
func (s *Server) checkGroupLogin(project string, e Engine, u User, email string) (iamLogin, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	refused := func(why string) (iamLogin, error) {
		return iamLogin{}, errIAMRefused{msg: "Cloud SQL IAM group authentication failed for user \"" + u.Name + "\": " + why}
	}
	typ, indiv := TypeIAMGroupUser, TypeIAMUser
	if strings.HasSuffix(email, saSuffix) {
		typ, indiv = TypeIAMGroupServiceAccount, TypeIAMServiceAccount
	}
	if !s.iamFlagOn(project, e) {
		return refused("the instance's " + iamFlag(isMySQLEngine(e)) + " flag is not on")
	}
	src := s.iamSource()
	if src == nil || src.Allowed == nil || src.Groups == nil {
		return refused("this instance checks no access tokens")
	}
	want, err := iamDBName(indiv, email, isMySQLEngine(e))
	if err != nil || want != u.Name {
		return refused("the access token is " + email + "'s, not this user's")
	}
	ctx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	us, err := e.ListUsers(ctx)
	if err != nil {
		return refused(err.Error())
	}
	var groups []User
	exists := false
	for _, x := range us {
		switch {
		case x.Type == TypeIAMGroup:
			groups = append(groups, x)
		case x.Name == u.Name && isGroupMemberType(x.Type):
			exists = true
		}
	}
	in := s.memberGroups(email, groups)
	replica := s.isReplica(project, e.Instance())
	if len(in) == 0 {
		if exists && !replica {
			if ge, ok := e.(iamGroupEngine); ok {
				_ = ge.DropGroupMember(ctx, User{Name: u.Name, Host: u.Host, Type: typ, IAMEmail: email})
			}
		}
		return refused(email + " is not a member of an active IAM group of the instance")
	}
	if !src.Allowed(project, e.Instance(), email, LoginPermission) {
		return refused(email + " does not have " + LoginPermission + " (roles/cloudsql.instanceUser) on the instance")
	}
	if replica {
		if !exists {
			return refused("a group member signs in to the primary instance first; its account then replicates to the read replicas")
		}
	} else {
		ge, ok := e.(iamGroupEngine)
		if !ok {
			return refused("IAM group authentication is served for PostgreSQL and MySQL")
		}
		m := User{Name: u.Name, Type: typ, IAMEmail: email}
		if isMySQLEngine(e) {
			m.Host = "%"
		}
		if err := ge.SyncGroupMember(ctx, m, s.rolePassword(u.Name), in); err != nil {
			return refused(err.Error())
		}
	}
	return iamLogin{role: u.Name, password: s.rolePassword(u.Name)}, nil
}

// groupMemberConflict refuses an individual IAM user identical to a group
// member's account: "you can't add an account with the type
// CLOUD_IAM_USER or CLOUD_IAM_SERVICE_ACCOUNT if an identical account with
// type CLOUD_IAM_GROUP_USER or CLOUD_IAM_GROUP_SERVICE_ACCOUNT already
// exists".
func groupMemberConflict(ctx context.Context, e Engine, name string) error {
	us, err := e.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range us {
		if u.Name == name && isGroupMemberType(u.Type) {
			return Errorf(http.StatusBadRequest, "Invalid request: %s is already a %s of instance %s, through an IAM group; "+
				"an individual IAM user identical to a group member cannot be added.", name, u.Type, e.Instance())
		}
	}
	return nil
}

// PostgreSQL: a group is a NOLOGIN role in cloudsqliamgroup; a member's
// account a login role in cloudsqliamgroupuser or
// cloudsqliamgroupserviceaccount, which pg_hba.conf holds to its password.
const (
	pgIAMGroupRole               = "cloudsqliamgroup"
	pgIAMGroupUserRole           = "cloudsqliamgroupuser"
	pgIAMGroupServiceAccountRole = "cloudsqliamgroupserviceaccount"
)

var pgIAMGroupHBA = []string{
	"host all +" + pgIAMGroupUserRole + " all md5",
	"host all +" + pgIAMGroupServiceAccountRole + " all md5",
}

func pgQuoteIdent(s string) string { return pgx.Identifier{s}.Sanitize() }

// pgEnsureGroupRoles makes the marker roles and the pg_hba.conf lines.
func pgEnsureGroupRoles(ctx context.Context, c *pgx.Conn) error {
	for _, g := range []string{pgIAMGroupRole, pgIAMGroupUserRole, pgIAMGroupServiceAccountRole} {
		var exists bool
		if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", g).Scan(&exists); err != nil {
			return pgError(err)
		}
		if !exists {
			if _, err := c.Exec(ctx, "CREATE ROLE "+g+" NOLOGIN"); err != nil {
				return pgError(err)
			}
		}
	}
	return pgEnsureHBA(ctx, c, pgIAMGroupHBA...)
}

func (p *Postgres) CreateIAMGroup(ctx context.Context, g User) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if err := pgEnsureGroupRoles(ctx, c); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "CREATE ROLE "+pgQuoteIdent(g.Name)+" NOLOGIN IN ROLE "+pgIAMGroupRole); err != nil {
		return pgError(err)
	}
	return nil
}

// pgOwnsObjects is whether role owns objects in some database.
func pgOwnsObjects(ctx context.Context, c *pgx.Conn, role string) (bool, error) {
	var owns bool
	err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_shdepend d JOIN pg_catalog.pg_roles r ON r.oid = d.refobjid
		WHERE d.refclassid = 'pg_catalog.pg_authid'::regclass AND d.deptype = 'o' AND r.rolname = $1)`, role).Scan(&owns)
	return owns, err
}

// pgDropPrivileges takes every privilege granted to role, in every
// database that takes connections: DROP OWNED of a role that owns nothing.
func (p *Postgres) pgDropPrivileges(ctx context.Context, c *pgx.Conn, role string) error {
	rows, err := c.Query(ctx, "SELECT datname FROM pg_catalog.pg_database WHERE datallowconn ORDER BY datname")
	if err != nil {
		return pgError(err)
	}
	var dbs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return err
		}
		dbs = append(dbs, d)
	}
	rows.Close()
	for _, d := range dbs {
		dc, err := p.connDB(ctx, d)
		if err != nil {
			return err
		}
		_, err = dc.Exec(ctx, "DROP OWNED BY "+pgQuoteIdent(role))
		_ = dc.Close(context.Background())
		if err != nil {
			return pgError(err)
		}
	}
	return nil
}

func (p *Postgres) DeleteIAMGroup(ctx context.Context, g User) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	owns, err := pgOwnsObjects(ctx, c, g.Name)
	if err != nil {
		return pgError(err)
	}
	if owns {
		return Errorf(http.StatusBadRequest, "Invalid request: the IAM group %s owns database objects; reassign their ownership before removing it.", g.IAMEmail)
	}
	if err := p.pgDropPrivileges(ctx, c, g.Name); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "DROP ROLE "+pgQuoteIdent(g.Name)); err != nil {
		return pgError(err)
	}
	return nil
}

// pgSetGroups makes role a member of exactly groups among the roles in
// cloudsqliamgroup.
func pgSetGroups(ctx context.Context, c *pgx.Conn, role string, groups []User) error {
	rows, err := c.Query(ctx, `SELECT g.rolname FROM pg_catalog.pg_auth_members a
		JOIN pg_catalog.pg_roles g ON g.oid = a.roleid JOIN pg_catalog.pg_roles r ON r.oid = a.member
		WHERE r.rolname = $1 AND EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members x JOIN pg_catalog.pg_roles k ON k.oid = x.roleid
			WHERE x.member = g.oid AND k.rolname = '`+pgIAMGroupRole+`')`, role)
	if err != nil {
		return pgError(err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return err
		}
		have[g] = true
	}
	rows.Close()
	want := map[string]bool{}
	for _, g := range groups {
		want[g.Name] = true
		if !have[g.Name] {
			if _, err := c.Exec(ctx, "GRANT "+pgQuoteIdent(g.Name)+" TO "+pgQuoteIdent(role)); err != nil {
				return pgError(err)
			}
		}
	}
	for g := range have {
		if !want[g] {
			if _, err := c.Exec(ctx, "REVOKE "+pgQuoteIdent(g)+" FROM "+pgQuoteIdent(role)); err != nil {
				return pgError(err)
			}
		}
	}
	return nil
}

func (p *Postgres) SyncGroupMember(ctx context.Context, m User, password string, groups []User) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	var exists bool
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", m.Name).Scan(&exists); err != nil {
		return pgError(err)
	}
	if !exists {
		if err := pgEnsureGroupRoles(ctx, c); err != nil {
			return err
		}
		marker := pgIAMGroupUserRole
		if m.Type == TypeIAMGroupServiceAccount {
			marker = pgIAMGroupServiceAccountRole
		}
		if _, err := c.Exec(ctx, "CREATE ROLE "+pgQuoteIdent(m.Name)+" LOGIN PASSWORD "+quotePGString(password)+" IN ROLE "+marker); err != nil {
			return pgError(err)
		}
	}
	return pgSetGroups(ctx, c, m.Name, groups)
}

func (p *Postgres) DropGroupMember(ctx context.Context, m User) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	c, err := p.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if err := pgSetGroups(ctx, c, m.Name, nil); err != nil {
		return err
	}
	owns, err := pgOwnsObjects(ctx, c, m.Name)
	if err != nil {
		return pgError(err)
	}
	if owns {
		// Kept, outside every group, until its objects are reassigned.
		return nil
	}
	if err := p.pgDropPrivileges(ctx, c, m.Name); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "DROP ROLE "+pgQuoteIdent(m.Name)); err != nil {
		return pgError(err)
	}
	return nil
}

// MySQL: a group is a role 'NAME'@'DOMAIN', recorded in myIAMGroupsTable;
// a member's account is 'NAME'@'%', recorded in myIAMTable with its group
// type, granted the groups' roles as default roles.
const myIAMGroupsTable = "mysql.cloudburrow_iam_groups"

const myIAMGroupsDDL = "CREATE TABLE IF NOT EXISTS " + myIAMGroupsTable +
	" (name VARCHAR(32) NOT NULL, host VARCHAR(255) NOT NULL, email VARCHAR(255) NOT NULL, PRIMARY KEY (name, host)) ENGINE=InnoDB"

func myAccount(name, host string) string { return myString(name) + "@" + myString(host) }

func (m *MySQL) CreateIAMGroup(ctx context.Context, g User) error {
	return m.exec(ctx, myIAMGroupsDDL,
		"CREATE ROLE "+myAccount(g.Name, g.Host),
		"REPLACE INTO "+myIAMGroupsTable+" (name, host, email) VALUES ("+myString(g.Name)+", "+myString(g.Host)+", "+myString(g.IAMEmail)+")")
}

func (m *MySQL) DeleteIAMGroup(ctx context.Context, g User) error {
	return m.exec(ctx, "DROP ROLE "+myAccount(g.Name, g.Host),
		"DELETE FROM "+myIAMGroupsTable+" WHERE name = "+myString(g.Name)+" AND host = "+myString(g.Host))
}

// myIAMGroups are the groups myIAMGroupsTable records, keyed by account.
func (m *MySQL) myIAMGroups(ctx context.Context) (map[[2]string]string, error) {
	db, err := m.db()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[[2]string]string{}
	rows, err := db.QueryContext(ctx, "SELECT name, host, email FROM "+myIAMGroupsTable)
	if err != nil {
		// No table: no group has been added.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var n, h, e string
		if err := rows.Scan(&n, &h, &e); err != nil {
			return nil, err
		}
		out[[2]string{n, h}] = e
	}
	return out, rows.Err()
}

// mySetGroups makes acct's group roles exactly groups, as default roles.
func (m *MySQL) mySetGroups(ctx context.Context, name string, groups []User) error {
	all, err := m.myIAMGroups(ctx)
	if err != nil {
		return err
	}
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT FROM_USER, FROM_HOST FROM mysql.role_edges WHERE TO_USER = ? AND TO_HOST = '%'", name)
	if err != nil {
		return myError(err)
	}
	have := map[[2]string]bool{}
	for rows.Next() {
		var n, h string
		if err := rows.Scan(&n, &h); err != nil {
			rows.Close()
			return err
		}
		if _, ok := all[[2]string{n, h}]; ok {
			have[[2]string{n, h}] = true
		}
	}
	rows.Close()
	acct := myAccount(name, "%")
	var stmts []string
	want := map[[2]string]bool{}
	for _, g := range groups {
		k := [2]string{g.Name, g.Host}
		want[k] = true
		if !have[k] {
			stmts = append(stmts, "GRANT "+myAccount(g.Name, g.Host)+" TO "+acct)
		}
	}
	for k := range have {
		if !want[k] {
			stmts = append(stmts, "REVOKE "+myAccount(k[0], k[1])+" FROM "+acct)
		}
	}
	if len(stmts) == 0 {
		return nil
	}
	stmts = append(stmts, "SET DEFAULT ROLE ALL TO "+acct)
	return m.exec(ctx, stmts...)
}

func (m *MySQL) SyncGroupMember(ctx context.Context, u User, password string, groups []User) error {
	ctx, cancel := context.WithTimeout(ctx, engineTimeout)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return err
	}
	var n int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User = ? AND Host = '%'", u.Name).Scan(&n)
	db.Close()
	if err != nil {
		return myError(err)
	}
	if n == 0 {
		if err := m.exec(ctx,
			"CREATE TABLE IF NOT EXISTS "+myIAMTable+" (user VARCHAR(32) NOT NULL PRIMARY KEY, type VARCHAR(40) NOT NULL, email VARCHAR(255) NOT NULL) ENGINE=InnoDB",
			"CREATE USER "+myAccount(u.Name, "%")+" IDENTIFIED BY "+myString(password),
			"REPLACE INTO "+myIAMTable+" (user, type, email) VALUES ("+myString(u.Name)+", "+myString(u.Type)+", "+myString(u.IAMEmail)+")"); err != nil {
			return err
		}
	}
	return m.mySetGroups(ctx, u.Name, groups)
}

func (m *MySQL) DropGroupMember(ctx context.Context, u User) error {
	// MySQL accounts own no objects (a definer is a name, not an owner).
	return m.exec(ctx, "DROP USER IF EXISTS "+myAccount(u.Name, "%"),
		"DELETE FROM "+myIAMTable+" WHERE user = "+myString(u.Name))
}

// groupNamed is whether u is a group that users.get and delete may name
// name: its email, or its user name.
func groupNamed(u User, name string) bool {
	return u.Type == TypeIAMGroup && (strings.EqualFold(u.IAMEmail, name) || u.Name == name)
}
