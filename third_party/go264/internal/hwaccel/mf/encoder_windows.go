package mf

import "time"

const hundredNanosecondsPerSecond = 10000000

const asyncSettle = 100 * time.Millisecond

type Encoder struct {
	name     string
	hardware bool
	async    bool

	transform *Transform
	gen       *eventGenerator
	inStream  uint32
	outStream uint32

	width  int
	height int
	stride int
	nv12   []byte

	providesSamples bool
	outSize         int
	outAlign        int

	frameDuration int64
	nextTime      int64

	credit            int
	ready             [][]byte
	drained           bool
	drainAcknowledged bool

	thread *comThread
}

func (e *Encoder) Name() string { return e.name }

func (e *Encoder) Hardware() bool { return e.hardware }

func OpenEncoder(f EncoderFormat, wantHardware bool) (*Encoder, error) {
	if f.Width <= 0 || f.Height <= 0 || f.Width%2 != 0 || f.Height%2 != 0 {
		return nil, &Error{Op: "OpenEncoder needs even positive dimensions", Code: sOK}
	}
	if f.FPSNum <= 0 || f.FPSDen <= 0 {
		return nil, &Error{Op: "OpenEncoder needs a positive frame rate", Code: sOK}
	}
	thread := newCOMThread()
	var enc *Encoder
	var err error
	thread.run(func() { enc, err = openEncoderHere(f, wantHardware) })
	if err != nil {
		thread.stop()
		return nil, err
	}
	enc.thread = thread
	return enc, nil
}

func openEncoderHere(f EncoderFormat, wantHardware bool) (*Encoder, error) {
	if err := Startup(); err != nil {
		return nil, err
	}

	_, outInfo := H264EncoderTypes()
	accept := func(d TransformDescription) bool { return !wantHardware || d.Hardware }
	transform, chosen, err := openTransform(MFTCategoryVideoEncoder, nil, &outInfo, accept)
	if err != nil {
		Shutdown()
		return nil, err
	}

	e := &Encoder{
		name:      chosen.Name,
		hardware:  chosen.Hardware,
		transform: transform,
		width:     f.Width,
		height:    f.Height,
		stride:    f.Width,
	}
	if err := e.configure(f); err != nil {
		e.closeHere()
		return nil, err
	}
	return e, nil
}

func (e *Encoder) configure(f EncoderFormat) error {
	if err := e.transform.Unlock(); err != nil {
		return err
	}
	e.async = e.transform.IsAsync()

	in, out, err := e.transform.StreamIDs()
	if err != nil {
		return err
	}
	e.inStream, e.outStream = in, out

	if f.QP > 0 {
		if err := e.transform.setConstantQuantiser(f.QP); err != nil {
			return err
		}
	}

	outType, err := f.OutputType()
	if err != nil {
		return err
	}
	defer outType.Release()
	if err := e.transform.SetOutputType(e.outStream, outType.obj.p, 0); err != nil {
		return err
	}

	inType, err := f.InputType()
	if err != nil {
		return err
	}
	defer inType.Release()
	if err := e.transform.SetInputType(e.inStream, inType.obj.p, 0); err != nil {
		return err
	}

	info, err := e.transform.OutputStreamInfo(e.outStream)
	if err != nil {
		return err
	}
	e.providesSamples = info.DwFlags&(outputStreamProvidesSamples|outputStreamCanProvideSamples) != 0
	e.outSize = int(info.CbSize)
	if e.outSize == 0 {
		e.outSize = e.width * e.height * 3 / 2
	}
	e.outAlign = int(info.CbAlignment)

	if e.async {
		gen, err := e.transform.eventGenerator()
		if err != nil {
			return err
		}
		e.gen = gen
	}

	e.nv12 = make([]byte, NV12Size(e.stride, e.height))
	e.frameDuration = int64(f.FPSDen) * hundredNanosecondsPerSecond / int64(f.FPSNum)

	if err := e.transform.ProcessMessage(MFTMessageNotifyBeginStreaming, 0); err != nil {
		return err
	}
	return e.transform.ProcessMessage(MFTMessageNotifyStartOfStream, 0)
}

func (e *Encoder) newInputSample(i420 []byte) (*Sample, error) {
	I420ToNV12(e.nv12, e.stride, i420, e.width, e.height)
	sample, err := NewSample()
	if err != nil {
		return nil, err
	}
	buf, err := NewMemoryBuffer(len(e.nv12))
	if err != nil {
		sample.Release()
		return nil, err
	}
	defer buf.Release()
	if err := buf.Write(e.nv12); err != nil {
		sample.Release()
		return nil, err
	}
	if err := sample.AddBuffer(buf); err != nil {
		sample.Release()
		return nil, err
	}
	if err := sample.SetTime(e.nextTime); err != nil {
		sample.Release()
		return nil, err
	}
	if err := sample.SetDuration(e.frameDuration); err != nil {
		sample.Release()
		return nil, err
	}
	e.nextTime += e.frameDuration
	return sample, nil
}

func (e *Encoder) pushFrame(i420 []byte) error {
	sample, err := e.newInputSample(i420)
	if err != nil {
		return err
	}
	defer sample.Release()
	code := e.transform.ProcessInput(e.inStream, sample.obj.p)
	return check("IMFTransform::ProcessInput", code)
}

func (e *Encoder) pullOutput() error {
	var buffers [1]MFTOutputDataBuffer
	buffers[0].DwStreamID = e.outStream

	var owned *Sample
	if !e.providesSamples {
		sample, err := NewSample()
		if err != nil {
			return err
		}
		buf, err := NewAlignedMemoryBuffer(e.outSize, e.outAlign)
		if err != nil {
			sample.Release()
			return err
		}
		if err := sample.AddBuffer(buf); err != nil {
			buf.Release()
			sample.Release()
			return err
		}
		buf.Release()
		owned = sample
		buffers[0].PSample = sample.obj.p
	}

	_, code := e.transform.ProcessOutput(buffers[:])
	produced := buffers[0].PSample

	if code == MFETransformNeedMoreInput || code == MFETransformStreamChange {
		owned.Release()
		return nil
	}

	if err := check("IMFTransform::ProcessOutput", code); err != nil {
		owned.Release()
		return err
	}

	sample := owned
	if sample == nil {
		if produced == nil {
			return nil
		}
		sample = &Sample{obj: unknown{produced}}
	}
	defer sample.Release()
	if buffers[0].PEvents != nil {
		(unknown{buffers[0].PEvents}).release()
	}

	buf, err := sample.ConvertToContiguousBuffer()
	if err != nil {
		return err
	}
	defer buf.Release()
	n, err := buf.CurrentLength()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	out := make([]byte, n)
	if _, err := buf.Read(out); err != nil {
		return err
	}
	e.ready = append(e.ready, out)
	return nil
}

func (e *Encoder) pumpEvent(wait bool) (bool, error) {
	ev, ok, err := e.gen.next(wait)
	if err != nil || !ok {
		return false, err
	}
	switch ev.kind {
	case transformEventNeedInput:
		e.credit++
	case transformEventHaveOutput:
		if err := e.pullOutput(); err != nil {
			return false, err
		}
	case transformEventDrainComplete:
		e.drainAcknowledged = true
	}
	return true, nil
}

func (e *Encoder) drainQueuedEvents() error {
	for {
		more, err := e.pumpEvent(false)
		if err != nil || !more {
			return err
		}
	}
}

func (e *Encoder) take() [][]byte {
	out := e.ready
	e.ready = nil
	return out
}

func (e *Encoder) encodeHere(i420 []byte) ([][]byte, error) {
	if e.transform == nil {
		return nil, &Error{Op: "Encode on a closed encoder", Code: sOK}
	}
	if len(i420) < I420Size(e.width, e.height) {
		return nil, &Error{Op: "Encode was given a short frame", Code: sOK}
	}
	if !e.async {
		if err := e.pushFrame(i420); err != nil {
			return nil, err
		}
		for {
			before := len(e.ready)
			if err := e.pullOutput(); err != nil {
				return nil, err
			}
			if len(e.ready) == before {
				break
			}
		}
		return e.take(), nil
	}

	for e.credit == 0 {
		if _, err := e.pumpEvent(true); err != nil {
			return nil, err
		}
	}
	e.credit--
	if err := e.pushFrame(i420); err != nil {
		return nil, err
	}
	if err := e.drainQueuedEvents(); err != nil {
		return nil, err
	}
	return e.take(), nil
}

func (e *Encoder) drainHere() ([][]byte, error) {
	if e.transform == nil || e.drained {
		return nil, nil
	}
	if e.async && e.gen == nil {
		return nil, nil
	}
	e.drained = true
	if err := e.transform.ProcessMessage(MFTMessageNotifyEndOfStream, 0); err != nil {
		return nil, err
	}
	if err := e.transform.ProcessMessage(MFTMessageCommandDrain, 0); err != nil {
		return nil, err
	}
	if !e.async {
		for {
			before := len(e.ready)
			if err := e.pullOutput(); err != nil {
				return nil, err
			}
			if len(e.ready) == before {
				break
			}
		}
		return e.take(), nil
	}
	for !e.drainAcknowledged {
		more, err := e.pumpEvent(true)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
	}
	return e.take(), nil
}

func (e *Encoder) discardPendingEvents() {
	if e.gen == nil {
		return
	}
	for i := 0; i < 1024; i++ {
		if _, ok, err := e.gen.next(false); err != nil || !ok {
			return
		}
	}
}

func (e *Encoder) closeHere() error {
	if e.transform == nil {
		return nil
	}
	if e.gen != nil && e.gen.abandoned {
		e.gen, e.ready, e.transform = nil, nil, nil
		return nil
	}
	_, drainErr := e.drainHere()
	if e.gen != nil && e.gen.abandoned {
		e.gen, e.ready, e.transform = nil, nil, nil
		return drainErr
	}
	e.transform.ProcessMessage(MFTMessageCommandFlush, 0)
	e.discardPendingEvents()
	e.transform.ProcessMessage(MFTMessageNotifyEndOfStream, 0)
	e.transform.ProcessMessage(MFTMessageNotifyEndStreaming, 0)
	if e.gen != nil {
		time.Sleep(asyncSettle)
		e.gen.shutdownTransform()
	}
	e.gen.release()
	e.gen = nil
	e.ready = nil
	e.transform.Release()
	e.transform = nil
	if err := Shutdown(); err != nil {
		return err
	}
	return drainErr
}

func (e *Encoder) Encode(i420 []byte) (out [][]byte, err error) {
	e.thread.run(func() { out, err = e.encodeHere(i420) })
	return out, err
}

func (e *Encoder) Drain() (out [][]byte, err error) {
	e.thread.run(func() { out, err = e.drainHere() })
	return out, err
}

func (e *Encoder) Close() error {
	var err error
	e.thread.run(func() { err = e.closeHere() })
	e.thread.stop()
	e.thread = nil
	return err
}
