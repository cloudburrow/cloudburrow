package tflitemodel

import (
	"errors"
	"fmt"
)

// Metadata is the buffer a model names name in its Model.metadata list
// (schema.fbs: Metadata{name, buffer}), such as "TFLITE_METADATA", the
// FlatBuffer of LiteRT's metadata_schema.fbs that MediaPipe's models
// carry (#1799). It is false when the model has no such entry.
func Metadata(data []byte, name string) (b []byte, ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			b, ok, err = nil, false, fmt.Errorf("the file is not a valid TensorFlow Lite model: %v", r)
		}
	}()
	if len(data) < 8 || string(data[4:8]) != "TFL3" {
		return nil, false, errors.New("the file is not a TensorFlow Lite model (it lacks the TFL3 FlatBuffer identifier)")
	}
	root := table(data, 0)
	bufs := tables(root, 12)
	for _, md := range tables(root, 16) {
		if str(md, 4) != name {
			continue
		}
		i := int(md.GetUint32Slot(6, 0))
		if i <= 0 || i >= len(bufs) {
			return nil, false, fmt.Errorf("the model's %s metadata names buffer %d, which it does not have", name, i)
		}
		p := field(bufs[i], 4)
		if p == 0 {
			return nil, false, fmt.Errorf("the model's %s metadata buffer is empty", name)
		}
		return bufs[i].ByteVector(bufs[i].Pos + p), true, nil
	}
	return nil, false, nil
}
