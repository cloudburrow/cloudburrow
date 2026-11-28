package datamigration

import (
	"encoding/json"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The fields Google's v1 reference (and gcloud database-migration, whose
// generated client is built from its discovery document) has and the
// newest published clouddmspb (v1.16.1) lacks: they are decoded from and
// written to JSON through overlay, and read here with str and boolOf.
// gcloud sends role on every profile, databaseVersionName on a cloudsql
// profile, and postgresHomogeneousConfig for --use-postgres-native.
//
// SslConfig.SslType's REQUIRED and NONE are added too, with Google's
// numbers (google/cloud/clouddms/v1/clouddms_resources.proto: REQUIRED = 3,
// NONE = 4).
var overlay = rest.MustOverlay("google.cloud.clouddms.v1", addFields)

func overlayField(name, json string, num int32, typ descriptorpb.FieldDescriptorProto_Type, msg string) *descriptorpb.FieldDescriptorProto {
	f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), JsonName: proto.String(json), Number: proto.Int32(num),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: typ.Enum()}
	if msg != "" {
		f.TypeName = proto.String(msg)
	}
	return f
}

func repeated(f *descriptorpb.FieldDescriptorProto) *descriptorpb.FieldDescriptorProto {
	f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	return f
}

func enumField(name, json string, num int32, typ string) *descriptorpb.FieldDescriptorProto {
	f := overlayField(name, json, num, descriptorpb.FieldDescriptorProto_TYPE_ENUM, "")
	f.TypeName = proto.String(typ)
	return f
}

func enumOf(name string, values ...string) *descriptorpb.EnumDescriptorProto {
	e := &descriptorpb.EnumDescriptorProto{Name: proto.String(name)}
	for i, v := range values {
		e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(v), Number: proto.Int32(int32(i))})
	}
	return e
}

func addFields(f *descriptorpb.FileDescriptorProto) {
	const (
		tMsg  = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		tStr  = descriptorpb.FieldDescriptorProto_TYPE_STRING
		tBool = descriptorpb.FieldDescriptorProto_TYPE_BOOL
		tI32  = descriptorpb.FieldDescriptorProto_TYPE_INT32
		pkg   = ".google.cloud.clouddms.v1."
	)
	byName := map[string]*descriptorpb.DescriptorProto{}
	for _, m := range f.GetMessageType() {
		byName[m.GetName()] = m
	}
	add := func(msg string, fields ...*descriptorpb.FieldDescriptorProto) {
		m := byName[msg]
		if m == nil {
			return
		}
		have := map[string]bool{}
		for _, fd := range m.GetField() {
			have[fd.GetName()] = true
		}
		for _, fd := range fields {
			if !have[fd.GetName()] {
				m.Field = append(m.Field, fd)
			}
		}
	}
	if _, ok := byName["MigrationJob"]; ok {
		f.MessageType = append(f.MessageType,
			&descriptorpb.DescriptorProto{Name: proto.String("PostgresHomogeneousConfig"), Field: []*descriptorpb.FieldDescriptorProto{
				overlayField("is_native_logical", "isNativeLogical", 1, tBool, ""),
				overlayField("max_additional_subscriptions", "maxAdditionalSubscriptions", 2, tI32, ""),
			}},
			&descriptorpb.DescriptorProto{Name: proto.String("MySqlHomogeneousConfig"), Field: []*descriptorpb.FieldDescriptorProto{
				overlayField("is_primary_destination", "isPrimaryDestination", 1, tBool, ""),
			}},
			// The objects a job migrates (#2257): objectsConfig's
			// sourceObjectsConfig, as gcloud's datamigration_v1 messages
			// and the REST reference have them.
			&descriptorpb.DescriptorProto{Name: proto.String("SourceObjectIdentifier"), Field: []*descriptorpb.FieldDescriptorProto{
				overlayField("database", "database", 1, tStr, ""),
				overlayField("schema", "schema", 2, tStr, ""),
				overlayField("table", "table", 3, tStr, ""),
				enumField("type", "type", 4, pkg+"SourceObjectIdentifier.MigrationJobObjectType"),
			}, EnumType: []*descriptorpb.EnumDescriptorProto{enumOf("MigrationJobObjectType",
				"MIGRATION_JOB_OBJECT_TYPE_UNSPECIFIED", "DATABASE", "SCHEMA", "TABLE")}},
			&descriptorpb.DescriptorProto{Name: proto.String("SourceObjectConfig"), Field: []*descriptorpb.FieldDescriptorProto{
				overlayField("object_identifier", "objectIdentifier", 1, tMsg, pkg+"SourceObjectIdentifier"),
			}},
			&descriptorpb.DescriptorProto{Name: proto.String("SourceObjectsConfig"), Field: []*descriptorpb.FieldDescriptorProto{
				repeated(overlayField("excluded_object_configs", "excludedObjectConfigs", 1, tMsg, pkg+"SourceObjectConfig")),
				repeated(overlayField("object_configs", "objectConfigs", 2, tMsg, pkg+"SourceObjectConfig")),
				enumField("objects_selection_type", "objectsSelectionType", 3, pkg+"SourceObjectsConfig.ObjectsSelectionType"),
			}, EnumType: []*descriptorpb.EnumDescriptorProto{enumOf("ObjectsSelectionType",
				"OBJECTS_SELECTION_TYPE_UNSPECIFIED", "ALL_OBJECTS", "SPECIFIED_OBJECTS")}},
			&descriptorpb.DescriptorProto{Name: proto.String("MigrationJobObjectsConfig"), Field: []*descriptorpb.FieldDescriptorProto{
				overlayField("source_objects_config", "sourceObjectsConfig", 1, tMsg, pkg+"SourceObjectsConfig"),
			}})
	}
	add("ConnectionProfile", overlayField("role", "role", 1001, tStr, ""))
	// Request fields Google's v1 has and clouddmspb v1.16.1 lacks, which
	// gcloud sends (its generated datamigration_v1 messages): resume's
	// skipValidation, promote's failback, restart's restartFailedObjects.
	add("ResumeMigrationJobRequest", overlayField("skip_validation", "skipValidation", 1001, tBool, ""))
	add("PromoteMigrationJobRequest", overlayField("failback", "failback", 1001, tBool, ""),
		overlayField("objects_filter", "objectsFilter", 1002, tMsg, pkg+"MigrationJobObjectsConfig"))
	add("RestartMigrationJobRequest", overlayField("restart_failed_objects", "restartFailedObjects", 1001, tBool, ""),
		overlayField("objects_filter", "objectsFilter", 1002, tMsg, pkg+"MigrationJobObjectsConfig"))
	add("PostgreSqlConnectionProfile",
		overlayField("database", "database", 1001, tStr, ""),
		overlayField("alloydb_cluster_id", "alloydbClusterId", 1002, tStr, ""),
		overlayField("enable_iam_authentication", "enableIamAuthentication", 1003, tBool, ""),
		overlayField("private_connectivity", "privateConnectivity", 1004, tMsg, pkg+"PrivateConnectivity"),
		overlayField("forward_ssh_connectivity", "forwardSshConnectivity", 1005, tMsg, pkg+"ForwardSshTunnelConnectivity"))
	add("MySqlConnectionProfile",
		overlayField("private_connectivity", "privateConnectivity", 1001, tMsg, pkg+"PrivateConnectivity"),
		overlayField("private_service_connect_connectivity", "privateServiceConnectConnectivity", 1002, tMsg, pkg+"PrivateServiceConnectConnectivity"),
		overlayField("static_service_ip_connectivity", "staticServiceIpConnectivity", 1003, tMsg, pkg+"StaticServiceIpConnectivity"),
		overlayField("forward_ssh_connectivity", "forwardSshConnectivity", 1004, tMsg, pkg+"ForwardSshTunnelConnectivity"))
	add("CloudSqlSettings", overlayField("database_version_name", "databaseVersionName", 1001, tStr, ""))
	add("AlloyDbSettings", overlayField("database_version", "databaseVersion", 1001, tStr, ""))
	add("MigrationJob",
		overlayField("postgres_homogeneous_config", "postgresHomogeneousConfig", 1001, tMsg, pkg+"PostgresHomogeneousConfig"),
		overlayField("mysql_homogeneous_config", "mysqlHomogeneousConfig", 1002, tMsg, pkg+"MySqlHomogeneousConfig"),
		overlayField("dump_type", "dumpType", 1003, tStr, ""),
		overlayField("purpose", "purpose", 1004, tStr, ""),
		overlayField("objects_config", "objectsConfig", 1005, tMsg, pkg+"MigrationJobObjectsConfig"))
	if ssl := byName["SslConfig"]; ssl != nil {
		for _, e := range ssl.GetEnumType() {
			if e.GetName() != "SslType" {
				continue
			}
			have := map[string]bool{}
			for _, v := range e.GetValue() {
				have[v.GetName()] = true
			}
			for _, v := range []struct {
				name string
				num  int32
			}{{"REQUIRED", 3}, {"NONE", 4}} {
				if !have[v.name] {
					e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(v.name), Number: proto.Int32(v.num)})
				}
			}
		}
	}
}

// Overlay is the JSON API's overlay, for the console's pages.
func Overlay() *rest.Overlay { return overlay }

// str is an added string field of m, "" when unset.
func str(m proto.Message, field protoreflect.Name) string {
	if v, ok := overlay.Get(m, field); ok {
		return v.String()
	}
	return ""
}

// setStr sets an added string field of m.
func setStr(m proto.Message, field protoreflect.Name, v string) {
	val := protoreflect.Value{}
	if v != "" {
		val = protoreflect.ValueOfString(v)
	}
	_ = overlay.Set(m, field, val)
}

// has reports whether m sets an added field.
func has(m proto.Message, field protoreflect.Name) bool {
	_, ok := overlay.Get(m, field)
	return ok
}

// nestedBool is an added message field's bool field: postgresHomogeneous
// Config.isNativeLogical.
func nestedBool(m proto.Message, field, sub protoreflect.Name) bool {
	v, ok := overlay.Get(m, field)
	if !ok {
		return false
	}
	msg := v.Message()
	fd := msg.Descriptor().Fields().ByName(sub)
	return fd != nil && msg.Get(fd).Bool()
}

// nestedStr is an added message field's string field.
func nestedStr(m proto.Message, field, sub protoreflect.Name) string {
	v, ok := overlay.Get(m, field)
	if !ok {
		return ""
	}
	msg := v.Message()
	fd := msg.Descriptor().Fields().ByName(sub)
	if fd == nil {
		return ""
	}
	return msg.Get(fd).String()
}

// boolField is an added bool field of m.
func boolField(m proto.Message, field protoreflect.Name) bool {
	v, ok := overlay.Get(m, field)
	return ok && v.Bool()
}

// SSLRequired and SSLNone are SslConfig.SslType's REQUIRED and NONE, which
// clouddmspb v1.16.1 does not name.
const (
	SSLRequired = 3
	SSLNone     = 4
)

// CloudSQLVersionName is a Cloud SQL destination's database version:
// databaseVersionName, or the enum's name.
func CloudSQLVersionName(s *dmspb.CloudSqlSettings) string { return cloudSQLVersion(s) }

// SetCloudSQLVersionName sets databaseVersionName, which clouddmspb
// v1.16.1 does not have.
func SetCloudSQLVersionName(s *dmspb.CloudSqlSettings, v string) {
	setStr(s, "database_version_name", v)
}

// SetAlloyDBVersion sets an AlloyDB destination's databaseVersion.
func SetAlloyDBVersion(s *dmspb.AlloyDbSettings, v string) { setStr(s, "database_version", v) }

// NativeLogical reports whether a job migrates with PostgreSQL's native
// logical replication (postgresHomogeneousConfig.isNativeLogical).
func NativeLogical(j *dmspb.MigrationJob) bool { return nativeLogical(j) }

// SetNativeLogical sets postgresHomogeneousConfig.isNativeLogical.
func SetNativeLogical(j *dmspb.MigrationJob, on bool) error {
	b, err := overlay.Marshal(j)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	m["postgresHomogeneousConfig"] = map[string]any{"isNativeLogical": on}
	if b, err = json.Marshal(m); err != nil {
		return err
	}
	proto.Reset(j)
	return overlay.Unmarshal(b, j)
}
