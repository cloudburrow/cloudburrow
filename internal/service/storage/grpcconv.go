package storage

// The gRPC API's Bucket and Object, to and from the JSON API's resources
// (#1321).

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

func ts(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

func tsString(t *timestamppb.Timestamp) string {
	return t.AsTime().UTC().Format(time.RFC3339Nano)
}

func i64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func secs(s string) *durationpb.Duration {
	if s == "" {
		return nil
	}
	return durationpb.New(time.Duration(i64(s)) * time.Second)
}

func protoDate(s string) *date.Date {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil
	}
	return &date.Date{Year: int32(t.Year()), Month: int32(t.Month()), Day: int32(t.Day())}
}

func jsonDate(d *date.Date) string {
	return fmt.Sprintf("%04d-%02d-%02d", d.GetYear(), d.GetMonth(), d.GetDay())
}

// --- objects ----------------------------------------------------------------

// jsonObject is the JSON API's Object, as far as the gRPC API carries it.
// Field names match case-insensitively.
type jsonObject struct {
	Name, Bucket, Etag, Generation, Metageneration, StorageClass, Size  string
	ContentEncoding, ContentDisposition, CacheControl, ContentLanguage  string
	ContentType, TimeCreated, Updated, TimeDeleted, TimeFinalized       string
	TimeStorageClassUpdated, CustomTime, SoftDeleteTime, HardDeleteTime string
	RetentionExpirationTime, Crc32c, Md5Hash, KmsKeyName, RestoreToken  string
	ComponentCount                                                      int32
	TemporaryHold                                                       bool
	EventBasedHold                                                      *bool
	Metadata                                                            map[string]string
	Owner                                                               *struct{ Entity, EntityID string }
	Acl                                                                 []jsonACE
	Retention                                                           *struct{ Mode, RetainUntilTime string }
	CustomerEncryption                                                  *struct{ EncryptionAlgorithm, KeySha256 string }
}

func (o jsonObject) proto() *pb.Object {
	out := &pb.Object{
		Name: o.Name, Bucket: bucketName(o.Bucket), Etag: o.Etag,
		Generation: i64(o.Generation), Metageneration: i64(o.Metageneration),
		StorageClass: o.StorageClass, Size: i64(o.Size),
		ContentEncoding: o.ContentEncoding, ContentDisposition: o.ContentDisposition,
		CacheControl: o.CacheControl, ContentLanguage: o.ContentLanguage, ContentType: o.ContentType,
		CreateTime: ts(o.TimeCreated), UpdateTime: ts(o.Updated), DeleteTime: ts(o.TimeDeleted),
		FinalizeTime: ts(o.TimeFinalized), UpdateStorageClassTime: ts(o.TimeStorageClassUpdated),
		CustomTime: ts(o.CustomTime), SoftDeleteTime: ts(o.SoftDeleteTime), HardDeleteTime: ts(o.HardDeleteTime),
		RetentionExpireTime: ts(o.RetentionExpirationTime), KmsKey: o.KmsKeyName,
		ComponentCount: o.ComponentCount, TemporaryHold: o.TemporaryHold, EventBasedHold: o.EventBasedHold,
		Metadata: o.Metadata,
	}
	if o.RestoreToken != "" {
		out.RestoreToken = &o.RestoreToken
	}
	sums := &pb.ObjectChecksums{}
	if b, err := base64.StdEncoding.DecodeString(o.Crc32c); err == nil && len(b) == 4 {
		c := binary.BigEndian.Uint32(b)
		sums.Crc32C = &c
	}
	if b, err := base64.StdEncoding.DecodeString(o.Md5Hash); err == nil && len(b) > 0 {
		sums.Md5Hash = b
	}
	out.Checksums = sums
	if ce := o.CustomerEncryption; ce != nil {
		sum, _ := base64.StdEncoding.DecodeString(ce.KeySha256)
		out.CustomerEncryption = &pb.CustomerEncryption{EncryptionAlgorithm: ce.EncryptionAlgorithm, KeySha256Bytes: sum}
	}
	if o.Owner != nil {
		out.Owner = &pb.Owner{Entity: o.Owner.Entity, EntityId: o.Owner.EntityID}
	}
	for _, e := range o.Acl {
		out.Acl = append(out.Acl, e.objectProto())
	}
	if r := o.Retention; r != nil {
		out.Retention = &pb.Object_Retention{RetainUntilTime: ts(r.RetainUntilTime)}
		switch strings.ToLower(r.Mode) {
		case "locked":
			out.Retention.Mode = pb.Object_Retention_LOCKED
		case "unlocked":
			out.Retention.Mode = pb.Object_Retention_UNLOCKED
		}
	}
	return out
}

// objectFields maps an Object's update_mask paths to its JSON fields.
var objectFields = map[string]string{
	"content_type": "contentType", "content_encoding": "contentEncoding", "content_disposition": "contentDisposition",
	"cache_control": "cacheControl", "content_language": "contentLanguage", "metadata": "metadata",
	"custom_time": "customTime", "temporary_hold": "temporaryHold", "event_based_hold": "eventBasedHold",
	"retention": "retention", "acl": "acl",
}

// objectToJSON is o as JSON API metadata: every field it sets, or, with
// paths (an update_mask), those fields, null where o leaves one unset.
func objectToJSON(o *pb.Object, paths []string) (map[string]any, error) {
	all := map[string]any{}
	set := func(k string, v any, ok bool) {
		if ok {
			all[k] = v
		}
	}
	set("name", o.GetName(), o.GetName() != "" && paths == nil)
	set("contentType", o.GetContentType(), o.GetContentType() != "")
	set("contentEncoding", o.GetContentEncoding(), o.GetContentEncoding() != "")
	set("contentDisposition", o.GetContentDisposition(), o.GetContentDisposition() != "")
	set("cacheControl", o.GetCacheControl(), o.GetCacheControl() != "")
	set("contentLanguage", o.GetContentLanguage(), o.GetContentLanguage() != "")
	set("metadata", o.GetMetadata(), len(o.GetMetadata()) > 0)
	set("storageClass", o.GetStorageClass(), o.GetStorageClass() != "" && paths == nil)
	set("customTime", tsString(o.GetCustomTime()), o.GetCustomTime() != nil)
	set("temporaryHold", o.GetTemporaryHold(), o.GetTemporaryHold() || paths != nil)
	set("eventBasedHold", o.GetEventBasedHold(), o.EventBasedHold != nil)
	if r := o.GetRetention(); r != nil {
		m := map[string]any{}
		switch r.GetMode() {
		case pb.Object_Retention_LOCKED:
			m["mode"] = "Locked"
		case pb.Object_Retention_UNLOCKED:
			m["mode"] = "Unlocked"
		}
		if r.GetRetainUntilTime() != nil {
			m["retainUntilTime"] = tsString(r.GetRetainUntilTime())
		}
		all["retention"] = m
	}
	if c := o.GetChecksums(); c != nil && paths == nil {
		addChecksums(all, c)
	}
	if acl := o.GetAcl(); len(acl) > 0 || paths != nil {
		all["acl"] = aclToJSON(acl)
	}
	if o.GetContexts() != nil {
		return nil, grpcstatus.Error(codes.Unimplemented, "object contexts are not implemented: the JSON API does not serve them")
	}
	if paths == nil {
		return all, nil
	}
	out := map[string]any{}
	for _, p := range paths {
		if p == "*" {
			for _, k := range objectFields {
				out[k] = all[k]
			}
			continue
		}
		if key, ok := strings.CutPrefix(p, "metadata."); ok {
			mapEntry(out, "metadata", key, o.GetMetadata())
			continue
		}
		k, ok := objectFields[p]
		if !ok {
			return nil, grpcstatus.Errorf(codes.Unimplemented, "updating %q is not implemented over gRPC", p)
		}
		out[k] = all[k]
	}
	return out, nil
}

// --- buckets ----------------------------------------------------------------

type jsonRule struct {
	Action struct {
		Type, StorageClass string
	}
	Condition struct {
		Age, NumNewerVersions, DaysSinceCustomTime, DaysSinceNoncurrentTime *int32
		CreatedBefore, CustomTimeBefore, NoncurrentTimeBefore               string
		IsLive                                                              *bool
		MatchesStorageClass, MatchesPrefix, MatchesSuffix                   []string
	}
}

// jsonBucket is the JSON API's Bucket, as far as the gRPC API carries it.
type jsonBucket struct {
	Name, Etag, ProjectNumber, Metageneration, Location, LocationType string
	StorageClass, Rpo, TimeCreated, Updated                           string
	DefaultEventBasedHold                                             bool
	Labels                                                            map[string]string
	Versioning                                                        *struct{ Enabled bool }
	RetentionPolicy                                                   *struct {
		RetentionPeriod, EffectiveTime string
		IsLocked                       bool
	}
	SoftDeletePolicy *struct{ RetentionDurationSeconds, EffectiveTime string }
	IamConfiguration *struct {
		UniformBucketLevelAccess *struct {
			Enabled    bool
			LockedTime string
		}
		PublicAccessPrevention string
	}
	Cors []struct {
		Origin, Method, ResponseHeader []string
		MaxAgeSeconds                  int32
	}
	Website               *struct{ MainPageSuffix, NotFoundPage string }
	Logging               *struct{ LogBucket, LogObjectPrefix string }
	Billing               *struct{ RequesterPays bool }
	Encryption            *struct{ DefaultKmsKeyName string }
	Autoclass             *struct{ Enabled bool }
	HierarchicalNamespace *struct{ Enabled bool }
	ObjectRetention       *struct{ Mode string }
	Lifecycle             *struct{ Rule []jsonRule }
	Owner                 *struct{ Entity, EntityID string }
	Acl, DefaultObjectAcl []jsonACE
}

func (b jsonBucket) proto() *pb.Bucket {
	out := &pb.Bucket{
		Name: bucketName(b.Name), BucketId: b.Name, Etag: b.Etag, Metageneration: i64(b.Metageneration),
		Location: b.Location, LocationType: b.LocationType, StorageClass: b.StorageClass, Rpo: b.Rpo,
		CreateTime: ts(b.TimeCreated), UpdateTime: ts(b.Updated), DefaultEventBasedHold: b.DefaultEventBasedHold,
		Labels: b.Labels,
	}
	if b.ProjectNumber != "" {
		out.Project = "projects/" + b.ProjectNumber
	}
	if v := b.Versioning; v != nil {
		out.Versioning = &pb.Bucket_Versioning{Enabled: v.Enabled}
	}
	if r := b.RetentionPolicy; r != nil {
		out.RetentionPolicy = &pb.Bucket_RetentionPolicy{RetentionDuration: secs(r.RetentionPeriod), EffectiveTime: ts(r.EffectiveTime), IsLocked: r.IsLocked}
	}
	if s := b.SoftDeletePolicy; s != nil {
		out.SoftDeletePolicy = &pb.Bucket_SoftDeletePolicy{RetentionDuration: secs(s.RetentionDurationSeconds), EffectiveTime: ts(s.EffectiveTime)}
	}
	if c := b.IamConfiguration; c != nil {
		out.IamConfig = &pb.Bucket_IamConfig{PublicAccessPrevention: c.PublicAccessPrevention}
		if u := c.UniformBucketLevelAccess; u != nil {
			out.IamConfig.UniformBucketLevelAccess = &pb.Bucket_IamConfig_UniformBucketLevelAccess{Enabled: u.Enabled, LockTime: ts(u.LockedTime)}
		}
	}
	for _, c := range b.Cors {
		out.Cors = append(out.Cors, &pb.Bucket_Cors{Origin: c.Origin, Method: c.Method, ResponseHeader: c.ResponseHeader, MaxAgeSeconds: c.MaxAgeSeconds})
	}
	if w := b.Website; w != nil {
		out.Website = &pb.Bucket_Website{MainPageSuffix: w.MainPageSuffix, NotFoundPage: w.NotFoundPage}
	}
	if l := b.Logging; l != nil {
		out.Logging = &pb.Bucket_Logging{LogBucket: bucketName(l.LogBucket), LogObjectPrefix: l.LogObjectPrefix}
		if l.LogBucket == "" {
			out.Logging.LogBucket = ""
		}
	}
	if x := b.Billing; x != nil {
		out.Billing = &pb.Bucket_Billing{RequesterPays: x.RequesterPays}
	}
	if e := b.Encryption; e != nil {
		out.Encryption = &pb.Bucket_Encryption{DefaultKmsKey: e.DefaultKmsKeyName}
	}
	if a := b.Autoclass; a != nil {
		out.Autoclass = &pb.Bucket_Autoclass{Enabled: a.Enabled}
	}
	if h := b.HierarchicalNamespace; h != nil {
		out.HierarchicalNamespace = &pb.Bucket_HierarchicalNamespace{Enabled: h.Enabled}
	}
	if r := b.ObjectRetention; r != nil {
		out.ObjectRetention = &pb.Bucket_ObjectRetention{Enabled: r.Mode == "Enabled"}
	}
	if o := b.Owner; o != nil {
		out.Owner = &pb.Owner{Entity: o.Entity, EntityId: o.EntityID}
	}
	for _, e := range b.Acl {
		out.Acl = append(out.Acl, e.bucketProto())
	}
	for _, e := range b.DefaultObjectAcl {
		out.DefaultObjectAcl = append(out.DefaultObjectAcl, e.objectProto())
	}
	if l := b.Lifecycle; l != nil {
		out.Lifecycle = &pb.Bucket_Lifecycle{}
		for _, r := range l.Rule {
			c := r.Condition
			pc := &pb.Bucket_Lifecycle_Rule_Condition{AgeDays: c.Age, NumNewerVersions: c.NumNewerVersions,
				DaysSinceCustomTime: c.DaysSinceCustomTime, DaysSinceNoncurrentTime: c.DaysSinceNoncurrentTime, IsLive: c.IsLive,
				MatchesStorageClass: c.MatchesStorageClass, MatchesPrefix: c.MatchesPrefix, MatchesSuffix: c.MatchesSuffix}
			if c.CreatedBefore != "" {
				pc.CreatedBefore = protoDate(c.CreatedBefore)
			}
			if c.CustomTimeBefore != "" {
				pc.CustomTimeBefore = protoDate(c.CustomTimeBefore)
			}
			if c.NoncurrentTimeBefore != "" {
				pc.NoncurrentTimeBefore = protoDate(c.NoncurrentTimeBefore)
			}
			out.Lifecycle.Rule = append(out.Lifecycle.Rule, &pb.Bucket_Lifecycle_Rule{
				Action: &pb.Bucket_Lifecycle_Rule_Action{Type: r.Action.Type, StorageClass: r.Action.StorageClass}, Condition: pc})
		}
	}
	return out
}

// bucketMaskFields maps a Bucket's update_mask paths to its JSON fields.
var bucketMaskFields = map[string]string{
	"labels": "labels", "versioning": "versioning", "storage_class": "storageClass",
	"default_event_based_hold": "defaultEventBasedHold", "retention_policy": "retentionPolicy",
	"soft_delete_policy": "softDeletePolicy", "iam_config": "iamConfiguration", "cors": "cors",
	"website": "website", "logging": "logging", "billing": "billing", "encryption": "encryption",
	"autoclass": "autoclass", "lifecycle": "lifecycle", "rpo": "rpo",
	"acl": "acl", "default_object_acl": "defaultObjectAcl",
}

// bucketToJSON is b as JSON API metadata: every field it sets, or, with
// paths (an update_mask), those fields, null where b leaves one unset.
func bucketToJSON(b *pb.Bucket, paths []string) (map[string]any, error) {
	if b.GetCustomPlacementConfig() != nil || b.GetIpFilter() != nil {
		return nil, grpcstatus.Error(codes.Unimplemented, "custom_placement_config and ip_filter are not implemented over gRPC")
	}
	all := map[string]any{}
	set := func(k string, v any, ok bool) {
		if ok {
			all[k] = v
		}
	}
	set("location", b.GetLocation(), b.GetLocation() != "" && paths == nil)
	set("storageClass", b.GetStorageClass(), b.GetStorageClass() != "")
	set("rpo", b.GetRpo(), b.GetRpo() != "")
	set("labels", b.GetLabels(), len(b.GetLabels()) > 0)
	set("defaultEventBasedHold", b.GetDefaultEventBasedHold(), b.GetDefaultEventBasedHold() || paths != nil)
	if acl := b.GetAcl(); len(acl) > 0 || paths != nil {
		all["acl"] = bucketACLToJSON(acl)
	}
	if acl := b.GetDefaultObjectAcl(); len(acl) > 0 || paths != nil {
		all["defaultObjectAcl"] = aclToJSON(acl)
	}
	if v := b.GetVersioning(); v != nil {
		all["versioning"] = map[string]any{"enabled": v.GetEnabled()}
	}
	if r := b.GetRetentionPolicy(); r != nil {
		all["retentionPolicy"] = map[string]any{"retentionPeriod": strconv.FormatInt(int64(r.GetRetentionDuration().AsDuration()/time.Second), 10)}
	}
	if s := b.GetSoftDeletePolicy(); s != nil {
		all["softDeletePolicy"] = map[string]any{"retentionDurationSeconds": strconv.FormatInt(int64(s.GetRetentionDuration().AsDuration()/time.Second), 10)}
	}
	if c := b.GetIamConfig(); c != nil {
		m := map[string]any{}
		if u := c.GetUniformBucketLevelAccess(); u != nil {
			m["uniformBucketLevelAccess"] = map[string]any{"enabled": u.GetEnabled()}
		}
		if c.GetPublicAccessPrevention() != "" {
			m["publicAccessPrevention"] = c.GetPublicAccessPrevention()
		}
		all["iamConfiguration"] = m
	}
	if len(b.GetCors()) > 0 {
		var cs []map[string]any
		for _, c := range b.GetCors() {
			m := map[string]any{"origin": c.GetOrigin(), "method": c.GetMethod(), "responseHeader": c.GetResponseHeader()}
			if c.GetMaxAgeSeconds() != 0 {
				m["maxAgeSeconds"] = c.GetMaxAgeSeconds()
			}
			cs = append(cs, m)
		}
		all["cors"] = cs
	}
	if w := b.GetWebsite(); w != nil {
		all["website"] = map[string]any{"mainPageSuffix": w.GetMainPageSuffix(), "notFoundPage": w.GetNotFoundPage()}
	}
	if l := b.GetLogging(); l != nil {
		lb := l.GetLogBucket()
		if id, err := bucketID(lb); err == nil {
			lb = id
		}
		all["logging"] = map[string]any{"logBucket": lb, "logObjectPrefix": l.GetLogObjectPrefix()}
	}
	if x := b.GetBilling(); x != nil {
		all["billing"] = map[string]any{"requesterPays": x.GetRequesterPays()}
	}
	if e := b.GetEncryption(); e != nil {
		if e.GetGoogleManagedEncryptionEnforcementConfig() != nil || e.GetCustomerManagedEncryptionEnforcementConfig() != nil || e.GetCustomerSuppliedEncryptionEnforcementConfig() != nil {
			return nil, grpcstatus.Error(codes.Unimplemented, "encryption enforcement configs are not implemented over gRPC")
		}
		all["encryption"] = map[string]any{"defaultKmsKeyName": e.GetDefaultKmsKey()}
	}
	if a := b.GetAutoclass(); a != nil {
		all["autoclass"] = map[string]any{"enabled": a.GetEnabled()}
	}
	if h := b.GetHierarchicalNamespace(); h != nil && paths == nil {
		all["hierarchicalNamespace"] = map[string]any{"enabled": h.GetEnabled()}
	}
	if l := b.GetLifecycle(); l != nil {
		rules := []map[string]any{}
		for _, r := range l.GetRule() {
			act := map[string]any{"type": r.GetAction().GetType()}
			if r.GetAction().GetStorageClass() != "" {
				act["storageClass"] = r.GetAction().GetStorageClass()
			}
			c := r.GetCondition()
			cond := map[string]any{}
			for k, v := range map[string]*int32{"age": c.AgeDays, "numNewerVersions": c.NumNewerVersions,
				"daysSinceCustomTime": c.DaysSinceCustomTime, "daysSinceNoncurrentTime": c.DaysSinceNoncurrentTime} {
				if v != nil {
					cond[k] = *v
				}
			}
			if c.IsLive != nil {
				cond["isLive"] = *c.IsLive
			}
			for k, v := range map[string]*date.Date{"createdBefore": c.GetCreatedBefore(), "customTimeBefore": c.GetCustomTimeBefore(),
				"noncurrentTimeBefore": c.GetNoncurrentTimeBefore()} {
				if v != nil {
					cond[k] = jsonDate(v)
				}
			}
			for k, v := range map[string][]string{"matchesStorageClass": c.GetMatchesStorageClass(), "matchesPrefix": c.GetMatchesPrefix(),
				"matchesSuffix": c.GetMatchesSuffix()} {
				if len(v) > 0 {
					cond[k] = v
				}
			}
			rules = append(rules, map[string]any{"action": act, "condition": cond})
		}
		all["lifecycle"] = map[string]any{"rule": rules}
	}
	if paths == nil {
		return all, nil
	}
	out := map[string]any{}
	for _, p := range paths {
		if p == "*" {
			for _, k := range bucketMaskFields {
				out[k] = all[k]
			}
			continue
		}
		if key, ok := strings.CutPrefix(p, "labels."); ok {
			mapEntry(out, "labels", key, b.GetLabels())
			continue
		}
		k, ok := bucketMaskFields[p]
		if !ok {
			return nil, grpcstatus.Errorf(codes.Unimplemented, "updating %q is not implemented over gRPC", p)
		}
		out[k] = all[k]
	}
	return out, nil
}

// mapEntry patches one key of a map field (an update_mask path
// metadata.<key> or labels.<key>): its value in from, or null, which the JSON
// API's patch reads as a deletion.
func mapEntry(out map[string]any, field, key string, from map[string]string) {
	m, _ := out[field].(map[string]any)
	if m == nil {
		m = map[string]any{}
		out[field] = m
	}
	if v, ok := from[key]; ok {
		m[key] = v
	} else {
		m[key] = nil
	}
}

// --- access control lists (#1233) ----------------------------------------

// jsonACE is a JSON API BucketAccessControl or ObjectAccessControl.
type jsonACE struct {
	Entity, Role, ID, EntityID, Etag, Email, Domain string
	ProjectTeam                                     *struct{ ProjectNumber, Team string }
}

func (e jsonACE) bucketProto() *pb.BucketAccessControl {
	out := &pb.BucketAccessControl{Role: e.Role, Id: e.ID, Entity: e.Entity, EntityId: e.EntityID, Etag: e.Etag, Email: e.Email, Domain: e.Domain}
	if t := e.ProjectTeam; t != nil {
		out.ProjectTeam = &pb.ProjectTeam{ProjectNumber: t.ProjectNumber, Team: t.Team}
	}
	return out
}

func (e jsonACE) objectProto() *pb.ObjectAccessControl {
	out := &pb.ObjectAccessControl{Role: e.Role, Id: e.ID, Entity: e.Entity, EntityId: e.EntityID, Etag: e.Etag, Email: e.Email, Domain: e.Domain}
	if t := e.ProjectTeam; t != nil {
		out.ProjectTeam = &pb.ProjectTeam{ProjectNumber: t.ProjectNumber, Team: t.Team}
	}
	return out
}

// aclToJSON is an object ACL (or a default object ACL) as the JSON API
// takes it: entity and role, which is all an entry sets. An empty list is
// [], which sets an empty ACL.
func aclToJSON(acl []*pb.ObjectAccessControl) []any {
	out := []any{}
	for _, e := range acl {
		out = append(out, map[string]any{"entity": e.GetEntity(), "role": e.GetRole()})
	}
	return out
}

func bucketACLToJSON(acl []*pb.BucketAccessControl) []any {
	out := []any{}
	for _, e := range acl {
		out = append(out, map[string]any{"entity": e.GetEntity(), "role": e.GetRole()})
	}
	return out
}
