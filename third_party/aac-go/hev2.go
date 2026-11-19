package aac

// HE-AAC v1 and v2: the AudioSpecificConfig, raw access units, and running
// SBR (sbr.go) and PS (ps.go) after the core decoder. Written for
// CloudBurrow from ISO/IEC 14496-3 (#1739).

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/aac-go/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/aac-go/internal/tables"
)

// heState is the decoder's SBR and PS state.
type heState struct {
	sbrOn   bool // SBR is decoded: signalled, or found in the stream
	noSBR   bool // the AudioSpecificConfig rules SBR out
	sbrRate int  // the SBR (output) rate
	// downsampled: the ASC sets the SBR rate to the core's, so SBR runs
	// at twice the core rate and a 32-band synthesis bank gives samples at
	// the core rate (downsampled SBR).
	downsampled bool
	sbrElems    []*sbrElement
	sbrHit      []bool // per element: this frame carried SBR data
}

// frameElement is one SCE or CPE of a raw_data_block.
type frameElement struct {
	first  int // its first channel
	stereo bool
	lfe    bool
}

// ASC is the part of an MPEG-4 AudioSpecificConfig (ISO/IEC 14496-3
// §1.6.2.1) the decoder uses.
type ASC struct {
	ObjectType             int // the core's audio object type (2 for AAC-LC)
	SamplingFrequencyIndex int
	SamplingFrequency      int
	ChannelConfiguration   int
	// SBR is 1 when signalled present, 0 when signalled absent, and -1
	// when not signalled (implicit: found in the stream or not).
	SBR int
	// PS likewise.
	PS int
	// ExtensionSamplingFrequency is the SBR rate when SBR is signalled.
	ExtensionSamplingFrequency int
	FrameLengthFlag            bool
}

// ParseASC parses an AudioSpecificConfig.
func ParseASC(asc []byte) (*ASC, error) {
	b := &bitr{r: bits.NewReader(asc)}
	objectType := func() int {
		t := b.u(5)
		if t == 31 {
			t = 32 + b.u(6)
		}
		return t
	}
	freq := func() (int, int) {
		i := b.u(4)
		if i == 15 {
			f := b.u(24)
			return i, f
		}
		f, _ := tables.SamplingFrequency(i)
		return i, f
	}
	a := &ASC{SBR: -1, PS: -1}
	a.ObjectType = objectType()
	a.SamplingFrequencyIndex, a.SamplingFrequency = freq()
	a.ChannelConfiguration = b.u(4)
	if a.ObjectType == 5 || a.ObjectType == 29 {
		a.SBR = 1
		if a.ObjectType == 29 {
			a.PS = 1
		}
		_, a.ExtensionSamplingFrequency = freq()
		a.ObjectType = objectType()
		if a.ObjectType == 22 {
			b.u(4)
		}
	}
	if b.err != nil {
		return nil, fmt.Errorf("aac: AudioSpecificConfig: %w", b.err)
	}
	if a.ObjectType != 2 {
		return a, unsupportedf("audio object type %d (AAC-LC, with SBR and PS, only)", a.ObjectType)
	}
	// GASpecificConfig
	a.FrameLengthFlag = b.u(1) == 1
	if b.u(1) == 1 { // dependsOnCoreCoder
		b.u(14)
	}
	b.u(1) // extensionFlag
	if a.ChannelConfiguration == 0 {
		return a, unsupportedf("channel_configuration 0 (a program_config_element)")
	}
	// The backward-compatible SBR and PS signalling.
	if a.SBR == -1 && b.err == nil && b.r.Remaining() >= 16 {
		if b.u(11) == 0x2b7 {
			if ext := objectType(); ext == 5 {
				a.SBR = b.u(1)
				if a.SBR == 1 {
					_, a.ExtensionSamplingFrequency = freq()
					if b.err == nil && b.r.Remaining() >= 12 && b.u(11) == 0x548 {
						a.PS = b.u(1)
					}
				}
			}
		}
	}
	if b.err != nil {
		return nil, fmt.Errorf("aac: AudioSpecificConfig: %w", b.err)
	}
	return a, nil
}

// NewDecoderASC makes a decoder for raw access units (raw_data_block()s,
// as MP4 carries them) described by an MPEG-4 AudioSpecificConfig: AAC-LC,
// with SBR (HE-AAC) and PS (HE-AAC v2), signalled explicitly or found in
// the stream.
func NewDecoderASC(asc []byte) (*Decoder, error) {
	a, err := ParseASC(asc)
	if err != nil {
		return nil, err
	}
	if a.FrameLengthFlag {
		return nil, unsupportedf("960-sample frames")
	}
	d, _ := NewDecoder()
	if err := d.configureCore(a.SamplingFrequencyIndex, a.ChannelConfiguration); err != nil {
		return nil, err
	}
	if a.SBR == 1 && a.ChannelConfiguration > 2 {
		return nil, unsupportedf("SBR over channel_configuration %d", a.ChannelConfiguration)
	}
	switch a.SBR {
	case 1:
		d.sbrOn = true
		d.sbrRate = a.ExtensionSamplingFrequency
		switch {
		case d.sbrRate == d.sampleRate:
			d.downsampled = true
		case d.sbrRate != 2*d.sampleRate:
			return nil, unsupportedf("SBR at %d Hz over a %d Hz core", d.sbrRate, d.sampleRate)
		}
	case 0:
		d.noSBR = true
	}
	return d, nil
}

// DecodeAU decodes one raw access unit to interleaved samples, a full-scale
// 16-bit sample being 1.0.
func (d *Decoder) DecodeAU(au []byte) ([]float32, error) {
	if !d.configured {
		return nil, fmt.Errorf("aac: DecodeAU needs a decoder from NewDecoderASC")
	}
	chans, err := d.decodeFrame(au)
	if err != nil {
		return nil, err
	}
	n := len(chans)
	out := make([]float32, len(chans[0])*n)
	for c, pcm := range chans {
		for i, v := range pcm {
			out[i*n+c] = float32(v / 32768)
		}
	}
	return out, nil
}

// OutputSampleRate is the rate of the decoded samples: the SBR rate when
// SBR is decoded.
func (d *Decoder) OutputSampleRate() int {
	if d.sbrOn {
		return d.sbrRate
	}
	return d.sampleRate
}

// OutputChannels is the number of decoded channels: two for a mono SBR
// stream, which may carry parametric stereo, as ffmpeg decodes it.
func (d *Decoder) OutputChannels() int {
	if d.sbrOn && d.channels == 1 {
		return 2
	}
	return d.channels
}

// readSBR decodes an SBR extension payload for the last element.
func (d *Decoder) readSBR(r *bits.BitReader, elems []frameElement, crc bool, payloadBits int) error {
	if d.channels > 2 {
		return unsupportedf("SBR over %d channels", d.channels)
	}
	if !d.sbrOn {
		// Implicit signalling: SBR found in the stream doubles the rate.
		d.sbrOn = true
		d.sbrRate = 2 * d.sampleRate
	}
	i := len(elems) - 1
	for len(d.sbrElems) <= i {
		d.sbrElems = append(d.sbrElems, nil)
		d.sbrHit = append(d.sbrHit, false)
	}
	e := d.sbrElems[i]
	if e == nil || e.stereo != elems[i].stereo {
		e = newSBRElement(d.sbrTableRate(), elems[i].stereo)
		d.sbrElems[i] = e
	}
	if err := e.parseExtension(r, crc, payloadBits); err != nil {
		return err
	}
	d.sbrHit[i] = true
	return nil
}

// sbrTableRate is the rate SBR's frequency tables are for: twice the
// core's, downsampled or not.
func (d *Decoder) sbrTableRate() int { return 2 * d.sampleRate }

// synth runs a channel's synthesis bank: 64 bands to 2048 samples, or, for
// downsampled SBR, 32 bands to 1024.
func (d *Decoder) synth(c *sbrChannel, x *[sbrSlots][64]complex128) []float64 {
	if d.downsampled {
		o := make([]float64, SamplesPerFrame)
		c.synthesis32(x, o)
		return o
	}
	o := make([]float64, 2*SamplesPerFrame)
	c.synthesis(x, o)
	return o
}

// applySBR runs SBR and PS over a frame's channels.
func (d *Decoder) applySBR(pcm [][]float64, elems []frameElement) [][]float64 {
	if !d.sbrOn {
		return pcm
	}
	var out [][]float64
	for i, el := range elems {
		for len(d.sbrElems) <= i {
			d.sbrElems = append(d.sbrElems, nil)
			d.sbrHit = append(d.sbrHit, false)
		}
		e := d.sbrElems[i]
		if e == nil {
			e = newSBRElement(d.sbrTableRate(), el.stereo)
			d.sbrElems[i] = e
		}
		hit := d.sbrHit[i]
		d.sbrHit[i] = false
		if el.stereo {
			for c := range 2 {
				x := e.process(c, pcm[el.first+c], hit)
				out = append(out, d.synth(&e.ch[c], x))
			}
			continue
		}
		x := e.process(0, pcm[el.first], hit)
		var l, r []float64
		if e.ps != nil {
			xl, xr := e.ps.apply(x, e.ch[0].ahead(), hit && e.psHit)
			l = d.synth(&e.ch[0], xl)
			r = d.synth(&e.ch[1], xr)
		} else {
			l = d.synth(&e.ch[0], x)
			r = append([]float64(nil), l...)
		}
		out = append(out, l)
		if len(elems) == 1 {
			out = append(out, r)
		}
	}
	return out
}
