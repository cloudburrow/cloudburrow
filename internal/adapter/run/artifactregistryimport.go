package run

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
)

// ImportAptArtifacts and ImportYumArtifacts (#1531) read .deb and .rpm
// objects from this instance's Cloud Storage into an APT or YUM
// repository: each package's control fields (a .deb's control file, an
// .rpm's header) name its package, version and architecture, and the
// object is stored as that version's file, under the pool path Google's
// apt repositories use.

var importReader struct {
	mu sync.Mutex
	f  SourceFetcher
}

// UseImportReader sets the Cloud Storage reader the imports use.
func UseImportReader(f SourceFetcher) {
	importReader.mu.Lock()
	importReader.f = f
	importReader.mu.Unlock()
}

const maxImportObject = 512 << 20

func readGCS(ctx context.Context, uri string) ([]byte, error) {
	importReader.mu.Lock()
	f := importReader.f
	importReader.mu.Unlock()
	if f == nil {
		return nil, apierror.FailedPrecondition("the import reads from Cloud Storage, which is not enabled on this instance: start it with storage in --services")
	}
	bucket, object, ok := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	if !strings.HasPrefix(uri, "gs://") || !ok || bucket == "" || object == "" {
		return nil, fmt.Errorf("invalid Cloud Storage URI %q: want gs://BUCKET/OBJECT", uri)
	}
	rc, err := f.Fetch(ctx, bucket, object, 0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxImportObject))
}

func importURIs(uris []string, wildcards bool) error {
	if wildcards {
		return apierror.Unimplemented("use_wildcards is not supported: name each object")
	}
	if len(uris) == 0 {
		return apierror.InvalidArgument("gcs_source.uris is required")
	}
	return nil
}

func errInfo(uri string, err error) *status.Status {
	return &status.Status{Code: 3, Message: uri + ": " + err.Error()}
}

// ImportAptArtifacts imports .deb objects into an APT repository.
func (a *ArtifactRegistryServer) ImportAptArtifacts(ctx context.Context, req *arpb.ImportAptArtifactsRequest) (*longrunningpb.Operation, error) {
	r, err := formatRepo(req.GetParent(), arpb.Repository_APT)
	if err != nil {
		return nil, err
	}
	src := req.GetGcsSource()
	if err := importURIs(src.GetUris(), src.GetUseWildcards()); err != nil {
		return nil, err
	}
	resp := &arpb.ImportAptArtifactsResponse{}
	for _, uri := range src.GetUris() {
		art, err := importDeb(ctx, r, uri)
		if err != nil {
			resp.Errors = append(resp.Errors, &arpb.ImportAptArtifactsErrorInfo{
				Source: &arpb.ImportAptArtifactsErrorInfo_GcsSource{GcsSource: &arpb.ImportAptArtifactsGcsSource{Uris: []string{uri}}},
				Error:  errInfo(uri, err)})
			continue
		}
		resp.AptArtifacts = append(resp.AptArtifacts, art)
	}
	return a.doneWith(req.GetParent(), &arpb.ImportAptArtifactsMetadata{}, resp)
}

func (a *ArtifactRegistryServer) doneWith(repoName string, meta, resp proto.Message) (*longrunningpb.Operation, error) {
	if a.s == nil {
		return nil, nil
	}
	loc := repoName[:strings.Index(repoName, "/repositories/")]
	op := a.s.ops.Create(loc, repoName)
	_ = a.s.ops.SetMetadata(op.Name, meta)
	_ = a.s.ops.Succeed(op.Name, resp)
	return a.s.toProtoOperation(op.Name)
}

func importDeb(ctx context.Context, r *arpb.Repository, uri string) (*arpb.AptArtifact, error) {
	b, err := readGCS(ctx, uri)
	if err != nil {
		return nil, err
	}
	control, err := debControl(b)
	if err != nil {
		return nil, err
	}
	fields := controlFields(control)
	pkg, ver, arch := fields["Package"], fields["Version"], fields["Architecture"]
	if pkg == "" || ver == "" || arch == "" {
		return nil, errors.New("the control file has no Package, Version or Architecture")
	}
	first := pkg[:1]
	if strings.HasPrefix(pkg, "lib") && len(pkg) > 3 {
		first = pkg[:4]
	}
	id := "pool/" + first + "/" + pkg + "/" + pkg + "_" + ver + "_" + arch + ".deb"
	if _, err := addFile(r.GetName(), id, pkg, ver, b, map[string]string{"arch": arch, "control": string(control)}); err != nil {
		return nil, err
	}
	return &arpb.AptArtifact{Name: r.GetName() + "/aptArtifacts/" + esc(pkg) + ":" + esc(ver) + ":" + esc(arch),
		PackageName: pkg, PackageType: arpb.AptArtifact_BINARY, Architecture: arch, Component: "main", ControlFile: control}, nil
}

// debControl reads a .deb's control file: an ar archive whose
// control.tar[.gz|.zst] member holds ./control.
func debControl(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, []byte("!<arch>\n")) {
		return nil, errors.New("not a Debian package: no ar header")
	}
	rest := b[8:]
	for len(rest) >= 60 {
		hdr := rest[:60]
		name := strings.TrimRight(strings.TrimSpace(string(hdr[:16])), "/")
		var size int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(hdr[48:58])), "%d", &size); err != nil || size < 0 || 60+size > len(rest) {
			return nil, errors.New("not a Debian package: bad ar member")
		}
		data := rest[60 : 60+size]
		rest = rest[60+size:]
		if size%2 == 1 && len(rest) > 0 {
			rest = rest[1:]
		}
		if !strings.HasPrefix(name, "control.tar") {
			continue
		}
		var tr io.Reader = bytes.NewReader(data)
		switch path.Ext(name) {
		case ".gz":
			zr, err := gzip.NewReader(tr)
			if err != nil {
				return nil, err
			}
			tr = zr
		case ".zst":
			zr, err := zstd.NewReader(tr)
			if err != nil {
				return nil, err
			}
			defer zr.Close()
			tr = zr
		case ".tar":
		default:
			return nil, fmt.Errorf("%s is not supported: only gzip, zstd or uncompressed control archives", name)
		}
		t := tar.NewReader(tr)
		for {
			h, err := t.Next()
			if err != nil {
				return nil, errors.New("the control archive has no control file")
			}
			if path.Clean(h.Name) == "control" {
				return io.ReadAll(io.LimitReader(t, 1<<20))
			}
		}
	}
	return nil, errors.New("not a Debian package: no control.tar member")
}

// controlFields parses a Debian control paragraph's single-line fields.
func controlFields(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// ImportYumArtifacts imports .rpm objects into a YUM repository.
func (a *ArtifactRegistryServer) ImportYumArtifacts(ctx context.Context, req *arpb.ImportYumArtifactsRequest) (*longrunningpb.Operation, error) {
	r, err := formatRepo(req.GetParent(), arpb.Repository_YUM)
	if err != nil {
		return nil, err
	}
	src := req.GetGcsSource()
	if err := importURIs(src.GetUris(), src.GetUseWildcards()); err != nil {
		return nil, err
	}
	resp := &arpb.ImportYumArtifactsResponse{}
	for _, uri := range src.GetUris() {
		art, err := importRPM(ctx, r, uri)
		if err != nil {
			resp.Errors = append(resp.Errors, &arpb.ImportYumArtifactsErrorInfo{
				Source: &arpb.ImportYumArtifactsErrorInfo_GcsSource{GcsSource: &arpb.ImportYumArtifactsGcsSource{Uris: []string{uri}}},
				Error:  errInfo(uri, err)})
			continue
		}
		resp.YumArtifacts = append(resp.YumArtifacts, art)
	}
	return a.doneWith(req.GetParent(), &arpb.ImportYumArtifactsMetadata{}, resp)
}

func importRPM(ctx context.Context, r *arpb.Repository, uri string) (*arpb.YumArtifact, error) {
	b, err := readGCS(ctx, uri)
	if err != nil {
		return nil, err
	}
	h, err := rpmHeader(b)
	if err != nil {
		return nil, err
	}
	name, ver, rel, arch := h[1000], h[1001], h[1002], h[1022]
	if name == "" || ver == "" || arch == "" {
		return nil, errors.New("the RPM header has no name, version or architecture")
	}
	full := ver
	if rel != "" {
		full += "-" + rel
	}
	id := name + "-" + full + "." + arch + ".rpm"
	typ := arpb.YumArtifact_BINARY
	if h[1044] == "" { // SOURCERPM is unset on a source package
		typ = arpb.YumArtifact_SOURCE
	}
	if _, err := addFile(r.GetName(), id, name, full, b, map[string]string{"arch": arch}); err != nil {
		return nil, err
	}
	return &arpb.YumArtifact{Name: r.GetName() + "/yumArtifacts/" + esc(name) + ":" + esc(full) + ":" + esc(arch),
		PackageName: name, PackageType: typ, Architecture: arch}, nil
}

// rpmHeader reads an .rpm's string tags: its 96-byte lead, the signature
// header (padded to 8 bytes), then the header whose tags name it.
func rpmHeader(b []byte) (map[int32]string, error) {
	if len(b) < 96 || !bytes.Equal(b[:4], []byte{0xed, 0xab, 0xee, 0xdb}) {
		return nil, errors.New("not an RPM package: no lead")
	}
	off := 96
	readHeader := func(off int) (map[int32]string, int, error) {
		if off+16 > len(b) || !bytes.Equal(b[off:off+3], []byte{0x8e, 0xad, 0xe8}) {
			return nil, 0, errors.New("not an RPM package: bad header")
		}
		n := int(binary.BigEndian.Uint32(b[off+8:]))
		size := int(binary.BigEndian.Uint32(b[off+12:]))
		idx := off + 16
		store := idx + 16*n
		if n < 0 || size < 0 || store+size > len(b) {
			return nil, 0, errors.New("not an RPM package: truncated header")
		}
		out := map[int32]string{}
		for i := 0; i < n; i++ {
			e := b[idx+16*i:]
			tag := int32(binary.BigEndian.Uint32(e))
			typ := binary.BigEndian.Uint32(e[4:])
			o := int(binary.BigEndian.Uint32(e[8:]))
			if (typ == 6 || typ == 9) && o >= 0 && o < size { // STRING, I18NSTRING
				s := b[store+o : store+size]
				if j := bytes.IndexByte(s, 0); j >= 0 {
					s = s[:j]
				}
				out[tag] = string(s)
			}
		}
		return out, store + size, nil
	}
	_, end, err := readHeader(off)
	if err != nil {
		return nil, err
	}
	if pad := end % 8; pad != 0 {
		end += 8 - pad
	}
	h, _, err := readHeader(end)
	return h, err
}
