package spannerfront

import (
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The admin methods the emulator refuses UNIMPLEMENTED with an empty
// message (measured: TestSpannerAdminRefusals) are refused by the front
// instead, with the same code and the reason (#1461). The emulator still
// answers the IAM methods and UpdateInstancePartition itself: it names why.
const (
	noBackups       = "the emulator keeps no backups: it cannot snapshot a database"
	noSchedules     = "the emulator takes no backups, so it keeps no backup schedules"
	noConfigs       = "the emulator has one instance configuration, emulator-config, and makes no others"
	databaseService = "/google.spanner.admin.database.v1.DatabaseAdmin/"
)

var refusedMethods = map[string]string{
	databaseService + "CreateBackup":                 noBackups,
	databaseService + "CopyBackup":                   noBackups,
	databaseService + "GetBackup":                    noBackups,
	databaseService + "UpdateBackup":                 noBackups,
	databaseService + "DeleteBackup":                 noBackups,
	databaseService + "ListBackups":                  noBackups,
	databaseService + "ListBackupOperations":         noBackups,
	databaseService + "RestoreDatabase":              noBackups,
	databaseService + "CreateBackupSchedule":         noSchedules,
	databaseService + "GetBackupSchedule":            noSchedules,
	databaseService + "UpdateBackupSchedule":         noSchedules,
	databaseService + "DeleteBackupSchedule":         noSchedules,
	databaseService + "ListBackupSchedules":          noSchedules,
	databaseService + "ListDatabaseOperations":       "the emulator does not list operations: get one by name with GetOperation",
	databaseService + "ListDatabaseRoles":            "the emulator does not list database roles",
	databaseService + "AddSplitPoints":               "the emulator does not split databases",
	databaseService + "InternalUpdateGraphOperation": "it is an internal method of Google's own services",
	instanceService + "CreateInstanceConfig":         noConfigs,
	instanceService + "UpdateInstanceConfig":         noConfigs,
	instanceService + "DeleteInstanceConfig":         noConfigs,
	instanceService + "ListInstanceConfigOperations": noConfigs,
	instanceService + "MoveInstance":                 noConfigs,
}

// refusedREST are the same methods' REST bindings.
var refusedREST = []struct {
	method string
	path   *regexp.Regexp
	name   string
}{
	{"", regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/backups(/[^/:]+)?$`), "Backups"},
	{http.MethodPost, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/backups:copy$`), "CopyBackup"},
	{http.MethodGet, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/backupOperations$`), "ListBackupOperations"},
	{http.MethodPost, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/databases:restore$`), "RestoreDatabase"},
	{"", regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/databases/[^/]+/backupSchedules(/[^/:]+)?$`), "Backup schedules"},
	{http.MethodGet, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/databaseOperations$`), "ListDatabaseOperations"},
	{http.MethodGet, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/databases/[^/]+/databaseRoles$`), "ListDatabaseRoles"},
	{http.MethodPost, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+/databases/[^/]+:addSplitPoints$`), "AddSplitPoints"},
	{http.MethodPost, regexp.MustCompile(`^/v1/projects/[^/]+/instanceConfigs$`), "CreateInstanceConfig"},
	{http.MethodPatch, regexp.MustCompile(`^/v1/projects/[^/]+/instanceConfigs/[^/:]+$`), "UpdateInstanceConfig"},
	{http.MethodDelete, regexp.MustCompile(`^/v1/projects/[^/]+/instanceConfigs/[^/:]+$`), "DeleteInstanceConfig"},
	{http.MethodGet, regexp.MustCompile(`^/v1/projects/[^/]+/instanceConfigOperations$`), "ListInstanceConfigOperations"},
	{http.MethodPost, regexp.MustCompile(`^/v1/projects/[^/]+/instances/[^/]+:move$`), "MoveInstance"},
}

var restReasons = map[string]string{
	"Backups": noBackups, "CopyBackup": noBackups, "ListBackupOperations": noBackups, "RestoreDatabase": noBackups,
	"Backup schedules":       noSchedules,
	"ListDatabaseOperations": refusedMethods[databaseService+"ListDatabaseOperations"],
	"ListDatabaseRoles":      refusedMethods[databaseService+"ListDatabaseRoles"],
	"AddSplitPoints":         refusedMethods[databaseService+"AddSplitPoints"],
	"CreateInstanceConfig":   noConfigs, "UpdateInstanceConfig": noConfigs, "DeleteInstanceConfig": noConfigs,
	"ListInstanceConfigOperations": noConfigs, "MoveInstance": noConfigs,
}

// Refusals answers the refused admin methods, over gRPC and REST.
type Refusals struct{ grpc *grpc.Server }

// NewRefusals returns the refusals.
func NewRefusals() *Refusals {
	return &Refusals{grpc: grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		m, _ := grpc.MethodFromServerStream(stream)
		return refusal(m[strings.LastIndex(m, "/")+1:], refusedMethods[m])
	}))}
}

func refusal(method, why string) error {
	return status.Errorf(codes.Unimplemented, "%s is not served here: %s", method, why)
}

func restRefusal(r *http.Request) (string, bool) {
	for _, b := range refusedREST {
		if (b.method == "" || b.method == r.Method) && b.path.MatchString(r.URL.Path) {
			return b.name, true
		}
	}
	return "", false
}

// Claims reports whether r is a refused method.
func (f *Refusals) Claims(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		_, ok := refusedMethods[r.URL.Path]
		return ok
	}
	_, ok := restRefusal(r)
	return ok
}

// ServeHTTP refuses a call Claims claimed.
func (f *Refusals) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		f.grpc.ServeHTTP(w, r)
		return
	}
	name, _ := restRefusal(r)
	writeError(w, refusal(name, restReasons[name]))
}
