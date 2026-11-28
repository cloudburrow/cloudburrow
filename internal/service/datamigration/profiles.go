package datamigration

import (
	"context"
	"strings"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// destinationTimeout bounds the creation of a destination's server.
const destinationTimeout = 20 * time.Minute

// CloudSQLVersions are the Cloud SQL database versions Database Migration
// Service creates a destination of, newest first (the reference's
// SqlDatabaseVersion and databaseVersionName); the instance serves a
// subset (Options.CloudSQLVersions).
var mysqlVersions = []string{"MYSQL_8_4", "MYSQL_8_0", "MYSQL_5_7", "MYSQL_5_6"}

// profileEngine is the engine of a source or destination profile.
func profileEngine(p *dmspb.ConnectionProfile) dmspb.DatabaseEngine {
	switch {
	case p.GetMysql() != nil:
		return dmspb.DatabaseEngine_MYSQL
	case p.GetPostgresql() != nil, p.GetAlloydb() != nil:
		return dmspb.DatabaseEngine_POSTGRESQL
	case p.GetOracle() != nil:
		return dmspb.DatabaseEngine_ORACLE
	case p.GetCloudsql() != nil:
		if strings.HasPrefix(cloudSQLVersion(p.GetCloudsql().GetSettings()), "MYSQL_") {
			return dmspb.DatabaseEngine_MYSQL
		}
		return dmspb.DatabaseEngine_POSTGRESQL
	}
	return dmspb.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED
}

// cloudSQLVersion is a cloudsql profile's version: databaseVersionName,
// or the deprecated databaseVersion.
func cloudSQLVersion(s *dmspb.CloudSqlSettings) string {
	if s == nil {
		return ""
	}
	if v := str(s, "database_version_name"); v != "" {
		return v
	}
	if v := s.GetDatabaseVersion(); v != dmspb.CloudSqlSettings_SQL_DATABASE_VERSION_UNSPECIFIED {
		return v.String()
	}
	return ""
}

// isSource is a profile a migration job reads from: a MySQL or
// PostgreSQL server, not a destination profile of an existing instance.
func isSource(p *dmspb.ConnectionProfile) bool {
	return (p.GetMysql() != nil || p.GetPostgresql() != nil) && !existingDestination(p)
}

// existingDestination is a mysql or postgresql profile of an existing
// Cloud SQL instance (cloudSqlId) or AlloyDB cluster (alloydbClusterId)
// that a job migrates into (#2257): role DESTINATION, or no host, as
// gcloud's `connection-profiles create mysql --role=DESTINATION
// --cloudsql-instance=ID` makes it ("For PostgreSQL or MySQL destination
// profiles with Cloud SQL or AlloyDB, this flag [--host] is optional if
// the instance or cluster is provided", gcloud 594's flags.py; the
// existing-instance guide's `connection-profiles create mysql ... --cloudsql-instance=DESTINATION_INSTANCE_ID`,
// https://docs.cloud.google.com/database-migration/docs/mysql/create-migration-job-existing-instance,
// read 2026-11-25). An alloydb profile without settings is one too.
func existingDestination(p *dmspb.ConnectionProfile) bool {
	host, id := "", ""
	switch {
	case p.GetMysql() != nil:
		host, id = p.GetMysql().GetHost(), p.GetMysql().GetCloudSqlId()
	case p.GetPostgresql() != nil:
		host, id = p.GetPostgresql().GetHost(), p.GetPostgresql().GetCloudSqlId()
		if a := str(p.GetPostgresql(), "alloydb_cluster_id"); a != "" {
			id = a
		}
	case p.GetAlloydb() != nil:
		return p.GetAlloydb().GetSettings() == nil
	default:
		return false
	}
	return id != "" && (str(p, "role") == "DESTINATION" || host == "")
}

// redact is p as it is answered: input-only fields cleared, their *Set
// flags raised.
func redact(p *dmspb.ConnectionProfile) *dmspb.ConnectionProfile {
	out := clone(p)
	clearSSL := func(c *dmspb.SslConfig) {
		if c != nil {
			c.ClientKey, c.ClientCertificate, c.CaCertificate = "", "", ""
		}
	}
	switch {
	case out.GetMysql() != nil:
		m := out.GetMysql()
		m.PasswordSet, m.Password = m.Password != "", ""
		clearSSL(m.Ssl)
		redactSSH(m)
	case out.GetPostgresql() != nil:
		m := out.GetPostgresql()
		m.PasswordSet, m.Password = m.Password != "", ""
		clearSSL(m.Ssl)
		redactSSH(m)
	case out.GetCloudsql() != nil:
		if st := out.GetCloudsql().GetSettings(); st != nil {
			st.RootPasswordSet, st.RootPassword = st.RootPassword != "", ""
		}
	case out.GetAlloydb() != nil:
		if st := out.GetAlloydb().GetSettings(); st != nil {
			// initialUser is "Required. Input only."
			st.InitialUser = nil
		}
	case out.GetOracle() != nil:
		m := out.GetOracle()
		m.PasswordSet, m.Password = m.Password != "", ""
		clearSSL(m.Ssl)
	}
	return out
}

// checkEndpoint validates a mysql or postgresql profile's connection.
func checkEndpoint(kind, host string, port int32, user, password string, ssl *dmspb.SslConfig, update bool) error {
	if host == "" {
		return apierror.InvalidArgument("connection_profile.%s.host is required", kind)
	}
	if port <= 0 || port > 65535 {
		return apierror.InvalidArgument("connection_profile.%s.port %d must be between 1 and 65535", kind, port)
	}
	if user == "" {
		return apierror.InvalidArgument("connection_profile.%s.username is required", kind)
	}
	if password == "" && !update {
		return apierror.InvalidArgument("connection_profile.%s.password is required", kind)
	}
	switch t := sslType(ssl); {
	case ssl == nil, t == 0, t == SSLRequired, t == SSLNone:
		if ssl.GetCaCertificate() != "" || ssl.GetClientCertificate() != "" {
			return apierror.InvalidArgument("connection_profile.%s.ssl: certificates are for SSL types SERVER_ONLY and SERVER_CLIENT", kind)
		}
	case t == int32(dmspb.SslConfig_SERVER_ONLY), t == int32(dmspb.SslConfig_SERVER_CLIENT):
		if ssl.GetCaCertificate() == "" {
			if update {
				// The certificates are input only: an update that names
				// the type without them keeps the stored ones.
				return nil
			}
			return apierror.InvalidArgument("connection_profile.%s.ssl.ca_certificate is required for SSL type %s", kind, dmspb.SslConfig_SslType(t))
		}
		if t == int32(dmspb.SslConfig_SERVER_CLIENT) && (ssl.GetClientKey() == "" || ssl.GetClientCertificate() == "") {
			return apierror.InvalidArgument("connection_profile.%s.ssl.client_key and client_certificate are required for SSL type SERVER_CLIENT", kind)
		}
		if t == int32(dmspb.SslConfig_SERVER_ONLY) && (ssl.GetClientKey() != "" || ssl.GetClientCertificate() != "") {
			return apierror.InvalidArgument("connection_profile.%s.ssl: SSL type SERVER_ONLY takes only ca_certificate", kind)
		}
		return checkCertificates(kind, ssl)
	default:
		return apierror.InvalidArgument("connection_profile.%s.ssl.type %d is not an SslType", kind, t)
	}
	if ssl.GetClientKey() != "" && ssl.GetClientCertificate() == "" {
		return apierror.InvalidArgument("connection_profile.%s.ssl.client_key requires client_certificate", kind)
	}
	return nil
}

// checkProfile validates a new profile; the caller holds mu.
func (s *Server) checkProfile(project, location string, p *dmspb.ConnectionProfile) error {
	if p == nil {
		return apierror.InvalidArgument("connection_profile is required")
	}
	if r := str(p, "role"); r != "" && r != "ROLE_UNSPECIFIED" && r != "SOURCE" && r != "DESTINATION" {
		return apierror.InvalidArgument("connection_profile.role %q must be SOURCE or DESTINATION", r)
	}
	switch {
	case existingDestination(p):
		return s.checkExistingDestination(p)
	case p.GetMysql() != nil:
		m := p.GetMysql()
		if err := checkEndpoint("mysql", m.GetHost(), m.GetPort(), m.GetUsername(), m.GetPassword(), m.GetSsl(), false); err != nil {
			return err
		}
		if has(m, "private_service_connect_connectivity") {
			return unimplemented("connection_profile.mysql.private_service_connect_connectivity",
				"Private Service Connect service attachments are not served; reach the source directly or through forward SSH (docs/gaps.md)")
		}
		if err := checkForwardSSH("mysql", m, false); err != nil {
			return err
		}
		return s.checkPrivateConnectivity(project, location, m)
	case p.GetPostgresql() != nil:
		m := p.GetPostgresql()
		if err := checkEndpoint("postgresql", m.GetHost(), m.GetPort(), m.GetUsername(), m.GetPassword(), m.GetSsl(), false); err != nil {
			return err
		}
		if m.GetPrivateServiceConnectConnectivity() != nil {
			return unimplemented("connection_profile.postgresql.private_service_connect_connectivity",
				"Private Service Connect service attachments are not served; reach the source directly or through forward SSH (docs/gaps.md)")
		}
		if err := checkForwardSSH("postgresql", m, false); err != nil {
			return err
		}
		if boolField(m, "enable_iam_authentication") {
			return unimplemented("connection_profile.postgresql.enable_iam_authentication",
				"the destinations' servers sign in to a source with the profile's password (docs/gaps.md)")
		}
		return s.checkPrivateConnectivity(project, location, m)
	case p.GetCloudsql() != nil:
		return s.checkCloudSQL(project, location, p.GetCloudsql())
	case p.GetAlloydb() != nil:
		return s.checkAlloyDB(p.GetAlloydb())
	case p.GetOracle() != nil:
		return unimplemented("Oracle connection profiles",
			"an Oracle source migrates through a conversion workspace (heterogeneous migration), which CloudBurrow does not run: no Oracle server is available to it")
	}
	return apierror.InvalidArgument("connection_profile must set one of mysql, postgresql, oracle, cloudsql or alloydb")
}

// checkPrivateConnectivity checks a source's privateConnectivity names a
// private connection of this project and location that is CREATED; the
// caller holds mu.
func (s *Server) checkPrivateConnectivity(project, location string, m proto.Message) error {
	pc := nestedStr(m, "private_connectivity", "private_connection")
	if pc == "" {
		if has(m, "private_connectivity") {
			return apierror.InvalidArgument("private_connectivity.private_connection is required")
		}
		return nil
	}
	mm := privateRE.FindStringSubmatch(pc)
	if mm == nil {
		return apierror.InvalidArgument("private_connectivity.private_connection %q must be projects/{project}/locations/{location}/privateConnections/{private_connection}", pc)
	}
	if mm[1] != project || mm[2] != location {
		return apierror.InvalidArgument("private_connectivity.private_connection %s must be in projects/%s/locations/%s", pc, project, location)
	}
	c, ok := s.privates[pc]
	if !ok {
		return apierror.NotFound("Resource '%s' was not found", pc)
	}
	if c.GetState() != dmspb.PrivateConnection_CREATED {
		return failedPrecondition("private connection %s is %s, not CREATED", pc, c.GetState())
	}
	return nil
}

func (s *Server) checkCloudSQL(project, location string, c *dmspb.CloudSqlConnectionProfile) error {
	if !s.opts.CloudSQL {
		return failedPrecondition("a Cloud SQL destination needs Cloud SQL: start the instance with --services including cloudsql or cloudsql-mysql")
	}
	st := c.GetSettings()
	if st == nil {
		return apierror.InvalidArgument("connection_profile.cloudsql.settings is required: Database Migration Service creates the destination instance from them")
	}
	if c.GetCloudSqlId() != "" {
		return apierror.InvalidArgument("connection_profile.cloudsql.cloud_sql_id is output only")
	}
	v := cloudSQLVersion(st)
	if v == "" {
		return apierror.InvalidArgument("connection_profile.cloudsql.settings.database_version_name (or database_version) is required")
	}
	if served := s.opts.CloudSQLVersions; served != nil {
		ok := false
		for _, x := range served() {
			ok = ok || x == v
		}
		if !ok {
			return unimplemented("a Cloud SQL destination of database version "+v, "this instance's Cloud SQL does not serve it")
		}
	}
	if strings.HasPrefix(v, "SQLSERVER_") {
		return unimplemented("a Cloud SQL for SQL Server destination", "SQL Server sources and their backups-based migration are not run (docs/gaps.md)")
	}
	if st.GetTier() == "" {
		return apierror.InvalidArgument("connection_profile.cloudsql.settings.tier is required")
	}
	if st.GetAvailabilityType() == dmspb.CloudSqlSettings_REGIONAL {
		return unimplemented("a REGIONAL Cloud SQL destination",
			"the destination replica runs as one server; create it ZONAL and change its availability after promotion (docs/gaps.md)")
	}
	if st.GetCmekKeyName() != "" {
		return unimplemented("connection_profile.cloudsql.settings.cmek_key_name", "a migration replica is created without a customer-managed key (docs/gaps.md)")
	}
	src := st.GetSourceId()
	if src == "" {
		return apierror.InvalidArgument("connection_profile.cloudsql.settings.source_id is required: the destination instance replicates that source profile")
	}
	sp, err := s.profileLocked(src)
	if err != nil {
		return err
	}
	if !isSource(sp) {
		return apierror.InvalidArgument("connection_profile.cloudsql.settings.source_id %s is not a MySQL or PostgreSQL source profile", src)
	}
	if want := profileEngine(sp); (want == dmspb.DatabaseEngine_MYSQL) != strings.HasPrefix(v, "MYSQL_") {
		return apierror.InvalidArgument("the destination's database version %s is not the engine of its source %s (%s)", v, src, want)
	}
	if m := profileRE.FindStringSubmatch(src); m[1] != project || m[2] != location {
		return apierror.InvalidArgument("connection_profile.cloudsql.settings.source_id %s must be in projects/%s/locations/%s", src, project, location)
	}
	return nil
}

func (s *Server) checkAlloyDB(a *dmspb.AlloyDbConnectionProfile) error {
	if !s.opts.AlloyDB {
		return failedPrecondition("an AlloyDB destination needs AlloyDB: start the instance with --services including alloydb")
	}
	if a.GetClusterId() == "" {
		return apierror.InvalidArgument("connection_profile.alloydb.cluster_id is required")
	}
	if !idRE.MatchString(a.GetClusterId()) {
		return apierror.InvalidArgument("connection_profile.alloydb.cluster_id %q is not a valid cluster ID", a.GetClusterId())
	}
	st := a.GetSettings()
	if st == nil {
		// A profile of an existing cluster (#2257), checked by
		// checkExistingDestination.
		return nil
	}
	if st.GetInitialUser().GetUser() == "" || st.GetInitialUser().GetPassword() == "" {
		return apierror.InvalidArgument("connection_profile.alloydb.settings.initial_user's user and password are required")
	}
	if st.GetVpcNetwork() == "" {
		return apierror.InvalidArgument("connection_profile.alloydb.settings.vpc_network is required")
	}
	if !strings.HasPrefix(st.GetVpcNetwork(), "projects/") || !strings.Contains(st.GetVpcNetwork(), "/global/networks/") {
		return apierror.InvalidArgument("connection_profile.alloydb.settings.vpc_network %q must be projects/{project}/global/networks/{network}", st.GetVpcNetwork())
	}
	pi := st.GetPrimaryInstanceSettings()
	if pi.GetId() == "" {
		return apierror.InvalidArgument("connection_profile.alloydb.settings.primary_instance_settings.id is required")
	}
	if !idRE.MatchString(pi.GetId()) {
		return apierror.InvalidArgument("connection_profile.alloydb.settings.primary_instance_settings.id %q must match [a-z0-9-]+", pi.GetId())
	}
	if pi.GetMachineConfig().GetCpuCount() <= 0 {
		return apierror.InvalidArgument("connection_profile.alloydb.settings.primary_instance_settings.machine_config.cpu_count is required")
	}
	if v := str(st, "database_version"); v != "" && v != "DATABASE_VERSION_UNSPECIFIED" &&
		v != "POSTGRES_14" && v != "POSTGRES_15" && v != "POSTGRES_16" && v != "POSTGRES_17" && v != "POSTGRES_18" {
		return apierror.InvalidArgument("connection_profile.alloydb.settings.database_version %q must be POSTGRES_14 to POSTGRES_18", v)
	}
	if st.GetEncryptionConfig().GetKmsKeyName() != "" {
		return unimplemented("connection_profile.alloydb.settings.encryption_config", "the destination cluster is created without a customer-managed key (docs/gaps.md)")
	}
	return nil
}

// profileLocked is the profile named name; the caller holds mu.
func (s *Server) profileLocked(name string) (*dmspb.ConnectionProfile, error) {
	if profileRE.FindStringSubmatch(name) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/connectionProfiles/{connection_profile}", name)
	}
	p, ok := s.profiles[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return p, nil
}

// targetOf is the destination server a cloudsql or alloydb profile, or a
// destination profile of an existing instance or cluster, names; an
// existing one's version is the server's versions record.
func (s *Server) targetOf(p *dmspb.ConnectionProfile) (Target, bool) {
	t, ok := targetOf(p)
	if ok && t.Existing {
		t.Version = s.versions[p.GetName()]
	}
	return t, ok
}

// existingID is an existing destination profile's instance or cluster:
// cloudSql reports a Cloud SQL instance.
func existingID(p *dmspb.ConnectionProfile) (cloudSQL bool, id string) {
	switch {
	case p.GetAlloydb() != nil:
		return false, p.GetAlloydb().GetClusterId()
	case p.GetPostgresql() != nil && str(p.GetPostgresql(), "alloydb_cluster_id") != "":
		return false, str(p.GetPostgresql(), "alloydb_cluster_id")
	case p.GetMysql() != nil:
		return true, p.GetMysql().GetCloudSqlId()
	}
	return true, p.GetPostgresql().GetCloudSqlId()
}

func targetOf(p *dmspb.ConnectionProfile) (Target, bool) {
	m := profileRE.FindStringSubmatch(p.GetName())
	if m == nil {
		return Target{}, false
	}
	if existingDestination(p) {
		t := Target{Project: m[1], Location: m[2], Existing: true}
		t.CloudSQL, t.ID = existingID(p)
		return t, true
	}
	switch {
	case p.GetCloudsql() != nil:
		id := p.GetCloudsql().GetCloudSqlId()
		if id == "" {
			id = m[3]
		}
		return Target{CloudSQL: true, Project: m[1], Location: m[2], ID: id, Version: cloudSQLVersion(p.GetCloudsql().GetSettings())}, true
	case p.GetAlloydb() != nil:
		v := str(p.GetAlloydb().GetSettings(), "database_version")
		return Target{Project: m[1], Location: m[2], ID: p.GetAlloydb().GetClusterId(), Version: v}, true
	}
	return Target{}, false
}

// sourceOf is a source profile's connection.
func sourceOf(p *dmspb.ConnectionProfile) Source {
	tlsOf := func(src *Source, c *dmspb.SslConfig) {
		switch sslType(c) {
		case SSLRequired:
			src.RequireSSL = true
		case int32(dmspb.SslConfig_SERVER_ONLY), int32(dmspb.SslConfig_SERVER_CLIENT):
			src.RequireSSL = true
			src.CACert, src.ClientCert, src.ClientKey = c.GetCaCertificate(), c.GetClientCertificate(), c.GetClientKey()
		}
	}
	var out Source
	if m := p.GetMysql(); m != nil {
		out = Source{Engine: dmspb.DatabaseEngine_MYSQL, Host: m.GetHost(), Port: int(m.GetPort()), User: m.GetUsername(), Password: m.GetPassword(), SSH: sshOf(m)}
		tlsOf(&out, m.GetSsl())
	} else {
		m := p.GetPostgresql()
		out = Source{Engine: dmspb.DatabaseEngine_POSTGRESQL, Host: m.GetHost(), Port: int(m.GetPort()), User: m.GetUsername(), Password: m.GetPassword(), SSH: sshOf(m)}
		tlsOf(&out, m.GetSsl())
	}
	out.Name = p.GetName()
	return out
}

func (s *Server) CreateConnectionProfile(_ context.Context, req *dmspb.CreateConnectionProfileRequest) (*longrunningpb.Operation, error) {
	project, location, err := checkParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := checkID("connection_profile", req.GetConnectionProfileId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	p := clone(req.GetConnectionProfile())
	if err := s.checkProfile(project, location, p); err != nil {
		return nil, err
	}
	if (req.GetValidateOnly() || req.GetSkipValidation()) && p.GetOracle() == nil {
		// "Supported only for Oracle connection profiles."
		return nil, apierror.InvalidArgument("validate_only and skip_validation are supported only for Oracle connection profiles")
	}
	name := req.GetParent() + "/connectionProfiles/" + req.GetConnectionProfileId()
	if _, ok := s.profiles[name]; ok {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	probe := &dmspb.ConnectionProfile{Name: name, ConnectionProfile: p.ConnectionProfile}
	if t, ok := targetOf(probe); ok && t.CloudSQL && !t.Existing {
		for _, o := range s.profiles {
			if ot, ok := targetOf(o); ok && ot.CloudSQL && ot.Project == t.Project && ot.ID == t.ID {
				return nil, apierror.AlreadyExists("the Cloud SQL instance %s is already the destination of %s", t.ID, o.GetName())
			}
		}
	}
	if existingDestination(probe) {
		// An existing instance or cluster (#2257): checked to be there,
		// standalone and running, and its version kept.
		t, _ := targetOf(probe)
		if s.opts.Backend == nil {
			return nil, failedPrecondition("this server runs no destinations")
		}
		if t.CloudSQL && !s.opts.CloudSQL {
			return nil, failedPrecondition("a Cloud SQL destination needs Cloud SQL: start the instance with --services including cloudsql or cloudsql-mysql")
		}
		if !t.CloudSQL && !s.opts.AlloyDB {
			return nil, failedPrecondition("an AlloyDB destination needs AlloyDB: start the instance with --services including alloydb")
		}
		v, err := s.opts.Backend.Existing(context.Background(), t)
		if err != nil {
			return nil, err
		}
		if p.GetMysql() != nil && !strings.HasPrefix(v, "MYSQL_") || p.GetPostgresql() != nil && !strings.HasPrefix(v, "POSTGRES_") {
			return nil, apierror.InvalidArgument("the destination %s runs %s, not the profile's engine", t.ID, v)
		}
		now := ts(s.now())
		p.Name, p.CreateTime, p.UpdateTime, p.Error, p.State = name, now, now, nil, dmspb.ConnectionProfile_READY
		if p.Provider == dmspb.DatabaseProvider_DATABASE_PROVIDER_UNSPECIFIED {
			p.Provider = dmspb.DatabaseProvider_CLOUDSQL
			if !t.CloudSQL {
				p.Provider = dmspb.DatabaseProvider_ALLOYDB
			}
		}
		s.profiles[name] = p
		s.versions[name] = v
		return s.done(req.GetParent(), "create", name, req.GetRequestId(), redact(p))
	}
	if s.opts.Backend == nil && (p.GetCloudsql() != nil || p.GetAlloydb().GetSettings() != nil) {
		return nil, failedPrecondition("this server runs no destinations")
	}
	now := ts(s.now())
	p.Name, p.CreateTime, p.UpdateTime, p.Error = name, now, now, nil
	switch {
	case p.GetCloudsql() != nil && p.Provider == dmspb.DatabaseProvider_DATABASE_PROVIDER_UNSPECIFIED:
		p.Provider = dmspb.DatabaseProvider_CLOUDSQL
	case p.GetAlloydb() != nil && p.Provider == dmspb.DatabaseProvider_DATABASE_PROVIDER_UNSPECIFIED:
		p.Provider = dmspb.DatabaseProvider_ALLOYDB
	}
	if p.GetCloudsql() != nil {
		p.GetCloudsql().CloudSqlId = req.GetConnectionProfileId()
	}
	provision := p.GetCloudsql() != nil || p.GetAlloydb().GetSettings() != nil
	if !provision {
		p.State = dmspb.ConnectionProfile_READY
		s.profiles[name] = p
		return s.done(req.GetParent(), "create", name, req.GetRequestId(), redact(p))
	}
	p.State = dmspb.ConnectionProfile_CREATING
	s.profiles[name] = p
	op, err := s.startOp(req.GetParent(), "create", name, req.GetRequestId())
	if err != nil {
		delete(s.profiles, name)
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	var src Source
	if c := p.GetCloudsql(); c != nil {
		src = sourceOf(s.profiles[c.GetSettings().GetSourceId()])
	}
	t, _ := targetOf(p)
	settingsCS, settingsADB := clone(p).GetCloudsql().GetSettings(), clone(p).GetAlloydb().GetSettings()
	opName := op.Name
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), destinationTimeout)
		defer cancel()
		var addrs CloudSQLAddresses
		var version string
		var err error
		if t.CloudSQL {
			addrs, err = s.opts.Backend.CreateCloudSQL(ctx, t, settingsCS, src)
		} else {
			version, err = s.createAlloyDB(ctx, t, settingsADB)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.profiles[name]
		if cur == nil {
			s.finishOp(opName, nil, apierror.Aborted("connection profile %s was deleted while it was created", name))
			_ = s.save()
			return
		}
		cur.UpdateTime = ts(s.now())
		if err != nil {
			cur.State, cur.Error = dmspb.ConnectionProfile_FAILED, statusOf(err)
			s.finishOp(opName, nil, err)
			_ = s.save()
			return
		}
		cur.State = dmspb.ConnectionProfile_READY
		if c := cur.GetCloudsql(); c != nil {
			c.PublicIp, c.PrivateIp = addrs.PublicIP, addrs.PrivateIP
		}
		if a := cur.GetAlloydb().GetSettings(); a != nil && version != "" {
			setStr(a, "database_version", version)
		}
		s.finishOp(opName, redact(cur), nil)
		_ = s.save()
	})
	return clone(op), nil
}

// createAlloyDB is the Backend's AlloyDB creation, answering the cluster's
// database version.
func (s *Server) createAlloyDB(ctx context.Context, t Target, st *dmspb.AlloyDbSettings) (string, error) {
	v, err := s.opts.Backend.CreateAlloyDB(ctx, t, st)
	if v == "" {
		v = t.Version
	}
	return v, err
}

func (s *Server) GetConnectionProfile(_ context.Context, req *dmspb.GetConnectionProfileRequest) (*dmspb.ConnectionProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.profileLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return redact(p), nil
}

func (s *Server) ListConnectionProfiles(_ context.Context, req *dmspb.ListConnectionProfilesRequest) (*dmspb.ListConnectionProfilesResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var all []*dmspb.ConnectionProfile
	for _, k := range sortedKeys(s.profiles, req.GetParent()+"/connectionProfiles/") {
		all = append(all, redact(s.profiles[k]))
	}
	s.mu.Unlock()
	out, next, err := listPage("dms-profiles:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &dmspb.ListConnectionProfilesResponse{ConnectionProfiles: out, NextPageToken: next}, nil
}

func (s *Server) UpdateConnectionProfile(_ context.Context, req *dmspb.UpdateConnectionProfileRequest) (*longrunningpb.Operation, error) {
	in := req.GetConnectionProfile()
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	cur, err := s.profileLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if cur.State == dmspb.ConnectionProfile_CREATING {
		return nil, failedPrecondition("connection profile %s is being created", cur.GetName())
	}
	next := clone(cur)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	for _, path := range paths {
		switch path {
		case "display_name", "displayName":
			next.DisplayName = in.GetDisplayName()
		case "labels":
			next.Labels = in.GetLabels()
		default:
			if err := updateEndpoint(next, in, path); err != nil {
				return nil, err
			}
		}
	}
	if isSource(next) {
		if m := next.GetMysql(); m != nil {
			keepSSL(cur.GetMysql().GetSsl(), m.GetSsl())
			keepSSHSecret(cur.GetMysql(), m)
			err = checkEndpoint("mysql", m.GetHost(), m.GetPort(), m.GetUsername(), m.GetPassword(), m.GetSsl(), true)
			if err == nil {
				err = checkForwardSSH("mysql", m, true)
			}
		} else {
			m := next.GetPostgresql()
			keepSSL(cur.GetPostgresql().GetSsl(), m.GetSsl())
			keepSSHSecret(cur.GetPostgresql(), m)
			err = checkEndpoint("postgresql", m.GetHost(), m.GetPort(), m.GetUsername(), m.GetPassword(), m.GetSsl(), true)
			if err == nil {
				err = checkForwardSSH("postgresql", m, true)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if req.GetValidateOnly() || req.GetSkipValidation() {
		return nil, apierror.InvalidArgument("validate_only and skip_validation are supported only for Oracle connection profiles")
	}
	next.UpdateTime = ts(s.now())
	s.profiles[next.GetName()] = next
	location := parentOf(next.GetName())
	return s.done(location, "update", next.GetName(), req.GetRequestId(), redact(next))
}

// keepSSL carries cur's input-only certificates into next when an update
// names the same verified type without them.
func keepSSL(cur, next *dmspb.SslConfig) {
	if next == nil || cur == nil || next.GetCaCertificate() != "" || sslType(next) != sslType(cur) {
		return
	}
	next.CaCertificate, next.ClientCertificate, next.ClientKey = cur.GetCaCertificate(), cur.GetClientCertificate(), cur.GetClientKey()
}

// updateEndpoint applies one update_mask path of a source's connection;
// a destination's settings are immutable.
func updateEndpoint(next, in *dmspb.ConnectionProfile, path string) error {
	kind, field, _ := strings.Cut(path, ".")
	switch {
	case kind == "mysql" && next.GetMysql() != nil && in.GetMysql() != nil:
		m, src := next.GetMysql(), in.GetMysql()
		switch field {
		case "host":
			m.Host = src.GetHost()
		case "port":
			m.Port = src.GetPort()
		case "username":
			m.Username = src.GetUsername()
		case "password":
			m.Password = src.GetPassword()
		case "ssl", "ssl.type", "ssl.caCertificate", "ssl.ca_certificate", "ssl.clientCertificate", "ssl.client_certificate", "ssl.clientKey", "ssl.client_key":
			m.Ssl = updateSSL(m.Ssl, src.GetSsl(), field)
		case "forwardSshConnectivity", "forward_ssh_connectivity":
			copyOverlay(m, src, "forward_ssh_connectivity")
		case "":
			pw := m.Password
			*m = *clone(src)
			if m.Password == "" {
				m.Password = pw
			}
		default:
			return apierror.InvalidArgument("update_mask path %q cannot be updated", path)
		}
		return nil
	case kind == "postgresql" && next.GetPostgresql() != nil && in.GetPostgresql() != nil:
		m, src := next.GetPostgresql(), in.GetPostgresql()
		switch field {
		case "host":
			m.Host = src.GetHost()
		case "port":
			m.Port = src.GetPort()
		case "username":
			m.Username = src.GetUsername()
		case "password":
			m.Password = src.GetPassword()
		case "ssl", "ssl.type", "ssl.caCertificate", "ssl.ca_certificate", "ssl.clientCertificate", "ssl.client_certificate", "ssl.clientKey", "ssl.client_key":
			m.Ssl = updateSSL(m.Ssl, src.GetSsl(), field)
		case "forwardSshConnectivity", "forward_ssh_connectivity":
			copyOverlay(m, src, "forward_ssh_connectivity")
		case "":
			pw := m.Password
			*m = *clone(src)
			if m.Password == "" {
				m.Password = pw
			}
		default:
			return apierror.InvalidArgument("update_mask path %q cannot be updated", path)
		}
		return nil
	case kind == "cloudsql" || kind == "alloydb":
		return apierror.InvalidArgument("update_mask path %q cannot be updated: a destination's settings are immutable", path)
	}
	return apierror.InvalidArgument("update_mask path %q cannot be updated on %s", path, next.GetName())
}

// updateSSL applies an ssl update_mask path: the whole config, or one of
// its fields (gcloud's `connection-profiles update --ssl-type` sends
// mysql.ssl.type, .caCertificate, .clientCertificate and .clientKey).
func updateSSL(cur, in *dmspb.SslConfig, field string) *dmspb.SslConfig {
	if field == "ssl" {
		return clone(in)
	}
	out := clone(cur)
	if out == nil {
		out = &dmspb.SslConfig{}
	}
	switch strings.TrimPrefix(field, "ssl.") {
	case "type":
		out.Type = in.GetType()
	case "caCertificate", "ca_certificate":
		out.CaCertificate = in.GetCaCertificate()
	case "clientCertificate", "client_certificate":
		out.ClientCertificate = in.GetClientCertificate()
	case "clientKey", "client_key":
		out.ClientKey = in.GetClientKey()
	}
	return out
}

// copyOverlay copies an added message field from src to dst.
func copyOverlay(dst, src proto.Message, field string) {
	v, ok := overlay.Get(src, protoreflect.Name(field))
	if !ok {
		_ = overlay.Set(dst, protoreflect.Name(field), protoreflect.Value{})
		return
	}
	_ = overlay.Set(dst, protoreflect.Name(field), v)
}

func parentOf(name string) string {
	parts := strings.SplitN(name, "/", 5)
	if len(parts) < 4 {
		return name
	}
	return strings.Join(parts[:4], "/")
}

func (s *Server) DeleteConnectionProfile(_ context.Context, req *dmspb.DeleteConnectionProfileRequest) (*longrunningpb.Operation, error) {
	if err := checkRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.replay(req.GetRequestId()); ok {
		return op, nil
	}
	p, err := s.profileLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if p.State == dmspb.ConnectionProfile_CREATING {
		return nil, failedPrecondition("connection profile %s is being created", p.GetName())
	}
	for _, k := range sortedKeys(s.jobs, "") {
		j := s.jobs[k]
		if j.GetSource() == p.GetName() || j.GetDestination() == p.GetName() {
			return nil, failedPrecondition("connection profile %s is used by the migration job %s", p.GetName(), j.GetName())
		}
	}
	for _, o := range s.profiles {
		if o.GetCloudsql().GetSettings().GetSourceId() == p.GetName() {
			return nil, failedPrecondition("connection profile %s is the source of the Cloud SQL destination %s", p.GetName(), o.GetName())
		}
	}
	location := parentOf(p.GetName())
	if isSource(p) && sourceOf(p).SSH != nil && s.opts.Backend != nil {
		src := sourceOf(p)
		s.background(func() {
			ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
			defer cancel()
			_ = s.opts.Backend.Release(ctx, src)
		})
	}
	delete(s.versions, p.GetName())
	if t, ok := targetOf(p); ok && t.CloudSQL && !t.Existing && req.GetForce() && s.opts.Backend != nil {
		// "In case of force delete, the CloudSQL replica database is also
		// deleted (only for CloudSQL connection profile)."
		p.State = dmspb.ConnectionProfile_DELETING
		op, err := s.startOp(location, "delete", p.GetName(), req.GetRequestId())
		if err != nil {
			return nil, err
		}
		_ = s.save()
		name, opName := p.GetName(), op.Name
		s.background(func() {
			ctx, cancel := context.WithTimeout(context.Background(), destinationTimeout)
			defer cancel()
			err := s.opts.Backend.Delete(ctx, t)
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				if cur := s.profiles[name]; cur != nil {
					cur.State, cur.Error = dmspb.ConnectionProfile_FAILED, statusOf(err)
				}
				s.finishOp(opName, nil, err)
			} else {
				delete(s.profiles, name)
				delete(s.policies, name)
				s.finishOp(opName, &emptyResp, nil)
			}
			_ = s.save()
		})
		return clone(op), nil
	}
	delete(s.profiles, p.GetName())
	delete(s.policies, p.GetName())
	return s.done(location, "delete", p.GetName(), req.GetRequestId(), &emptyResp)
}

var emptyResp = emptypb.Empty{}
