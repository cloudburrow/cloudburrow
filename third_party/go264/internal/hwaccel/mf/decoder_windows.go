package mf

const defaultSampleSpacing = 400000

type DecodedPicture struct {
	I420   []byte
	Width  int
	Height int
}

type Decoder struct {
	name     string
	hardware bool

	direct3D    bool
	manager     *deviceManager
	reader      *textureReader
	sawTextures bool

	transform *Transform
	inStream  uint32
	outStream uint32

	width         int
	height        int
	stride        int
	surfaceHeight int
	offsetX       int
	offsetY       int

	providesSamples bool
	outSize         int
	outAlign        int

	nv12  []byte
	ready []*DecodedPicture

	started bool
	drained bool
	time    int64

	thread *comThread
}

type DecoderOptions struct {
	HardwareTransform bool
	Direct3D          bool
}

func (d *Decoder) Name() string { return d.name }

func (d *Decoder) Hardware() bool { return d.hardware }

func (d *Decoder) Direct3D() bool { return d.direct3D }

func (d *Decoder) Accelerated() bool { return d.direct3D && d.sawTextures }

func (d *Decoder) FeatureLevel() uint32 {
	if d.manager == nil || d.manager.device == nil {
		return 0
	}
	return d.manager.device.featureLevel
}

func OpenDecoder(hardwareOnly bool) (*Decoder, error) {
	return OpenDecoderWithOptions(DecoderOptions{HardwareTransform: hardwareOnly})
}

func OpenDecoderWithOptions(opt DecoderOptions) (*Decoder, error) {
	thread := newCOMThread()
	var dec *Decoder
	var err error
	thread.run(func() { dec, err = openDecoderHere(opt) })
	if err != nil {
		thread.stop()
		return nil, err
	}
	dec.thread = thread
	return dec, nil
}

func openDecoderHere(opt DecoderOptions) (*Decoder, error) {
	if err := Startup(); err != nil {
		return nil, err
	}
	inInfo, _ := H264DecoderTypes()
	accept := func(t TransformDescription) bool { return !opt.HardwareTransform || t.Hardware }
	transform, chosen, err := openTransform(MFTCategoryVideoDecoder, &inInfo, nil, accept)
	if err != nil {
		Shutdown()
		return nil, err
	}
	d := &Decoder{name: chosen.Name, hardware: chosen.Hardware, transform: transform}
	if err := d.configure(opt); err != nil {
		d.closeHere()
		return nil, err
	}
	return d, nil
}

func (d *Decoder) isDirect3DAware() bool {
	attrs, err := d.transform.Attributes()
	if err != nil {
		return false
	}
	defer attrs.release()
	v, ok := attributeUint32(attrs, &MFSAD3D11Aware)
	return ok && v != 0
}

func (d *Decoder) attachDirect3D() error {
	if !d3d11Available() {
		return &Error{Op: "d3d11.dll is not present", Code: sOK}
	}
	if !d.isDirect3DAware() {
		return &Error{Op: "the decoder transform is not Direct3D 11 aware", Code: sOK}
	}
	manager, err := newDeviceManager()
	if err != nil {
		return err
	}
	if err := d.transform.ProcessMessage(MFTMessageSetD3DManager, uintptr(manager.obj.p)); err != nil {
		manager.release()
		return err
	}
	d.manager = manager
	d.reader = newTextureReader(manager.device)
	d.direct3D = true
	return nil
}

func (d *Decoder) configure(opt DecoderOptions) error {
	if err := d.transform.Unlock(); err != nil {
		return err
	}
	if d.transform.IsAsync() {
		return &Error{Op: "asynchronous decoder transforms are not driven yet", Code: sOK}
	}
	if opt.Direct3D {
		if err := d.attachDirect3D(); err != nil {
			return err
		}
	}

	in, out, err := d.transform.StreamIDs()
	if err != nil {
		return err
	}
	d.inStream, d.outStream = in, out

	inType, err := NewMediaType()
	if err != nil {
		return err
	}
	defer inType.Release()
	if err := inType.SetGUID(&MFMTMajorType, MFMediaTypeVideo); err != nil {
		return err
	}
	if err := inType.SetGUID(&MFMTSubtype, MFVideoFormatH264); err != nil {
		return err
	}
	if err := inType.SetUint32(&MFMTInterlaceMode, MFVideoInterlaceProgressive); err != nil {
		return err
	}
	if err := d.transform.SetInputType(d.inStream, inType.obj.p, 0); err != nil {
		return err
	}
	if err := d.selectOutputType(); err != nil {
		return err
	}
	return d.transform.ProcessMessage(MFTMessageNotifyBeginStreaming, 0)
}

func (d *Decoder) selectOutputType() error {
	for i := uint32(0); ; i++ {
		raw, code := d.transform.OutputAvailableType(d.outStream, i)
		if code == MFENoMoreTypes {
			return &Error{Op: "the decoder offers no planar output type", Code: MFENoMoreTypes}
		}
		if err := check("IMFTransform::GetOutputAvailableType", code); err != nil {
			return err
		}
		candidate := &MediaType{obj: unknown{raw}}
		subtype, err := candidate.GUID(&MFMTSubtype)
		if err != nil || subtype != MFVideoFormatNV12 {
			candidate.Release()
			continue
		}
		err = d.transform.SetOutputType(d.outStream, candidate.obj.p, 0)
		candidate.Release()
		if err != nil {
			return err
		}
		return d.readOutputGeometry()
	}
}

func (d *Decoder) readOutputGeometry() error {
	raw, err := d.transform.OutputCurrentType(d.outStream)
	if err != nil {
		return err
	}
	current := &MediaType{obj: unknown{raw}}
	defer current.Release()

	w, h, err := current.Size(&MFMTFrameSize)
	if err != nil {
		return err
	}
	d.width, d.height = int(w), int(h)
	d.surfaceHeight = int(h)
	d.offsetX, d.offsetY = 0, 0
	if area, ok := current.VideoArea(&MFMTMinimumDisplayAperture); ok {
		d.applyDisplayAperture(area)
	}
	d.stride = int(w)
	if v, err := current.Uint32(&MFMTDefaultStride); err == nil && int32(v) > 0 {
		d.stride = int(int32(v))
	}

	info, err := d.transform.OutputStreamInfo(d.outStream)
	if err != nil {
		return err
	}
	d.providesSamples = info.DwFlags&(outputStreamProvidesSamples|outputStreamCanProvideSamples) != 0
	d.outSize = int(info.CbSize)
	if d.outSize < NV12Size(d.stride, d.height) {
		d.outSize = NV12Size(d.stride, d.height)
	}
	d.outAlign = int(info.CbAlignment)
	d.nv12 = make([]byte, NV12Size(d.stride, d.surfaceHeight))
	return nil
}

func (d *Decoder) applyDisplayAperture(area VideoArea) {
	width, height := int(area.Width), int(area.Height)
	offsetX, offsetY := int(area.OffsetX.Value), int(area.OffsetY.Value)
	if width%2 != 0 || height%2 != 0 || offsetX < 0 || offsetY < 0 {
		return
	}
	if offsetX%2 != 0 || offsetY%2 != 0 {
		return
	}
	if offsetX+width > d.width || offsetY+height > d.surfaceHeight {
		return
	}
	d.width, d.height = width, height
	d.offsetX, d.offsetY = offsetX, offsetY
}

func (d *Decoder) copyNV12(dst []byte, src []byte, pitch, surfaceHeight int) {
	luma := d.offsetY*pitch + d.offsetX
	chroma := pitch*surfaceHeight + (d.offsetY/2)*pitch + d.offsetX
	if luma > len(src) || chroma < luma {
		return
	}
	NV12ToI420Offset(dst, src[luma:], pitch, chroma-luma, d.width, d.height)
}

func (d *Decoder) pushUnits(annexB []byte) error {
	sample, err := NewSample()
	if err != nil {
		return err
	}
	defer sample.Release()
	buf, err := NewMemoryBuffer(len(annexB))
	if err != nil {
		return err
	}
	defer buf.Release()
	if err := buf.Write(annexB); err != nil {
		return err
	}
	if err := sample.AddBuffer(buf); err != nil {
		return err
	}
	if err := sample.SetTime(d.time); err != nil {
		return err
	}
	d.time += defaultSampleSpacing
	for attempt := 0; attempt < 4096; attempt++ {
		code := d.transform.ProcessInput(d.inStream, sample.obj.p)
		if code != MFENotAccepting {
			return check("IMFTransform::ProcessInput", code)
		}
		if err := d.pullAll(); err != nil {
			return err
		}
	}
	return &Error{Op: "IMFTransform::ProcessInput kept refusing the stream", Code: MFENotAccepting}
}

func (d *Decoder) newPicture() *DecodedPicture {
	return &DecodedPicture{
		I420:   make([]byte, I420Size(d.width, d.height)),
		Width:  d.width,
		Height: d.height,
	}
}

func (d *Decoder) collectTexture(sample *Sample) (bool, error) {
	if d.reader == nil {
		return false, nil
	}
	pic := d.newPicture()
	need := d.offsetX + d.width
	ok, err := d.reader.withMappedNV12(sample, need, d.offsetY+d.height, func(src []byte, pitch, height int) {
		d.copyNV12(pic.I420, src, pitch, height)
	})
	if err != nil || !ok {
		return false, err
	}
	d.sawTextures = true
	d.ready = append(d.ready, pic)
	return true, nil
}

func (d *Decoder) collectSample(sample *Sample) error {
	if ok, err := d.collectTexture(sample); err != nil || ok {
		return err
	}
	buf, err := sample.ConvertToContiguousBuffer()
	if err != nil {
		return err
	}
	defer buf.Release()
	n, err := buf.Read(d.nv12)
	if err != nil {
		return err
	}
	if n < NV12Size(d.stride, d.surfaceHeight) {
		return nil
	}
	pic := d.newPicture()
	d.copyNV12(pic.I420, d.nv12, d.stride, d.surfaceHeight)
	d.ready = append(d.ready, pic)
	return nil
}

func (d *Decoder) pullOutput() (bool, error) {
	var buffers [1]MFTOutputDataBuffer
	buffers[0].DwStreamID = d.outStream

	var owned *Sample
	if !d.providesSamples {
		sample, err := NewSample()
		if err != nil {
			return false, err
		}
		buf, err := NewAlignedMemoryBuffer(d.outSize, d.outAlign)
		if err != nil {
			sample.Release()
			return false, err
		}
		if err := sample.AddBuffer(buf); err != nil {
			buf.Release()
			sample.Release()
			return false, err
		}
		buf.Release()
		owned = sample
		buffers[0].PSample = sample.obj.p
	}

	_, code := d.transform.ProcessOutput(buffers[:])
	produced := buffers[0].PSample

	switch {
	case code == MFETransformNeedMoreInput:
		owned.Release()
		return false, nil
	case code == MFETransformStreamChange:
		owned.Release()
		if err := d.selectOutputType(); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := check("IMFTransform::ProcessOutput", code); err != nil {
		owned.Release()
		return false, err
	}

	sample := owned
	if sample == nil {
		if produced == nil {
			return false, nil
		}
		sample = &Sample{obj: unknown{produced}}
	}
	defer sample.Release()
	if buffers[0].PEvents != nil {
		(unknown{buffers[0].PEvents}).release()
	}
	if err := d.collectSample(sample); err != nil {
		return false, err
	}
	return true, nil
}

func (d *Decoder) pullAll() error {
	for {
		more, err := d.pullOutput()
		if err != nil || !more {
			return err
		}
	}
}

func (d *Decoder) take() []*DecodedPicture {
	out := d.ready
	d.ready = nil
	return out
}

func (d *Decoder) decodeHere(annexB []byte) ([]*DecodedPicture, error) {
	if d.transform == nil {
		return nil, &Error{Op: "Decode on a closed decoder", Code: sOK}
	}
	if len(annexB) == 0 {
		return nil, nil
	}
	if !d.started || d.drained {
		if err := d.transform.ProcessMessage(MFTMessageNotifyStartOfStream, 0); err != nil {
			return nil, err
		}
		d.started = true
		d.drained = false
	}
	units := SplitAccessUnits(annexB)
	if len(units) == 0 {
		units = [][]byte{annexB}
	}
	for _, au := range units {
		if err := d.pushUnits(au); err != nil {
			return nil, err
		}
		if err := d.pullAll(); err != nil {
			return nil, err
		}
	}
	return d.take(), nil
}

func (d *Decoder) flushHere() ([]*DecodedPicture, error) {
	if d.transform == nil || d.drained {
		return nil, nil
	}
	d.drained = true
	if err := d.transform.ProcessMessage(MFTMessageNotifyEndOfStream, 0); err != nil {
		return nil, err
	}
	if err := d.transform.ProcessMessage(MFTMessageCommandDrain, 0); err != nil {
		return nil, err
	}
	if err := d.pullAll(); err != nil {
		return nil, err
	}
	return d.take(), nil
}

func (d *Decoder) closeHere() error {
	if d.transform == nil {
		return nil
	}
	_, flushErr := d.flushHere()
	d.transform.ProcessMessage(MFTMessageCommandFlush, 0)
	d.transform.ProcessMessage(MFTMessageNotifyEndStreaming, 0)
	if d.direct3D {
		d.transform.ProcessMessage(MFTMessageSetD3DManager, 0)
	}
	d.transform.Release()
	d.transform = nil
	d.reader.release()
	d.reader = nil
	d.manager.release()
	d.manager = nil
	d.ready = nil
	if err := Shutdown(); err != nil {
		return err
	}
	return flushErr
}

func (d *Decoder) Decode(annexB []byte) (out []*DecodedPicture, err error) {
	d.thread.run(func() { out, err = d.decodeHere(annexB) })
	return out, err
}

func (d *Decoder) Flush() (out []*DecodedPicture, err error) {
	d.thread.run(func() { out, err = d.flushHere() })
	return out, err
}

func (d *Decoder) Close() error {
	var err error
	d.thread.run(func() { err = d.closeHere() })
	d.thread.stop()
	d.thread = nil
	return err
}
