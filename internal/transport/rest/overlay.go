package rest

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Overlay serves fields an API's REST discovery document publishes before
// its proto does (#1788): Cloud Run v2's WorkerPoolScaling.scalingMode is
// in the discovery document from revision 20261108 and in no published
// runpb descriptor. An Overlay is a copy of a package's descriptors with
// those fields added; JSON is decoded and encoded through the copy, and the
// generated message carries the added fields as unknown fields, so the
// server code that knows of them reads them (Overlay.Get) and every other
// path, gRPC among them, sees the message as the proto defines it.
//
// The added fields' numbers are the overlay's own, not Google's (which are
// unpublished): the JSON names are the contract, so anything stored is
// stored as overlay JSON (Overlay.Marshal), never as wire bytes that a
// later proto could read differently.
type Overlay struct {
	files *protoregistry.Files
	types *protoregistry.Types
}

// NewOverlay copies the descriptors of every file in pkg, lets edit add to
// each, and builds the copy. edit is called once per file of the package.
func NewOverlay(pkg string, edit func(*descriptorpb.FileDescriptorProto)) (*Overlay, error) {
	return NewOverlays([]string{pkg}, edit)
}

// NewOverlays is NewOverlay over several packages (an API's versions, as
// AlloyDB's v1, v1beta and v1alpha, #1524): edit is called once per file
// of each, and reads the file's package to tell them apart.
func NewOverlays(pkgs []string, edit func(*descriptorpb.FileDescriptorProto)) (*Overlay, error) {
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	own := map[protoreflect.FullName]bool{}
	for _, p := range pkgs {
		own[protoreflect.FullName(p)] = true
	}
	var add func(f protoreflect.FileDescriptor)
	add = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		for i := 0; i < f.Imports().Len(); i++ {
			add(f.Imports().Get(i).FileDescriptor)
		}
		fdp := protodesc.ToFileDescriptorProto(f)
		if own[f.Package()] {
			edit(fdp)
		}
		set.File = append(set.File, fdp)
	}
	for _, pkg := range pkgs {
		n := 0
		protoregistry.GlobalFiles.RangeFilesByPackage(protoreflect.FullName(pkg), func(f protoreflect.FileDescriptor) bool {
			add(f)
			n++
			return true
		})
		if n == 0 {
			return nil, fmt.Errorf("rest: overlay: no files in package %s", pkg)
		}
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("rest: overlay of %s: %w", strings.Join(pkgs, ", "), err)
	}
	o := &Overlay{files: files, types: &protoregistry.Types{}}
	var regMsgs func(protoreflect.MessageDescriptors) error
	regMsgs = func(ms protoreflect.MessageDescriptors) error {
		for i := 0; i < ms.Len(); i++ {
			if err := o.types.RegisterMessage(dynamicpb.NewMessageType(ms.Get(i))); err != nil {
				return err
			}
			if err := regMsgs(ms.Get(i).Messages()); err != nil {
				return err
			}
		}
		return nil
	}
	for _, pkg := range pkgs {
		files.RangeFilesByPackage(protoreflect.FullName(pkg), func(f protoreflect.FileDescriptor) bool {
			err = regMsgs(f.Messages())
			return err == nil
		})
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

// MustOverlays is NewOverlays, panicking on an error.
func MustOverlays(pkgs []string, edit func(*descriptorpb.FileDescriptorProto)) *Overlay {
	o, err := NewOverlays(pkgs, edit)
	if err != nil {
		panic(err)
	}
	return o
}

// MustOverlay is NewOverlay, panicking on an error, for package variables.
func MustOverlay(pkg string, edit func(*descriptorpb.FileDescriptorProto)) *Overlay {
	o, err := NewOverlay(pkg, edit)
	if err != nil {
		panic(err)
	}
	return o
}

// FindMessageByName resolves the overlay's own messages first, then the
// generated ones.
func (o *Overlay) FindMessageByName(n protoreflect.FullName) (protoreflect.MessageType, error) {
	if mt, err := o.types.FindMessageByName(n); err == nil {
		return mt, nil
	}
	return protoregistry.GlobalTypes.FindMessageByName(n)
}

// FindMessageByURL resolves an Any's type URL, as FindMessageByName.
func (o *Overlay) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	if mt, err := o.types.FindMessageByURL(url); err == nil {
		return mt, nil
	}
	return protoregistry.GlobalTypes.FindMessageByURL(url)
}

// FindExtensionByName resolves an extension among the generated ones.
func (o *Overlay) FindExtensionByName(n protoreflect.FullName) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByName(n)
}

// FindExtensionByNumber resolves an extension among the generated ones.
func (o *Overlay) FindExtensionByNumber(m protoreflect.FullName, n protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByNumber(m, n)
}

// dynamic is the overlay's copy of m, holding m's fields and the added ones
// (from m's unknown fields), or nil when the overlay has no copy of m's type.
func (o *Overlay) dynamic(m proto.Message) (proto.Message, error) {
	if o == nil {
		return nil, nil
	}
	mt, err := o.types.FindMessageByName(m.ProtoReflect().Descriptor().FullName())
	if err != nil {
		return nil, nil
	}
	raw, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	d := mt.New().Interface()
	if err := (proto.UnmarshalOptions{Resolver: o}).Unmarshal(raw, d); err != nil {
		return nil, err
	}
	return d, nil
}

// Unmarshal decodes JSON into m, the overlay's added fields included (as m's
// unknown fields). With a nil overlay it is protojson.Unmarshal.
func (o *Overlay) Unmarshal(b []byte, m proto.Message) error {
	if o == nil {
		return protojson.Unmarshal(b, m)
	}
	mt, err := o.types.FindMessageByName(m.ProtoReflect().Descriptor().FullName())
	if err != nil {
		return protojson.UnmarshalOptions{Resolver: o}.Unmarshal(b, m)
	}
	d := mt.New().Interface()
	if err := (protojson.UnmarshalOptions{Resolver: o}).Unmarshal(b, d); err != nil {
		return err
	}
	raw, err := proto.Marshal(d)
	if err != nil {
		return err
	}
	return proto.Unmarshal(raw, m)
}

// MarshalOptions encodes m as JSON with opts, the overlay's added fields
// included, in m and in any Any it holds.
func (o *Overlay) MarshalOptions(opts protojson.MarshalOptions, m proto.Message) ([]byte, error) {
	if o == nil {
		return opts.Marshal(m)
	}
	opts.Resolver = o
	d, err := o.dynamic(m)
	if err != nil {
		return nil, err
	}
	if d != nil {
		return opts.Marshal(d)
	}
	return opts.Marshal(m)
}

// Marshal is MarshalOptions with the default options.
func (o *Overlay) Marshal(m proto.Message) ([]byte, error) {
	return o.MarshalOptions(protojson.MarshalOptions{}, m)
}

// Get reads an added field of m by its proto name: ok is false when m does
// not set it (or the overlay has no copy of m's type).
func (o *Overlay) Get(m proto.Message, field protoreflect.Name) (protoreflect.Value, bool) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return protoreflect.Value{}, false
	}
	d, err := o.dynamic(m)
	if err != nil || d == nil {
		return protoreflect.Value{}, false
	}
	fd := d.ProtoReflect().Descriptor().Fields().ByName(field)
	if fd == nil || !d.ProtoReflect().Has(fd) {
		return protoreflect.Value{}, false
	}
	return d.ProtoReflect().Get(fd), true
}

// Set sets (or, with an invalid value, clears) an added field of m by its
// proto name, keeping every other field. The value must belong to the
// overlay's copy of the field (a message value from NewField).
func (o *Overlay) Set(m proto.Message, field protoreflect.Name, v protoreflect.Value) error {
	d, err := o.dynamic(m)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("rest: overlay has no %s", m.ProtoReflect().Descriptor().FullName())
	}
	fd := d.ProtoReflect().Descriptor().Fields().ByName(field)
	if fd == nil {
		return fmt.Errorf("rest: overlay has no field %s.%s", m.ProtoReflect().Descriptor().FullName(), field)
	}
	if v.IsValid() {
		d.ProtoReflect().Set(fd, v)
	} else {
		d.ProtoReflect().Clear(fd)
	}
	raw, err := proto.Marshal(d)
	if err != nil {
		return err
	}
	return proto.Unmarshal(raw, m)
}

// Copy is the overlay's copy of m (a dynamic message of the same full name,
// holding m's fields and the added ones), or nil when the overlay has none.
func (o *Overlay) Copy(m proto.Message) (proto.Message, error) { return o.dynamic(m) }

// Back writes a Copy (as changed) back into m, which it resets first.
func (o *Overlay) Back(d, m proto.Message) error {
	raw, err := proto.Marshal(d)
	if err != nil {
		return err
	}
	return proto.Unmarshal(raw, m)
}
