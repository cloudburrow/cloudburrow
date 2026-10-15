package run

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// v1DecodeService decodes a v1 Service body as rest.DecodeJSON does,
// refusing unknown fields, except spec.template.spec.sandboxes: newer
// gcloud releases send it (Cloud Run's sandbox CLI templates), which the
// generated Go client this adapter is built on does not know. An empty one
// is dropped; one that sets templates is 501, as there is no sandbox CLI
// locally.
func v1DecodeService(r *http.Request, dst *runv1.Service) error {
	return v1DecodeDroppingSandboxes(r, dst, "spec", "template", "spec")
}

// v1DecodeJob is v1DecodeService for a v1 Job (#1349), whose task template
// is where gcloud sends the empty sandboxes:
// spec.template.spec.template.spec.sandboxes.
func v1DecodeJob(r *http.Request, dst *runv1.Job) error {
	return v1DecodeDroppingSandboxes(r, dst, "spec", "template", "spec", "template", "spec")
}

// v1DecodeDroppingSandboxes decodes a body as rest.DecodeJSON does, after
// dropping an empty sandboxes field from the object at path.
func v1DecodeDroppingSandboxes(r *http.Request, dst any, path ...string) error {
	if r.Body == nil {
		return apierror.InvalidArgument("request body is required")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return apierror.InvalidArgument("request body: %v", err)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) == nil {
		ts := doc
		for _, k := range path {
			ts, _ = ts[k].(map[string]any)
		}
		if sb, ok := ts["sandboxes"]; ok {
			if m, _ := sb.(map[string]any); sb != nil && len(m) > 0 {
				if t, _ := m["templates"].([]any); len(t) > 0 || len(m) > 1 {
					return apierror.Unimplemented("unsupported Cloud Run v1 configuration: %s.sandboxes: not mapped", strings.Join(path, "."))
				}
			}
			delete(ts, "sandboxes")
			if raw, err = json.Marshal(doc); err != nil {
				return apierror.Internal(err, "re-encode the request")
			}
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return rest.DecodeJSON(r, dst)
}

// The v1 counterparts of the v2 volumes, mounts and probes (#1297). v1 and
// v2 share one mapping, so each is converted to v2's shape and checked and
// rendered there; what v2 refuses, v1 refuses in the same words.

// fromV1Volumes converts v1 volumes, naming the ones v2 has no shape for.
func fromV1Volumes(in []*runv1.Volume, gap func(bool, string)) []*runpb.Volume {
	var out []*runpb.Volume
	for _, v := range in {
		o := &runpb.Volume{Name: v.Name}
		switch {
		case v.Secret != nil:
			s := &runpb.SecretVolumeSource{Secret: v.Secret.SecretName, DefaultMode: int32(v.Secret.DefaultMode)}
			for _, it := range v.Secret.Items {
				s.Items = append(s.Items, &runpb.VersionToPath{Path: it.Path, Version: it.Key, Mode: int32(it.Mode)})
			}
			gap(v.Secret.Optional, "spec.template.spec.volumes.secret.optional")
			o.VolumeType = &runpb.Volume_Secret{Secret: s}
		case v.EmptyDir != nil:
			e := &runpb.EmptyDirVolumeSource{SizeLimit: v.EmptyDir.SizeLimit}
			switch v.EmptyDir.Medium {
			case "Memory":
				e.Medium = runpb.EmptyDirVolumeSource_MEMORY
			case "":
			default:
				gap(true, "spec.template.spec.volumes.emptyDir.medium")
			}
			o.VolumeType = &runpb.Volume_EmptyDir{EmptyDir: e}
		case v.Nfs != nil:
			o.VolumeType = &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{Server: v.Nfs.Server, Path: v.Nfs.Path, ReadOnly: v.Nfs.ReadOnly}}
		default:
			// configMap and csi (Cloud Storage FUSE) have no local source.
			gap(true, "spec.template.spec.volumes["+v.Name+"]")
			continue
		}
		out = append(out, o)
	}
	return out
}

func fromV1Mounts(in []*runv1.VolumeMount) []*runpb.VolumeMount {
	var out []*runpb.VolumeMount
	for _, m := range in {
		out = append(out, &runpb.VolumeMount{Name: m.Name, MountPath: m.MountPath, SubPath: m.SubPath})
	}
	return out
}

// fromV1Probe converts a v1 probe. An exec probe has no v2 counterpart.
func fromV1Probe(p *runv1.Probe, field string, gap func(bool, string)) *runpb.Probe {
	if p == nil {
		return nil
	}
	out := &runpb.Probe{InitialDelaySeconds: int32(p.InitialDelaySeconds), TimeoutSeconds: int32(p.TimeoutSeconds),
		PeriodSeconds: int32(p.PeriodSeconds), FailureThreshold: int32(p.FailureThreshold)}
	gap(p.SuccessThreshold > 1, field+".successThreshold")
	gap(p.Exec != nil, field+".exec")
	switch {
	case p.HttpGet != nil:
		gap(p.HttpGet.Host != "" || (p.HttpGet.Scheme != "" && p.HttpGet.Scheme != "HTTP"), field+".httpGet.host/scheme")
		h := &runpb.HTTPGetAction{Path: p.HttpGet.Path, Port: int32(p.HttpGet.Port)}
		for _, hh := range p.HttpGet.HttpHeaders {
			h.HttpHeaders = append(h.HttpHeaders, &runpb.HTTPHeader{Name: hh.Name, Value: hh.Value})
		}
		out.ProbeType = &runpb.Probe_HttpGet{HttpGet: h}
	case p.TcpSocket != nil:
		gap(p.TcpSocket.Host != "", field+".tcpSocket.host")
		out.ProbeType = &runpb.Probe_TcpSocket{TcpSocket: &runpb.TCPSocketAction{Port: int32(p.TcpSocket.Port)}}
	case p.Grpc != nil:
		out.ProbeType = &runpb.Probe_Grpc{Grpc: &runpb.GRPCAction{Port: int32(p.Grpc.Port), Service: p.Grpc.Service}}
	}
	return out
}

func toV1Volumes(in []*runpb.Volume) []*runv1.Volume {
	var out []*runv1.Volume
	for _, v := range in {
		if v.GetCloudSqlInstance() != nil {
			// v1's run.googleapis.com/cloudsql-instances annotation.
			continue
		}
		o := &runv1.Volume{Name: v.GetName()}
		switch {
		case v.GetSecret() != nil:
			s := v.GetSecret()
			o.Secret = &runv1.SecretVolumeSource{SecretName: s.GetSecret(), DefaultMode: int64(s.GetDefaultMode())}
			for _, it := range s.GetItems() {
				o.Secret.Items = append(o.Secret.Items, &runv1.KeyToPath{Key: it.GetVersion(), Path: it.GetPath(), Mode: int64(it.GetMode())})
			}
		case v.GetEmptyDir() != nil:
			o.EmptyDir = &runv1.EmptyDirVolumeSource{SizeLimit: v.GetEmptyDir().GetSizeLimit()}
			if v.GetEmptyDir().GetMedium() == runpb.EmptyDirVolumeSource_MEMORY {
				o.EmptyDir.Medium = "Memory"
			}
		}
		out = append(out, o)
	}
	return out
}

func toV1Mounts(in []*runpb.VolumeMount, volumes []*runpb.Volume) []*runv1.VolumeMount {
	secret, sql := map[string]bool{}, map[string]bool{}
	for _, v := range volumes {
		secret[v.GetName()] = v.GetSecret() != nil
		sql[v.GetName()] = v.GetCloudSqlInstance() != nil
	}
	var out []*runv1.VolumeMount
	for _, m := range in {
		if sql[m.GetName()] {
			continue
		}
		out = append(out, &runv1.VolumeMount{Name: m.GetName(), MountPath: m.GetMountPath(), ReadOnly: secret[m.GetName()]})
	}
	return out
}

// toV1Probe renders a probe as v1 reports it: on the serving port when it
// names none, as Cloud Run fills it in and as the probe was rendered.
func toV1Probe(p *runpb.Probe, serving int32) *runv1.Probe {
	if p == nil {
		return nil
	}
	if serving == 0 {
		serving = 8080
	}
	port := func(n int32) int64 {
		if n == 0 {
			return int64(serving)
		}
		return int64(n)
	}
	out := &runv1.Probe{InitialDelaySeconds: int64(p.GetInitialDelaySeconds()), TimeoutSeconds: int64(p.GetTimeoutSeconds()),
		PeriodSeconds: int64(p.GetPeriodSeconds()), FailureThreshold: int64(p.GetFailureThreshold())}
	switch {
	case p.GetHttpGet() != nil:
		h := p.GetHttpGet()
		out.HttpGet = &runv1.HTTPGetAction{Path: h.GetPath(), Port: port(h.GetPort())}
		for _, hh := range h.GetHttpHeaders() {
			out.HttpGet.HttpHeaders = append(out.HttpGet.HttpHeaders, &runv1.HTTPHeader{Name: hh.GetName(), Value: hh.GetValue()})
		}
	case p.GetTcpSocket() != nil:
		out.TcpSocket = &runv1.TCPSocketAction{Port: port(p.GetTcpSocket().GetPort())}
	case p.GetGrpc() != nil:
		out.Grpc = &runv1.GRPCAction{Port: port(p.GetGrpc().GetPort()), Service: p.GetGrpc().GetService()}
	}
	return out
}
