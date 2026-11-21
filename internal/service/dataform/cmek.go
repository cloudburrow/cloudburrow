package dataform

import (
	"context"
	"regexp"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// Customer-managed encryption keys (#2105), as Google documents them
// (https://docs.cloud.google.com/dataform/docs/cmek, read 2026-11-21):
//
//   - a repository's kms_key_name "will be used to encrypt user data in the
//     repository and all child resources. It is not possible to add or
//     update the encryption key after the repository is created"
//     (projects.locations.repositories); Config's default_kms_key_name "is
//     used if no encryption key is provided when a repository is created";
//   - "Repositories must use matching regional keys": the key's location is
//     the repository's;
//   - the Dataform service agent needs roles/cloudkms.cryptoKeyEncrypterDecrypter
//     on the key, checked when IAM is enforced;
//   - data_encryption_state.kms_key_version_name is "The KMS key version
//     name with which data of a resource is encrypted".
//
// Here the key is a CryptoKey of the instance's Cloud KMS. Creating the
// repository wraps a data key with it (cmek.Seal), and every call that
// reads or writes the repository's data (its files, workspaces,
// compilation results, release and workflow configs and invocations)
// first has Cloud KMS unwrap it: while the key version is disabled or
// destroyed, or the agent lacks the role, the call is refused as
// cmek.Error words it. The repository's record (Get, List, Update,
// Delete) and its IAM policy stay readable, as Google's do.
//
// UNVERIFIED (docs re-read 2026-11-21, the page above): the files on disk
// (git objects and working copies) are not themselves encrypted with the
// data key; Google documents no observable difference.

// agentDomain is Dataform's service agent domain (service-PROJECT_NUMBER@gcp-sa-dataform...).
const agentDomain = "gcp-sa-dataform.iam.gserviceaccount.com"

var repoPrefixRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/repositories/([^/]+)`)

func keyError(what string, err error) error {
	if e := cmek.As(err); e != nil {
		return &apierror.Error{Code: e.Code(), Message: what + ": " + e.Error()}
	}
	return apierror.Wrap(err)
}

// repositoryKey is the key a new repository in project/location is
// encrypted with: its own kms_key_name, else the location's Config
// default, else none.
func (s *Server) repositoryKey(r *dfpb.Repository, project, location string) string {
	if k := r.GetKmsKeyName(); k != "" {
		return k
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configs["projects/"+project+"/locations/"+location+"/config"].GetDefaultKmsKeyName()
}

// sealRepository checks key for a repository named name and wraps its
// data key.
func (s *Server) sealRepository(ctx context.Context, name, key string) (cmek.Sealed, error) {
	m := repoPrefixRE.FindStringSubmatch(name)
	if err := cmek.CheckName("kms_key_name", key, m[2]); err != nil {
		return cmek.Sealed{}, keyError("kms_key_name", err)
	}
	agent := cmek.ServiceAgent(m[1], agentDomain)
	if err := s.opts.Keys.Usable(ctx, m[1], agent, key); err != nil {
		return cmek.Sealed{}, keyError("the repository's Cloud KMS key cannot be used", err)
	}
	sealed, err := s.opts.Keys.Seal(ctx, m[1], agent, key, nil, []byte(name))
	if err != nil {
		return cmek.Sealed{}, keyError("the repository's Cloud KMS key cannot be used", err)
	}
	return sealed, nil
}

// checkKey has Cloud KMS unwrap the data key of the repository a resource
// name is in; nil for a repository without a key.
func (s *Server) checkKey(ctx context.Context, resource string) error {
	m := repoPrefixRE.FindStringSubmatch(resource)
	if m == nil {
		return nil
	}
	s.mu.Lock()
	sealed, ok := s.sealed[m[0]]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if _, err := s.opts.Keys.Open(ctx, m[1], cmek.ServiceAgent(m[1], agentDomain), sealed, []byte(m[0])); err != nil {
		return keyError("the repository's Cloud KMS key cannot be used", err)
	}
	return nil
}

// keyFree are the methods a repository's key does not gate: its record,
// its policy, and the calls that name no repository's data.
var keyFree = map[string]bool{
	"CreateRepository": true, "GetRepository": true, "ListRepositories": true, "UpdateRepository": true,
	"DeleteRepository": true, "MoveRepository": true, "GetIamPolicy": true, "SetIamPolicy": true, "TestIamPermissions": true,
}

// requestResource is the first string field of req naming a repository
// or something in one.
func requestResource(req any) string {
	m, ok := req.(proto.Message)
	if !ok {
		return ""
	}
	found := ""
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.StringKind && !fd.IsList() && !fd.IsMap() && repoPrefixRE.MatchString(v.String()) {
			found = v.String()
			return false
		}
		return true
	})
	return found
}

// keyedDesc is the Dataform service with each method's handler gated by
// checkKey, for gRPC and the JSON transcoder alike.
func (s *Server) keyedDesc() *grpc.ServiceDesc {
	desc := dfpb.Dataform_ServiceDesc
	desc.Methods = append([]grpc.MethodDesc(nil), dfpb.Dataform_ServiceDesc.Methods...)
	for i := range desc.Methods {
		md := &desc.Methods[i]
		if keyFree[md.MethodName] {
			continue
		}
		h := md.Handler
		md.Handler = func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
			return h(srv, ctx, dec, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				if r := requestResource(req); r != "" {
					if err := s.checkKey(ctx, r); err != nil {
						return nil, err
					}
				}
				if ic != nil {
					return ic(ctx, req, info, handler)
				}
				return handler(ctx, req)
			})
		}
	}
	return &desc
}
