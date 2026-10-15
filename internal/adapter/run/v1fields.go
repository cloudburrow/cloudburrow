package run

import (
	runpb "cloud.google.com/go/run/apiv2/runpb"
	runv1 "google.golang.org/api/run/v1"
)

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
	secret := map[string]bool{}
	for _, v := range volumes {
		secret[v.GetName()] = v.GetSecret() != nil
	}
	var out []*runv1.VolumeMount
	for _, m := range in {
		out = append(out, &runv1.VolumeMount{Name: m.GetName(), MountPath: m.GetMountPath(), ReadOnly: secret[m.GetName()]})
	}
	return out
}

func toV1Probe(p *runpb.Probe) *runv1.Probe {
	if p == nil {
		return nil
	}
	out := &runv1.Probe{InitialDelaySeconds: int64(p.GetInitialDelaySeconds()), TimeoutSeconds: int64(p.GetTimeoutSeconds()),
		PeriodSeconds: int64(p.GetPeriodSeconds()), FailureThreshold: int64(p.GetFailureThreshold())}
	switch {
	case p.GetHttpGet() != nil:
		h := p.GetHttpGet()
		out.HttpGet = &runv1.HTTPGetAction{Path: h.GetPath(), Port: int64(h.GetPort())}
		for _, hh := range h.GetHttpHeaders() {
			out.HttpGet.HttpHeaders = append(out.HttpGet.HttpHeaders, &runv1.HTTPHeader{Name: hh.GetName(), Value: hh.GetValue()})
		}
	case p.GetTcpSocket() != nil:
		out.TcpSocket = &runv1.TCPSocketAction{Port: int64(p.GetTcpSocket().GetPort())}
	case p.GetGrpc() != nil:
		out.Grpc = &runv1.GRPCAction{Port: int64(p.GetGrpc().GetPort()), Service: p.GetGrpc().GetService()}
	}
	return out
}
