package main

// The products behind Cloud SQL Studio and AlloyDB Studio (#2208): the
// sign-in dialog's databases and users, and the run of a script as the
// signed-in user (consoledbstudio.go has the Studio's calls).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// cloudSQLSavedQueriesFile and alloyDBSavedQueriesFile are the Studios'
// saved queries records in the instance directory.
const (
	cloudSQLSavedQueriesFile = "cloudsql-saved-queries.json"
	alloyDBSavedQueriesFile  = "alloydb-saved-queries.json"
)

// dbStudioRowLimit bounds the rows a statement's result holds before the
// 10 MB cut.
const dbStudioRowLimit = 200000

// StudioCall implements console.Studio for Cloud SQL Studio.
func (p cloudSQLInstancesProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	return p.studio().call(ctx, project, call, body)
}

// StudioTimeout lets a Studio run take Google's five minutes.
func (cloudSQLInstancesProvider) StudioTimeout(call string) time.Duration {
	if call == "run" {
		return dbStudioRunTimeout + 10*time.Second
	}
	return 0
}

func (p cloudSQLInstancesProvider) studio() dbStudioBackend {
	return dbStudioBackend{
		product: "Cloud SQL", savedQueries: p.savedQueries, iamPrincipal: p.iamPrincipal,
		engine: func(_ context.Context, project, instance string) (string, error) {
			if p.srv == nil {
				return "", errors.New("the Cloud SQL Admin API is not running in this process")
			}
			switch e := p.srv.StudioEngine(project, instance); e {
			case "POSTGRES", "MYSQL":
				return e, nil
			case "":
				return "", fmt.Errorf("the Cloud SQL instance %s does not exist in project %s", instance, project)
			default:
				return "", fmt.Errorf("Cloud SQL Studio for %s instances is not served here (#2208): its server runs statements only on PostgreSQL and MySQL", e)
			}
		},
		options: func(ctx context.Context, project, instance string) (map[string]any, error) {
			svc, err := p.api(ctx)
			if err != nil {
				return nil, err
			}
			in, err := svc.Instances.Get(project, instance).Context(ctx).Do()
			if err != nil {
				return nil, sqlAPIError(err)
			}
			dbs, err := svc.Databases.List(project, instance).Context(ctx).Do()
			if err != nil {
				return nil, sqlAPIError(err)
			}
			us, err := svc.Users.List(project, instance).Context(ctx).Do()
			if err != nil {
				return nil, sqlAPIError(err)
			}
			mysql := strings.HasPrefix(in.DatabaseVersion, "MYSQL")
			var names []string
			for _, d := range dbs.Items {
				names = append(names, d.Name)
			}
			users := []map[string]string{}
			for _, u := range us.Items {
				// MySQL's root is not supported by Studio, as Google documents.
				if mysql && u.Name == "root" {
					continue
				}
				t := u.Type
				if t == "" {
					t = sqladmin.TypeBuiltIn
				}
				users = append(users, map[string]string{"name": u.Name, "type": t})
			}
			engine := "POSTGRES"
			if mysql {
				engine = "MYSQL"
			}
			return map[string]any{"engine": engine, "databases": names, "users": users, "state": in.State,
				"version": in.DatabaseVersion}, nil
		},
		run: func(ctx context.Context, l *dbStudioLogin, sql string) ([]dbStudioResult, error) {
			if p.srv == nil {
				return nil, errors.New("the Cloud SQL Admin API is not running in this process")
			}
			res, role, err := p.srv.StudioSQL(ctx, sqladmin.StudioLogin{Project: l.project, Instance: l.instance, Database: l.database,
				User: l.user, Password: l.password, IAMEmail: l.iam}, sql, dbStudioRowLimit)
			if role != "" {
				l.role = role
			}
			out := make([]dbStudioResult, 0, len(res))
			for _, r := range res {
				rows := r.Rows
				if rows == nil {
					rows = [][]*string{}
				}
				out = append(out, dbStudioResult{Columns: r.Columns, Types: r.Types, Rows: rows, Message: r.Message, Partial: r.More})
			}
			if err != nil {
				return out, sqlAPIError(err)
			}
			return out, nil
		},
	}
}

// StudioCall implements console.Studio for AlloyDB Studio: a cluster's,
// signed in to its primary instance.
func (p alloyDBProvider) StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error) {
	return p.studio().call(ctx, project, call, body)
}

// StudioTimeout lets a Studio run take five minutes, as Cloud SQL
// Studio's.
func (alloyDBProvider) StudioTimeout(call string) time.Duration {
	if call == "run" {
		return dbStudioRunTimeout + 10*time.Second
	}
	return 0
}

// alloyDBPrimary is cluster's primary instance, which AlloyDB Studio
// signs in to.
func alloyDBPrimary(ctx context.Context, svc *alloyDBService, project, cluster string) (string, error) {
	if !strings.HasPrefix(cluster, "projects/"+project+"/locations/") || !strings.Contains(cluster, "/clusters/") {
		return "", fmt.Errorf("%q is not a cluster of project %s", cluster, project)
	}
	s, err := alloyDBAPI(svc)
	if err != nil {
		return "", err
	}
	list, err := s.ListInstances(ctx, &alloydbpb.ListInstancesRequest{Parent: cluster})
	if err != nil {
		return "", err
	}
	for _, in := range list.GetInstances() {
		if in.GetInstanceType() == alloydbpb.Instance_PRIMARY {
			if in.GetState() != alloydbpb.Instance_READY {
				return "", fmt.Errorf("the cluster's primary instance is %s: AlloyDB Studio queries only a running instance", alloyDBState(in.GetState()))
			}
			return in.GetName(), nil
		}
	}
	return "", errors.New("the cluster has no primary instance: create one to use AlloyDB Studio")
}

func (p alloyDBProvider) studio() dbStudioBackend {
	return dbStudioBackend{
		product: "AlloyDB", savedQueries: p.savedQueries, iamPrincipal: p.iamPrincipal,
		engine: func(ctx context.Context, project, cluster string) (string, error) {
			_, err := alloyDBPrimary(ctx, p.svc, project, cluster)
			return "POSTGRES", err
		},
		options: func(ctx context.Context, project, cluster string) (map[string]any, error) {
			if _, err := alloyDBPrimary(ctx, p.svc, project, cluster); err != nil {
				return nil, err
			}
			s, err := alloyDBAPI(p.svc)
			if err != nil {
				return nil, err
			}
			us, err := s.ListUsers(ctx, &alloydbpb.ListUsersRequest{Parent: cluster})
			if err != nil {
				return nil, err
			}
			users := []map[string]string{}
			for _, u := range us.GetUsers() {
				id := u.GetName()[strings.LastIndex(u.GetName(), "/")+1:]
				users = append(users, map[string]string{"name": id, "type": u.GetUserType().String()})
			}
			dbs, err := s.ListDatabases(ctx, &alloydbpb.ListDatabasesRequest{Parent: cluster})
			if err != nil {
				return nil, err
			}
			var names []string
			for _, d := range dbs.GetDatabases() {
				if d.GetIsTemplateDatabase() {
					continue
				}
				names = append(names, d.GetName()[strings.LastIndex(d.GetName(), "/")+1:])
			}
			sort.Strings(names)
			return map[string]any{"engine": "POSTGRES", "databases": names, "users": users}, nil
		},
		run: func(ctx context.Context, l *dbStudioLogin, sql string) ([]dbStudioResult, error) {
			s, err := alloyDBAPI(p.svc)
			if err != nil {
				return nil, err
			}
			primary, err := alloyDBPrimary(ctx, p.svc, l.project, l.instance)
			if err != nil {
				return nil, err
			}
			req := &alloydbpb.ExecuteSqlRequest{Instance: primary, Database: l.database, User: l.user, SqlStatement: sql,
				UserCredential: &alloydbpb.ExecuteSqlRequest_Password{Password: l.password}}
			l.role = l.user
			if l.iam != "" {
				l.role = strings.TrimSuffix(strings.ToLower(l.iam), ".gserviceaccount.com")
				req.User, req.UserCredential = l.role, nil
			}
			resp, err := s.StudioExecuteSql(ctx, req, l.iam)
			if err != nil {
				return nil, errors.New(grpcMessage(err))
			}
			out := []dbStudioResult{}
			for _, r := range resp.GetSqlResults() {
				res := dbStudioResult{Columns: []string{}, Rows: [][]*string{}}
				for _, c := range r.GetColumns() {
					res.Columns = append(res.Columns, c.GetName())
					res.Types = append(res.Types, c.GetType())
				}
				for _, row := range r.GetRows() {
					vals := make([]*string, 0, len(row.GetValues()))
					for _, v := range row.GetValues() {
						if v.GetNullValue() {
							vals = append(vals, nil)
							continue
						}
						s := v.GetValue()
						vals = append(vals, &s)
					}
					res.Rows = append(res.Rows, vals)
				}
				out = append(out, res)
			}
			if md := resp.GetMetadata(); md.GetStatus() == alloydbpb.ExecuteSqlMetadata_ERROR {
				return out, errors.New(md.GetMessage())
			}
			return out, nil
		},
	}
}

// grpcMessage is an in-process API error's message, without its code.
func grpcMessage(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}
