package sqladmin

// Instance verbs over the server an instance already is (#1462):
// executeSql runs statements in it as a database user, truncateLog
// truncates MySQL's log tables, and preCheckMajorVersionUpgrade checks an
// upgrade as instances.patch would make it. The verbs that need what
// CloudBurrow does not run are refused, by verbWhy.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SecretAccessor reads a Secret Manager secret version's payload by its
// resource name: executeSql's passwordSecretVersion. The CLI wires this
// instance's Secret Manager.
type SecretAccessor interface {
	AccessSecretVersion(ctx context.Context, versionName string) ([]byte, error)
}

// SetSecretAccessor sets where executeSql reads a user's password; nil
// refuses passwordSecretVersion, naming the service to enable.
func (s *Server) SetSecretAccessor(a SecretAccessor) {
	s.emu.Lock()
	s.secrets = a
	s.emu.Unlock()
}

// sqlResult is one statement's result, as executeSql answers it.
type sqlResult struct {
	Columns []map[string]any
	Rows    [][]*string // nil is NULL
	Message string
	// More is set when rows beyond the limit were dropped.
	More bool
}

// sqlRunner is an engine that runs statements as a user.
type sqlRunner interface {
	// RunSQL runs stmt, one or more statements, in database as user,
	// returning each statement's result until one fails, and that error.
	RunSQL(ctx context.Context, user, password, database, stmt string, rowLimit int64) ([]sqlResult, error)
}

// authError is a database's refusal of a user's credentials.
type authError struct{ error }

func (p *Postgres) RunSQL(ctx context.Context, user, password, database, stmt string, rowLimit int64) ([]sqlResult, error) {
	host, port, err := net.SplitHostPort(p.HostAddr())
	if err != nil {
		return nil, Errorf(http.StatusServiceUnavailable, "the PostgreSQL tunnel is not up")
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: net.JoinHostPort(host, port),
		Path: "/" + database, RawQuery: "sslmode=disable"}
	c, err := pgx.Connect(ctx, u.String())
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "28P01" || pe.Code == "28000") {
			return nil, authError{err}
		}
		return nil, err
	}
	defer c.Close(context.Background())
	var out []sqlResult
	mrr := c.PgConn().Exec(ctx, stmt)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		var res sqlResult
		for _, fd := range rr.FieldDescriptions() {
			typ := "UNKNOWN"
			if t, ok := c.TypeMap().TypeForOID(fd.DataTypeOID); ok {
				typ = strings.ToUpper(t.Name)
			}
			res.Columns = append(res.Columns, map[string]any{"name": fd.Name, "type": typ})
		}
		for rr.NextRow() {
			if rowLimit > 0 && int64(len(res.Rows)) >= rowLimit {
				res.More = true
				continue
			}
			row := make([]*string, len(rr.Values()))
			for i, v := range rr.Values() {
				if v != nil {
					s := string(v)
					row[i] = &s
				}
			}
			res.Rows = append(res.Rows, row)
		}
		tag, err := rr.Close()
		if err != nil {
			_ = mrr.Close()
			return out, err
		}
		if len(res.Columns) == 0 {
			res.Message = tag.String()
		}
		out = append(out, res)
	}
	return out, mrr.Close()
}

func (m *MySQL) RunSQL(ctx context.Context, user, password, database, stmt string, rowLimit int64) ([]sqlResult, error) {
	addr := m.HostAddr()
	if addr == "" {
		return nil, Errorf(http.StatusServiceUnavailable, "the MySQL tunnel is not up")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = user, password, "tcp", addr, database
	cfg.Timeout, cfg.MultiStatements = engineTimeout, true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		var me *mysql.MySQLError
		if asMySQL(err, &me) && (me.Number == 1045 || me.Number == 1698) {
			return nil, authError{err}
		}
		return nil, err
	}
	defer rows.Close()
	var out []sqlResult
	for {
		var res sqlResult
		cts, _ := rows.ColumnTypes()
		for _, ct := range cts {
			res.Columns = append(res.Columns, map[string]any{"name": ct.Name(), "type": ct.DatabaseTypeName()})
		}
		for rows.Next() {
			vals := make([]sql.RawBytes, len(cts))
			ptrs := make([]any, len(cts))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return out, err
			}
			if rowLimit > 0 && int64(len(res.Rows)) >= rowLimit {
				res.More = true
				continue
			}
			row := make([]*string, len(vals))
			for i, v := range vals {
				if v != nil {
					s := string(v)
					row[i] = &s
				}
			}
			res.Rows = append(res.Rows, row)
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
		out = append(out, res)
		if !rows.NextResultSet() {
			break
		}
	}
	return out, rows.Err()
}

// executeSql is instances.executeSql: the statements run in the instance's
// server as the payload's user, whose password is read from this
// instance's Secret Manager, as Cloud SQL's Data API runs them.
func (s *Server) executeSQL(r *http.Request, project string, e Engine) (any, error) {
	var body struct {
		User                  string `json:"user"`
		SQLStatement          string `json:"sqlStatement"`
		Database              string `json:"database"`
		AutoIamAuthn          bool   `json:"autoIamAuthn"`
		RowLimit              int64  `json:"rowLimit,string"`
		PartialResultMode     string `json:"partialResultMode"`
		PasswordSecretVersion string `json:"passwordSecretVersion"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body.SQLStatement) == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: sqlStatement is required.")
	}
	if body.Database == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: database is required.")
	}
	if body.AutoIamAuthn || body.PasswordSecretVersion == "" {
		return nil, unimplemented("executeSql with IAM database authentication",
			"IAM database authentication is not served (no identities, ADR-0004); give user and passwordSecretVersion, a Secret Manager secret version holding the user's password")
	}
	if body.User == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: user is required with passwordSecretVersion.")
	}
	switch body.PartialResultMode {
	case "", "PARTIAL_RESULT_MODE_UNSPECIFIED", "FAIL_PARTIAL_RESULT", "ALLOW_PARTIAL_RESULT":
	default:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid value for partialResultMode: %s.", body.PartialResultMode)
	}
	if body.RowLimit < 0 {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: rowLimit must not be negative.")
	}
	if err := s.dataAPIAllowed(project, e); err != nil {
		return nil, err
	}
	runner, ok := e.(sqlRunner)
	if !ok {
		return nil, unimplemented("executeSql on this instance", "its server cannot run statements as a user")
	}
	s.emu.RLock()
	acc := s.secrets
	s.emu.RUnlock()
	if acc == nil {
		return nil, Errorf(http.StatusPreconditionFailed, "passwordSecretVersion is read from this instance's Secret Manager, which is not running; "+
			"start CloudBurrow with secretmanager in --services.")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*engineTimeout)
	defer cancel()
	pw, err := acc.AccessSecretVersion(ctx, body.PasswordSecretVersion)
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: cannot read passwordSecretVersion %s: %v.", body.PasswordSecretVersion, err)
	}
	start := time.Now()
	results, runErr := runner.RunSQL(ctx, body.User, strings.TrimRight(string(pw), "\r\n"), body.Database, body.SQLStatement, body.RowLimit)
	var apiErr *apiError
	if errors.As(runErr, &apiErr) {
		return nil, runErr
	}
	out := map[string]any{"metadata": map[string]any{
		"sqlStatementExecutionTime": fmt.Sprintf("%.6fs", time.Since(start).Seconds())}}
	items := []any{}
	partial := false
	for _, res := range results {
		rows := []any{}
		for _, row := range res.Rows {
			vals := []any{}
			for _, v := range row {
				if v == nil {
					vals = append(vals, map[string]any{"nullValue": true})
				} else {
					vals = append(vals, map[string]any{"value": *v})
				}
			}
			rows = append(rows, map[string]any{"values": vals})
		}
		item := map[string]any{"columns": res.Columns, "rows": rows}
		if res.Message != "" {
			item["message"] = res.Message
		}
		if res.More {
			item["partialResult"] = true
			partial = true
		}
		items = append(items, item)
	}
	if partial && body.PartialResultMode != "ALLOW_PARTIAL_RESULT" && runErr == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: the result has more than rowLimit (%d) rows; "+
			"set partialResultMode to ALLOW_PARTIAL_RESULT to get the first rowLimit rows.", body.RowLimit)
	}
	out["results"] = items
	if runErr != nil {
		code := 3 // INVALID_ARGUMENT: the database refused the statement
		var ae authError
		if errors.As(runErr, &ae) {
			code = 16 // UNAUTHENTICATED
		}
		out["status"] = map[string]any{"code": code, "message": runErr.Error()}
	}
	return out, nil
}

// dataAPIAllowed is whether the instance's settings.dataApiAccess allows
// executeSql: Cloud SQL's default is DISALLOW_DATA_API.
func (s *Server) dataAPIAllowed(project string, e Engine) error {
	c := s.createdNamed(project, e.Instance())
	if c == nil {
		return Errorf(http.StatusPreconditionFailed, "The Data API is not enabled for instance %s: its settings.dataApiAccess is DISALLOW_DATA_API, "+
			"and the settings of the servers `up` runs cannot be changed. Create an instance with settings.dataApiAccess ALLOW_DATA_API.", e.Instance())
	}
	s.emu.RLock()
	v, _ := c.spec.Settings["dataApiAccess"].(string)
	s.emu.RUnlock()
	if v != "ALLOW_DATA_API" {
		return Errorf(http.StatusPreconditionFailed, "The Data API is not enabled for instance %s. "+
			"Set settings.dataApiAccess to ALLOW_DATA_API to run executeSql.", e.Instance())
	}
	return nil
}

// truncateLog is instances.truncateLog: MySQL's general or slow log table.
func (s *Server) truncateLog(r *http.Request, base, project string, e Engine) (any, error) {
	var body struct {
		Ctx *struct {
			LogType string `json:"logType"`
		} `json:"truncateLogContext"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	m, ok := e.(*MySQL)
	if !ok {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: truncateLog is supported only for MySQL instances; %s is %s.", e.Instance(), e.DatabaseVersion())
	}
	if body.Ctx == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: truncateLogContext is required.")
	}
	table := map[string]string{"MYSQL_GENERAL_TABLE": "mysql.general_log", "MYSQL_SLOW_TABLE": "mysql.slow_log"}[body.Ctx.LogType]
	if table == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: truncateLogContext.logType must be MYSQL_GENERAL_TABLE or MYSQL_SLOW_TABLE, got %q.", body.Ctx.LogType)
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	if err := m.exec(r.Context(), "TRUNCATE TABLE "+table); err != nil {
		return nil, err
	}
	return s.done(base, project, e, "TRUNCATE_LOG", e.Instance()), nil
}

// preCheckMajorVersionUpgrade checks an upgrade to the target version as
// instances.patch would: what would refuse it is an ERROR response, and an
// upgrade it would make has none.
func (s *Server) preCheckMajorVersionUpgrade(r *http.Request, base, project string, e Engine) (any, error) {
	var body struct {
		Ctx *struct {
			TargetDatabaseVersion string `json:"targetDatabaseVersion"`
		} `json:"preCheckMajorVersionUpgradeContext"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Ctx == nil || body.Ctx.TargetDatabaseVersion == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: preCheckMajorVersionUpgradeContext.targetDatabaseVersion is required.")
	}
	target := body.Ctx.TargetDatabaseVersion
	c, err := s.createdFor(project, e.Instance(), "instances.preCheckMajorVersionUpgrade")
	if err != nil {
		return nil, err
	}
	if versionIndex(target) >= 0 && versionIndex(target) <= versionIndex(c.spec.DatabaseVersion) && engineOf(target) == engineOf(c.spec.DatabaseVersion) {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: the target version %s is not a major version upgrade of %s.", target, c.spec.DatabaseVersion)
	}
	s.emu.RLock()
	checkErr := s.checkUpgrade(c, target, false)
	s.emu.RUnlock()
	var ae *apiError
	if errors.As(checkErr, &ae) && ae.code == http.StatusBadRequest && strings.Contains(ae.msg, "Invalid value for databaseVersion") {
		return nil, checkErr
	}
	responses := []any{}
	if checkErr != nil {
		responses = append(responses, map[string]any{"messageType": "ERROR", "message": checkErr.Error(),
			"actionsRequired": []any{"Resolve the error, then upgrade with instances.patch."}})
	}
	op := s.done(base, project, e, "PRE_CHECK_MAJOR_VERSION_UPGRADE", e.Instance())
	pc := map[string]any{"kind": "sql#preCheckMajorVersionUpgradeContext", "targetDatabaseVersion": target, "preCheckResponse": responses}
	s.mu.Lock()
	s.ops[op["name"].(string)]["preCheckMajorVersionUpgradeContext"] = pc
	s.mu.Unlock()
	op["preCheckMajorVersionUpgradeContext"] = pc
	return op, nil
}
