// Package filestorecsi is the Filestore CSI driver of the Kubernetes
// Engine clusters (#2192), filestore.csi.storage.gke.io, as GKE's
// GcpFilestoreCsiDriver add-on runs it
// (https://docs.cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/filestore-csi-driver,
// read 2026-11-23):
//
//   - the controller (controller.go), in the CLI, installs the driver's
//     CSIDriver and the StorageClasses GKE installs (standard-rwx,
//     premium-rwx, zonal-rwx, enterprise-rwx and regional-rwx), and
//     provisions each PersistentVolumeClaim of a StorageClass of the
//     driver as a Filestore instance made through the served Filestore
//     API, named pvc-UID as GKE names it, with a PersistentVolume whose
//     volumeHandle is modeInstance/LOCATION/INSTANCE/SHARE and whose
//     volumeAttributes are ip and volume (and protocol); a released
//     volume of reclaim policy Delete deletes its instance;
//   - the node plugin (this file), `cloudburrow-storage
//     filestore-csi-node` in each node, registers the driver with the
//     kubelet and mounts IP:/SHARE over NFS where the kubelet asks.
//
// The node plugin speaks CSI v1 and the kubelet's plugin registration
// over gRPC, its messages encoded with protowire: the few fields it reads
// and writes are the CSI spec's (csi.proto) and the kubelet's
// (pluginregistration api.proto), so it links no generated code.
package filestorecsi

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// Driver is the driver's name.
const Driver = "filestore.csi.storage.gke.io"

// Version is the driver version the plugin reports.
const Version = "v1.0.0-cloudburrow"

// PluginDir is where the node plugin's CSI socket is; RegistrationDir the
// kubelet's plugin registration directory.
const (
	PluginDir       = "/var/lib/kubelet/plugins/" + Driver
	RegistrationDir = "/var/lib/kubelet/plugins_registry"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// rawCodec passes messages through as bytes: the handlers encode and
// decode them with protowire.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: %T", v)
	}
	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: %T", v)
	}
	*b = append((*b)[:0], data...)
	return nil
}

var _ encoding.Codec = rawCodec{}

// msg is a decoded message: its string and bool fields by number, and its
// map<string,string> fields' entries.
type msg struct {
	str  map[protowire.Number][]string
	vint map[protowire.Number]uint64
	maps map[protowire.Number]map[string]string
}

func decode(b []byte) (msg, error) {
	m := msg{str: map[protowire.Number][]string{}, vint: map[protowire.Number]uint64{}, maps: map[protowire.Number]map[string]string{}}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return m, protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return m, protowire.ParseError(n)
			}
			b = b[n:]
			m.str[num] = append(m.str[num], string(v))
			// It may be a map entry: key 1, value 2.
			if k, val, ok := mapEntry(v); ok {
				if m.maps[num] == nil {
					m.maps[num] = map[string]string{}
				}
				m.maps[num][k] = val
			}
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return m, protowire.ParseError(n)
			}
			b = b[n:]
			m.vint[num] = v
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return m, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return m, nil
}

func mapEntry(b []byte) (key, value string, ok bool) {
	seen := false
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || typ != protowire.BytesType {
			return "", "", false
		}
		b = b[n:]
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return "", "", false
		}
		b = b[n:]
		switch num {
		case 1:
			key, seen = string(v), true
		case 2:
			value = string(v)
		default:
			return "", "", false
		}
	}
	return key, value, seen
}

func (m msg) get(n protowire.Number) string {
	if v := m.str[n]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// enc builds a message.
type enc []byte

func (e enc) str(n protowire.Number, s string) enc {
	if s == "" {
		return e
	}
	e = protowire.AppendTag(e, n, protowire.BytesType)
	return protowire.AppendString(e, s)
}

func (e enc) boolean(n protowire.Number, v bool) enc {
	if !v {
		return e
	}
	e = protowire.AppendTag(e, n, protowire.VarintType)
	return protowire.AppendVarint(e, 1)
}

func (e enc) message(n protowire.Number, b []byte) enc {
	e = protowire.AppendTag(e, n, protowire.BytesType)
	return protowire.AppendBytes(e, b)
}

// handler is one method: request bytes in, response bytes out.
type handler func(ctx context.Context, req msg) (enc, error)

func serviceDesc(name string, methods map[string]handler) *grpc.ServiceDesc {
	desc := &grpc.ServiceDesc{ServiceName: name, HandlerType: (*any)(nil)}
	for method, h := range methods {
		h := h
		desc.Methods = append(desc.Methods, grpc.MethodDesc{MethodName: method,
			Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				var in []byte
				if err := dec(&in); err != nil {
					return nil, err
				}
				m, err := decode(in)
				if err != nil {
					return nil, status.Error(codes.InvalidArgument, err.Error())
				}
				out, err := h(ctx, m)
				if err != nil {
					return nil, err
				}
				b := []byte(out)
				if b == nil {
					b = []byte{}
				}
				return &b, nil
			}})
	}
	return desc
}

// Node is the node plugin.
type Node struct {
	// NodeID is the node's name.
	NodeID string
	// Mount runs mount(8) and umount(8); nil runs them.
	Mount func(ctx context.Context, args ...string) error
	// Mounted reports whether path is a mount point; nil reads
	// /proc/self/mountinfo.
	Mounted func(path string) bool
}

func (n *Node) mount(ctx context.Context, args ...string) error {
	if n.Mount != nil {
		return n.Mount(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (n *Node) mounted(path string) bool {
	if n.Mounted != nil {
		return n.Mounted(path)
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 4 && f[4] == path {
			return true
		}
	}
	return false
}

// MountArgs is the mount(8) command for a volume's context.
func MountArgs(vc map[string]string, target string, readonly bool) ([]string, error) {
	ip, share := vc["ip"], vc["volume"]
	if net.ParseIP(ip) == nil || share == "" || strings.ContainsAny(share, "/ ,") {
		return nil, fmt.Errorf("the volume's context needs ip and volume: %v", vc)
	}
	opts := []string{"vers=3"}
	if vc["protocol"] == "NFS_V4_1" {
		opts = []string{"vers=4.1"}
	}
	if readonly {
		opts = append(opts, "ro")
	}
	return []string{"mount", "-t", "nfs", "-o", strings.Join(opts, ","), ip + ":/" + share, target}, nil
}

func (n *Node) services() []*grpc.ServiceDesc {
	identity := serviceDesc("csi.v1.Identity", map[string]handler{
		"GetPluginInfo": func(context.Context, msg) (enc, error) {
			return enc(nil).str(1, Driver).str(2, Version), nil
		},
		"GetPluginCapabilities": func(context.Context, msg) (enc, error) { return enc{}, nil },
		// ready: a BoolValue holding true.
		"Probe": func(context.Context, msg) (enc, error) { return enc(nil).message(1, enc(nil).boolean(1, true)), nil },
	})
	node := serviceDesc("csi.v1.Node", map[string]handler{
		"NodeGetCapabilities": func(context.Context, msg) (enc, error) { return enc{}, nil },
		"NodeGetInfo": func(context.Context, msg) (enc, error) {
			return enc(nil).str(1, n.NodeID), nil
		},
		"NodePublishVolume": func(ctx context.Context, m msg) (enc, error) {
			target := m.get(4)
			if m.get(1) == "" || target == "" {
				return nil, status.Error(codes.InvalidArgument, "volume_id and target_path are required")
			}
			if n.mounted(target) {
				return enc{}, nil
			}
			args, err := MountArgs(m.maps[8], target, m.vint[6] == 1)
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			if err := os.MkdirAll(target, 0o750); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			if err := n.mount(ctx, args...); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			return enc{}, nil
		},
		"NodeUnpublishVolume": func(ctx context.Context, m msg) (enc, error) {
			target := m.get(2)
			if target == "" {
				return nil, status.Error(codes.InvalidArgument, "target_path is required")
			}
			if n.mounted(target) {
				if err := n.mount(ctx, "umount", target); err != nil {
					return nil, status.Error(codes.Internal, err.Error())
				}
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return nil, status.Error(codes.Internal, err.Error())
			}
			return enc{}, nil
		},
	})
	return []*grpc.ServiceDesc{identity, node}
}

// registration is the kubelet's plugin registration service.
func registration(endpoint string) *grpc.ServiceDesc {
	return serviceDesc("pluginregistration.Registration", map[string]handler{
		"GetInfo": func(context.Context, msg) (enc, error) {
			return enc(nil).str(1, "CSIPlugin").str(2, Driver).str(3, endpoint).str(4, "1.0.0"), nil
		},
		"NotifyRegistrationStatus": func(_ context.Context, m msg) (enc, error) {
			if m.vint[1] != 1 {
				fmt.Fprintf(os.Stderr, "filestore-csi-node: the kubelet did not register the driver: %s\n", m.get(2))
			}
			return enc{}, nil
		},
	})
}

func listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return net.Listen("unix", path)
}

// Serve serves the plugin's CSI socket in dir and its registration socket
// in regDir until ctx ends.
func (n *Node) Serve(ctx context.Context, dir, regDir string) error {
	csiSock := filepath.Join(dir, "csi.sock")
	regSock := filepath.Join(regDir, Driver+"-reg.sock")
	l, err := listen(csiSock)
	if err != nil {
		return err
	}
	rl, err := listen(regSock)
	if err != nil {
		_ = l.Close()
		return err
	}
	srv := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}))
	for _, d := range n.services() {
		srv.RegisterService(d, nil)
	}
	reg := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}))
	reg.RegisterService(registration(csiSock), nil)
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(l) }()
	go func() { errc <- reg.Serve(rl) }()
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	srv.Stop()
	reg.Stop()
	_ = os.Remove(regSock)
	return err
}

// Run is `cloudburrow-storage filestore-csi-node`.
func Run(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) error {
	fs := flag.NewFlagSet("filestore-csi-node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nodeID := fs.String("node-id", "", "the node's name")
	dir := fs.String("plugin-dir", PluginDir, "where the CSI socket is made")
	regDir := fs.String("registration-dir", RegistrationDir, "the kubelet's plugin registration directory")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *nodeID == "" {
		host, err := os.Hostname()
		if err != nil {
			return err
		}
		*nodeID = host
	}
	n := &Node{NodeID: *nodeID}
	return n.Serve(ctx, *dir, *regDir)
}
