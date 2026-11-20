package run

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// SourceUpload (#1958): google.cloud.run.v2.SourceUpload.UploadSource, the
// Cloud Run API's upload of a source archive that `gcloud beta run deploy
// --source` makes with --run-upload (and by default with --no-build). The
// archive is a media upload, as gcloud's generated client sends it: a POST
// to /upload/v2/{parent}:uploadSource with uploadType=multipart (the
// request's JSON, then the archive) or uploadType=media (the archive
// alone). It is written to the instance's Cloud Storage, in the regional
// bucket gcloud's own upload uses, run-sources-{project}-{region}, at
// {kind}s/{name}/{timestamp}-{uuid}{.zip|.tar.gz} as gcloud names its
// objects, and the response names it as cloudStorageSource.
//
// A request with no media (gRPC, or the JSON path) carries no archive, so
// it is INVALID_ARGUMENT, naming the upload path.

// SourceUploadServer serves google.cloud.run.v2.SourceUpload.
type SourceUploadServer struct {
	runpb.UnimplementedSourceUploadServer
	b *BuildsServer
}

// SourceUpload returns the SourceUpload service, which writes through the
// builds' Cloud Storage.
func (b *BuildsServer) SourceUpload() *SourceUploadServer { return &SourceUploadServer{b: b} }

// Register adds the SourceUpload service to a gRPC server or a transcoder.
func (u *SourceUploadServer) Register(g grpc.ServiceRegistrar) {
	runpb.RegisterSourceUploadServer(g, u)
}

// maxUploadSourceBytes is the upload's limit in Google's discovery
// document (maxSize 262144000).
const maxUploadSourceBytes = 262144000

// UploadSource without media: there is no archive to store.
func (u *SourceUploadServer) UploadSource(_ context.Context, req *runpb.UploadSourceRequest) (*runpb.UploadSourceResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	return nil, apierror.InvalidArgument("UploadSource takes the source archive as media: POST it to /upload/v2/%s:uploadSource "+
		"with uploadType=multipart or uploadType=media", req.GetParent())
}

// uploadSourcePath matches the media upload path.
var uploadSourcePath = regexp.MustCompile(`^/upload/v2/(projects/[^/]+/locations/[^/:]+):uploadSource$`)

// uploadSourceMeta is UploadSourceRequest as Google's discovery document
// spells it; the Go proto names only service.
type uploadSourceMeta struct {
	Service       string `json:"service"`
	Job           string `json:"job"`
	WorkerPool    string `json:"workerPool"`
	Instance      string `json:"instance"`
	ContainerID   string `json:"containerId"`
	EncryptionKey string `json:"encryptionKey"`
}

var containerIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// Handler serves the media upload path and passes every other request on.
func (u *SourceUploadServer) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := uploadSourcePath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			writeUploadError(w, apierror.InvalidArgument("UploadSource is a POST"))
			return
		}
		resp, err := u.upload(r, m[1])
		if err != nil {
			writeUploadError(w, err)
			return
		}
		b, _ := protojson.Marshal(resp)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(b)
	})
}

func writeUploadError(w http.ResponseWriter, err error) { apierror.WriteJSON(w, err) }

func (u *SourceUploadServer) upload(r *http.Request, parent string) (*runpb.UploadSourceResponse, error) {
	project, location, err := resource.ParseLocation(parent)
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	ow, ok := u.b.fetch.(ObjectWriter)
	if u.b.fetch == nil || !ok {
		return nil, apierror.FailedPrecondition("this instance does not serve Cloud Storage: start it with --services run,storage")
	}
	var meta uploadSourceMeta
	var data []byte
	switch typ := r.URL.Query().Get("uploadType"); typ {
	case "multipart":
		boundary := uploadBoundary(r.Header.Get("Content-Type"))
		if boundary == "" {
			return nil, apierror.InvalidArgument("uploadType=multipart takes a multipart/related body, not %q", r.Header.Get("Content-Type"))
		}
		mr := multipart.NewReader(r.Body, boundary)
		part, err := mr.NextPart()
		if err != nil {
			return nil, apierror.InvalidArgument("the upload has no metadata part: %v", err)
		}
		mb, err := io.ReadAll(io.LimitReader(part, 1<<20))
		if err != nil {
			return nil, apierror.InvalidArgument("read the metadata part: %v", err)
		}
		if len(bytes.TrimSpace(mb)) > 0 {
			if err := json.Unmarshal(mb, &meta); err != nil {
				return nil, apierror.InvalidArgument("the metadata part is not an UploadSourceRequest: %v", err)
			}
		}
		part, err = mr.NextPart()
		if err != nil {
			return nil, apierror.InvalidArgument("the upload has no media part: %v", err)
		}
		if data, err = readUploadSource(part); err != nil {
			return nil, err
		}
	case "media":
		q := r.URL.Query()
		meta = uploadSourceMeta{Service: q.Get("service"), Job: q.Get("job"), WorkerPool: q.Get("workerPool"),
			Instance: q.Get("instance"), ContainerID: q.Get("containerId"), EncryptionKey: q.Get("encryptionKey")}
		if data, err = readUploadSource(r.Body); err != nil {
			return nil, err
		}
	default:
		return nil, apierror.InvalidArgument("uploadType %q is not supported: send multipart or media", typ)
	}
	if len(data) == 0 {
		return nil, apierror.InvalidArgument("the source archive is empty")
	}
	if meta.EncryptionKey != "" {
		// Checked and not used, as a function's kms_key_name: this
		// instance's Cloud Storage does not encrypt with a key.
		if err := checkKMSKey("encryptionKey", meta.EncryptionKey); err != nil {
			return nil, err
		}
	}
	if meta.ContainerID != "" && !containerIDPattern.MatchString(meta.ContainerID) {
		return nil, apierror.InvalidArgument("containerId %q must be lowercase letters, digits, hyphens and periods, "+
			"begin and end with a letter or digit, and be at most 63 characters", meta.ContainerID)
	}
	kind, name := "", ""
	for _, t := range []struct{ kind, name string }{{"services", meta.Service}, {"jobs", meta.Job},
		{"workerpools", meta.WorkerPool}, {"instances", meta.Instance}} {
		if t.name == "" {
			continue
		}
		if kind != "" {
			return nil, apierror.InvalidArgument("name one of service, job, workerPool and instance")
		}
		kind, name = t.kind, t.name[strings.LastIndex(t.name, "/")+1:]
	}
	if kind == "" {
		kind, name = "sources", "upload"
	}
	if meta.ContainerID != "" {
		name += "/" + meta.ContainerID
	}
	suffix := ".zip"
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		suffix = ".tar.gz"
	}
	id := uuid.New()
	now := time.Now()
	object := fmt.Sprintf("%s/%s/%d.%06d-%s%s", kind, name, now.Unix(), now.Nanosecond()/1000, hex.EncodeToString(id[:]), suffix)
	bucket := runSourcesBucket(project, location)
	if err := ow.Put(r.Context(), project, bucket, object, data, true); err != nil {
		return nil, &apierror.Error{Code: codes.Unavailable, Message: fmt.Sprintf("write the source to gs://%s/%s: %v", bucket, object, err)}
	}
	return &runpb.UploadSourceResponse{CloudStorageSource: &runpb.SourceCode_CloudStorageSource{Bucket: bucket, Object: object}}, nil
}

// uploadBoundary is a multipart Content-Type's boundary. gcloud's
// apitools quotes it with single quotes (boundary='===...=='), which
// mime.ParseMediaType keeps or, with '=' inside, refuses: the quotes are
// dropped, and the refused form is read by hand.
func uploadBoundary(ct string) string {
	if mt, params, err := mime.ParseMediaType(ct); err == nil {
		if !strings.HasPrefix(mt, "multipart/") {
			return ""
		}
		b := params["boundary"]
		if len(b) >= 2 && b[0] == '\'' && b[len(b)-1] == '\'' {
			b = b[1 : len(b)-1]
		}
		return b
	}
	mt, rest, _ := strings.Cut(ct, ";")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mt)), "multipart/") {
		return ""
	}
	for _, p := range strings.Split(rest, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok && strings.EqualFold(k, "boundary") {
			if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			return v
		}
	}
	return ""
}

func readUploadSource(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxUploadSourceBytes+1))
	if err != nil {
		return nil, apierror.InvalidArgument("read the source archive: %v", err)
	}
	if len(data) > maxUploadSourceBytes {
		return nil, &apierror.Error{Code: codes.ResourceExhausted, Message: fmt.Sprintf("the source archive is larger than %d bytes", maxUploadSourceBytes)}
	}
	return data, nil
}

// runSourcesBucket is gcloud's regional source bucket name: the project
// with ':' and '.' as '_' and "google" as "elgoog", which bucket names may
// not contain.
func runSourcesBucket(project, region string) string {
	safe := strings.NewReplacer(":", "_", ".", "_").Replace(project)
	safe = strings.ReplaceAll(safe, "google", "elgoog")
	return "run-sources-" + safe + "-" + region
}
