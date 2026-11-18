package run

import (
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Worker pool automatic scaling (#1788). Cloud Run v2's discovery document
// publishes, from revision 20261108 (google.golang.org/api v0.314.0), four
// fields on GoogleCloudRunV2WorkerPoolScaling that no runpb descriptor has
// yet (cloud.google.com/go/run v1.25.0, googleapis master's
// vendor_settings.proto of 2026-11-07):
//
//   - scalingMode: SCALING_MODE_UNSPECIFIED, AUTOMATIC ("Automatically scale
//     between min and max instances."), MANUAL ("Scale to manual instance
//     count.");
//   - minInstanceCount, maxInstanceCount: "The minimum [maximum] count of
//     instances distributed among revisions based on the specified instance
//     split percentages.";
//   - cpuScaling.cpuUtilization: "Accepted values are between 0.1 and 0.90
//     (inclusive) or 0.0 to disable CPU utilization as threshold for
//     scaling."
//
// They are served over REST only, through the transcoder's overlay
// (rest.Overlay): the JSON API accepts, stores and returns them by those
// names. gRPC answers as the proto defines it; the fields ride in the
// WorkerPoolScaling message as unknown fields under the overlay's own
// numbers, which a gRPC client's parser skips (and a Go client keeps, so a
// read-modify-write over gRPC keeps them).
//
// What AUTOMATIC does locally: the pool runs minInstanceCount instances,
// divided among revisions by instanceSplits as manualInstanceCount is.
// Google's worker pool docs (re-read 2026-11-18:
// https://docs.cloud.google.com/run/docs/configuring/workerpools/manual-scaling,
// https://docs.cloud.google.com/run/docs/reference/rest/v2/projects.locations.workerPools)
// describe manual counts and external autoscalers only, and do not say what
// signal grows an AUTOMATIC pool above its minimum or how a CPU target is
// measured; growing on CPU is not implemented (the kind cluster has no
// metrics pipeline) and is #1789's open work. UNVERIFIED (docs re-read
// 2026-11-18, URLs above).

// The overlay's own field numbers for the added fields: far above any
// number Google uses in vendor_settings.proto, never stored as wire bytes.
const (
	fieldScalingMode      = "scaling_mode"
	fieldMinInstanceCount = "min_instance_count"
	fieldMaxInstanceCount = "max_instance_count"
	fieldCPUScaling       = "cpu_scaling"
	fieldCPUUtilization   = "cpu_utilization"

	numScalingMode      = 1000001
	numMinInstanceCount = 1000002
	numMaxInstanceCount = 1000003
	numCPUScaling       = 1000004
)

// ScalingMode numbers, as the discovery document lists them.
const (
	scalingModeUnspecified = 0
	scalingModeAutomatic   = 1
	scalingModeManual      = 2
)

// runOverlay is google.cloud.run.v2 with the worker pool scaling fields the
// discovery document publishes and runpb lacks.
var runOverlay = rest.MustOverlay("google.cloud.run.v2", addWorkerPoolScaling)

// addWorkerPoolScaling adds the fields to WorkerPoolScaling, and the
// CpuScaling message, to vendor_settings.proto's descriptor. A field the
// proto already has (once Google publishes it) is left as published.
func addWorkerPoolScaling(f *descriptorpb.FileDescriptorProto) {
	for _, m := range f.GetMessageType() {
		if m.GetName() != "WorkerPoolScaling" {
			continue
		}
		has := map[string]bool{}
		for _, fd := range m.GetField() {
			has[fd.GetName()] = true
		}
		opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
		add := func(name, json string, num int32, typ descriptorpb.FieldDescriptorProto_Type, typeName string) {
			if has[name] {
				return
			}
			fd := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), JsonName: proto.String(json),
				Number: proto.Int32(num), Label: opt, Type: typ.Enum()}
			if typeName != "" {
				fd.TypeName = proto.String(typeName)
			}
			m.Field = append(m.Field, fd)
		}
		if !has[fieldScalingMode] {
			m.EnumType = append(m.EnumType, &descriptorpb.EnumDescriptorProto{Name: proto.String("ScalingMode"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("SCALING_MODE_UNSPECIFIED"), Number: proto.Int32(scalingModeUnspecified)},
					{Name: proto.String("AUTOMATIC"), Number: proto.Int32(scalingModeAutomatic)},
					{Name: proto.String("MANUAL"), Number: proto.Int32(scalingModeManual)},
				}})
		}
		add(fieldScalingMode, "scalingMode", numScalingMode, descriptorpb.FieldDescriptorProto_TYPE_ENUM,
			".google.cloud.run.v2.WorkerPoolScaling.ScalingMode")
		add(fieldMinInstanceCount, "minInstanceCount", numMinInstanceCount, descriptorpb.FieldDescriptorProto_TYPE_INT32, "")
		add(fieldMaxInstanceCount, "maxInstanceCount", numMaxInstanceCount, descriptorpb.FieldDescriptorProto_TYPE_INT32, "")
		if !has[fieldCPUScaling] {
			add(fieldCPUScaling, "cpuScaling", numCPUScaling, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE,
				".google.cloud.run.v2.CpuScaling")
			// cpuUtilization has presence: 0.0 disables the threshold.
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{Name: proto.String("CpuScaling"),
				Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String(fieldCPUUtilization),
					JsonName: proto.String("cpuUtilization"), Number: proto.Int32(1), Label: opt,
					Type: descriptorpb.FieldDescriptorProto_TYPE_FLOAT.Enum(), OneofIndex: proto.Int32(0),
					Proto3Optional: proto.Bool(true)}},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("_cpu_utilization")}}})
		}
		return
	}
}

// poolScaling is a worker pool's scaling, the added fields included.
type poolScaling struct {
	mode     int32 // scalingMode*
	min, max int32
	cpu      *float32 // cpuScaling.cpuUtilization, when set
}

// scalingOf reads a pool's scaling.
func scalingOf(sc *runpb.WorkerPoolScaling) poolScaling {
	var out poolScaling
	if sc == nil {
		return out
	}
	if v, ok := runOverlay.Get(sc, fieldScalingMode); ok {
		out.mode = int32(v.Enum())
	}
	if v, ok := runOverlay.Get(sc, fieldMinInstanceCount); ok {
		out.min = int32(v.Int())
	}
	if v, ok := runOverlay.Get(sc, fieldMaxInstanceCount); ok {
		out.max = int32(v.Int())
	}
	if v, ok := runOverlay.Get(sc, fieldCPUScaling); ok {
		cs := v.Message()
		if fd := cs.Descriptor().Fields().ByName(fieldCPUUtilization); cs.Has(fd) {
			f := float32(cs.Get(fd).Float())
			out.cpu = &f
		}
	}
	return out
}

// automatic reports whether the pool scales automatically.
func (p poolScaling) automatic() bool { return p.mode == scalingModeAutomatic }

// checkPoolScaling enforces the limits the discovery document states, and
// the ones counts imply.
func checkPoolScaling(sc *runpb.WorkerPoolScaling) error {
	if c := sc.GetManualInstanceCount(); c < 0 {
		return apierror.InvalidArgument("scaling.manualInstanceCount must not be negative")
	}
	p := scalingOf(sc)
	switch {
	case p.min < 0:
		return apierror.InvalidArgument("scaling.minInstanceCount must not be negative")
	case p.max < 0:
		return apierror.InvalidArgument("scaling.maxInstanceCount must not be negative")
	// A minimum above a set maximum is refused, as Cloud Run refuses one on
	// a service's template; for worker pools it is UNVERIFIED.
	// Docs re-read 2026-11-18, still unanswered: https://docs.cloud.google.com/run/docs/reference/rest/v2/projects.locations.workerPools
	case p.max > 0 && p.min > p.max:
		return apierror.InvalidArgument("scaling.minInstanceCount (%d) must not exceed scaling.maxInstanceCount (%d)", p.min, p.max)
	}
	if p.cpu != nil && *p.cpu != 0 && (*p.cpu < 0.1 || *p.cpu > 0.9) {
		return apierror.InvalidArgument("scaling.cpuScaling.cpuUtilization must be between 0.1 and 0.90 (inclusive), or 0.0 to disable it; got %v", *p.cpu)
	}
	return nil
}

// poolInstances is the number of instances a pool runs across its
// revisions: minInstanceCount when it scales automatically, else
// manualInstanceCount.
func poolInstances(wp *runpb.WorkerPool) int32 {
	if p := scalingOf(wp.GetScaling()); p.automatic() {
		return p.min
	}
	return wp.GetScaling().GetManualInstanceCount()
}

// copyOf is the overlay copy of m, for mask merges that may name the added
// fields; m's type is always in the overlay.
func copyOf(m proto.Message) (protoreflect.Message, error) {
	d, err := runOverlay.Copy(m)
	if err != nil {
		return nil, apierror.Internal(err, "copy %s", m.ProtoReflect().Descriptor().FullName())
	}
	return d.ProtoReflect(), nil
}

// WorkerPoolScaling is a pool's scaling as the discovery document defines
// it, for callers that hold the generated message (the console, over gRPC,
// where the added fields arrive as unknown fields and are kept).
type WorkerPoolScaling struct {
	Mode     string // "", "AUTOMATIC" or "MANUAL"
	Manual   *int32
	Min, Max int32
	CPU      *float32 // cpuScaling.cpuUtilization
}

var scalingModeNames = map[int32]string{scalingModeAutomatic: "AUTOMATIC", scalingModeManual: "MANUAL"}

// ReadWorkerPoolScaling reads sc, the added fields included.
func ReadWorkerPoolScaling(sc *runpb.WorkerPoolScaling) WorkerPoolScaling {
	p := scalingOf(sc)
	out := WorkerPoolScaling{Mode: scalingModeNames[p.mode], Min: p.min, Max: p.max, CPU: p.cpu}
	if sc != nil && sc.ManualInstanceCount != nil {
		n := sc.GetManualInstanceCount()
		out.Manual = &n
	}
	return out
}

// WorkerPoolInstances is the number of instances a pool runs: its minimum
// when it scales automatically, else its manual count.
func WorkerPoolInstances(wp *runpb.WorkerPool) int32 { return poolInstances(wp) }

// Message is s as the generated message, the added fields as unknown
// fields, which UpdateWorkerPool and CreateWorkerPool read over gRPC too.
func (s WorkerPoolScaling) Message() (*runpb.WorkerPoolScaling, error) {
	sc := &runpb.WorkerPoolScaling{ManualInstanceCount: s.Manual}
	d, err := runOverlay.Copy(sc)
	if err != nil {
		return nil, err
	}
	m := d.ProtoReflect()
	fields := m.Descriptor().Fields()
	for num, name := range scalingModeNames {
		if name == s.Mode {
			m.Set(fields.ByName(fieldScalingMode), protoreflect.ValueOfEnum(protoreflect.EnumNumber(num)))
		}
	}
	if s.Mode != "" && !m.Has(fields.ByName(fieldScalingMode)) {
		return nil, apierror.InvalidArgument("unknown scaling mode %q", s.Mode)
	}
	if s.Min != 0 {
		m.Set(fields.ByName(fieldMinInstanceCount), protoreflect.ValueOfInt32(s.Min))
	}
	if s.Max != 0 {
		m.Set(fields.ByName(fieldMaxInstanceCount), protoreflect.ValueOfInt32(s.Max))
	}
	if s.CPU != nil {
		cs := m.NewField(fields.ByName(fieldCPUScaling))
		cs.Message().Set(cs.Message().Descriptor().Fields().ByName(fieldCPUUtilization), protoreflect.ValueOfFloat32(*s.CPU))
		m.Set(fields.ByName(fieldCPUScaling), cs)
	}
	if err := runOverlay.Back(d, sc); err != nil {
		return nil, err
	}
	return sc, checkPoolScaling(sc)
}
