package embedding

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/mp4"
	aacdec "github.com/arabian9ts/aac-go"
)

// A video's audio track (#1732), for embed_content_config's
// audio_track_extraction. Google's multimodal embeddings page documents it
// as "Extract audio from video input", off by default, the audio counting
// against the 180 seconds of audio a request. EmbeddingGemma 2's processor
// (Transformers' load_audio_from_video) loads a video's audio as a clip of
// its own, placed right after the video's frames; the clip is decoded as an
// audio part is (mono, resampled to 16 kHz) and embedded the same way.
//
// The track is read from the same MP4 or MOV with github.com/Eyevinn/mp4ff
// and decoded as ffmpeg decodes it: the samples of the first sound track, the
// edit list's leading media time (an AAC encoder's priming) dropped and its
// duration kept. The codecs: AAC-LC (mp4a, the audio of nearly every MP4 and
// MOV) with github.com/arabian9ts/aac-go (pure Go, Apache 2.0), MP3 (mp4a
// with the MPEG-1/2 audio object types, or .mp3) with third_party/go-mp3, and
// QuickTime linear PCM (lpcm, sowt, twos, in24, in32, fl32, fl64); the audio
// formats Google lists are MP3 and WAV. Other codecs (Opus, AC-3, ALAC,
// HE-AAC...) are refused naming them.

// DecodeVideoAudio decodes the audio track of an MP4 or MOV video to mono
// samples at AudioSampleRate, between opt's start and end offsets. A video
// with no sound track gives nil and no error.
func DecodeVideoAudio(mimeType string, data []byte, opt VideoOptions) (samples []float32, err error) {
	if !videoMIMETypes[baseMIME(mimeType)] {
		return nil, fmt.Errorf("mime_type %q is not supported; send one of %s", mimeType, joinTypes(VideoMIMETypes))
	}
	defer func() {
		if r := recover(); r != nil {
			samples, err = nil, fmt.Errorf("the video's audio track: %v", r)
		}
	}()
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil || f.Moov == nil {
		return nil, fmt.Errorf("the data is not a valid MP4 or MOV video")
	}
	var trak *mp4.TrakBox
	for _, t := range f.Moov.Traks {
		if t.Mdia != nil && t.Mdia.Hdlr != nil && t.Mdia.Hdlr.HandlerType == "soun" {
			trak = t
			break
		}
	}
	if trak == nil {
		return nil, nil
	}
	mono, rate, err := decodeSoundTrack(f, trak, data)
	if err != nil {
		var long *tooLongError
		if errors.As(err, &long) {
			return nil, err
		}
		return nil, fmt.Errorf("the video's audio track: %v", err)
	}
	// The interval video_metadata asks for.
	from := int(math.Round(opt.Start.Seconds() * float64(rate)))
	to := len(mono)
	if opt.End > 0 {
		to = min(to, int(math.Round(opt.End.Seconds()*float64(rate))))
	}
	if from >= to {
		return nil, nil
	}
	mono = mono[from:to]
	if secs := float64(len(mono)) / float64(rate); secs > MaxAudioSeconds {
		return nil, &tooLongError{secs}
	}
	if rate != AudioSampleRate {
		mono = resample(mono, rate, AudioSampleRate)
	}
	return mono, nil
}

func joinTypes(t []string) string {
	var b bytes.Buffer
	for i, s := range t {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(s)
	}
	return b.String()
}

// soundTrack is a sound track's samples and how to read them.
type soundTrack struct {
	timescale uint32
	samples   [][]byte
	// shift is the edit list's first media time, and keep its duration in
	// the media's timescale (0: to the end).
	shift, keep int64
}

// decodeSoundTrack decodes a sound track to mono samples at its own rate.
func decodeSoundTrack(f *mp4.File, trak *mp4.TrakBox, data []byte) ([]float32, int, error) {
	md := trak.Mdia
	if md.Mdhd == nil || md.Mdhd.Timescale == 0 || md.Minf == nil || md.Minf.Stbl == nil ||
		md.Minf.Stbl.Stsd == nil || len(md.Minf.Stbl.Stsd.Children) == 0 {
		return nil, 0, fmt.Errorf("the track has no media header or sample description")
	}
	st := md.Minf.Stbl
	tr := &soundTrack{timescale: md.Mdhd.Timescale}
	if trak.Edts != nil && len(trak.Edts.Elst) > 0 {
		for _, e := range trak.Edts.Elst[0].Entries {
			if e.MediaTime < 0 {
				continue // an empty edit: a delay before the audio starts
			}
			tr.shift = e.MediaTime
			if mvhd := f.Moov.Mvhd; mvhd != nil && mvhd.Timescale > 0 && e.SegmentDuration > 0 {
				tr.keep = int64(math.Round(float64(e.SegmentDuration) * float64(tr.timescale) / float64(mvhd.Timescale)))
			}
			break
		}
	}
	add := func(_ int64, _ uint32, _ int32, _ bool, sample []byte) { tr.samples = append(tr.samples, sample) }
	if err := progressiveSamples(st, data, add); err != nil {
		return nil, 0, err
	}
	if f.IsFragmented() {
		var trex *mp4.TrexBox
		if f.Moov.Mvex != nil {
			trex, _ = f.Moov.Mvex.GetTrex(trak.Tkhd.TrackID)
		}
		if trex == nil {
			trex = &mp4.TrexBox{TrackID: trak.Tkhd.TrackID}
		}
		for _, seg := range f.Segments {
			for _, frag := range seg.Fragments {
				fs, err := frag.GetFullSamples(trex)
				if err != nil {
					return nil, 0, fmt.Errorf("a fragment: %v", err)
				}
				for _, s := range fs {
					tr.samples = append(tr.samples, s.Data)
				}
			}
		}
	}
	if len(tr.samples) == 0 {
		return nil, 0, fmt.Errorf("the track has no samples")
	}

	var (
		pcm      []float32 // interleaved
		channels int
		rate     int
		err      error
	)
	entry := st.Stsd.Children[0]
	switch entry.Type() {
	case "mp4a":
		ase := st.Stsd.Mp4a
		if ase == nil || ase.Esds == nil || ase.Esds.DecConfigDescriptor == nil {
			return nil, 0, fmt.Errorf("the mp4a track has no esds")
		}
		dcd := ase.Esds.DecConfigDescriptor
		switch dcd.ObjectType {
		case 0x40, 0x66, 0x67, 0x68: // MPEG-4 audio; MPEG-2 AAC Main, LC and SSR
			pcm, channels, rate, err = decodeAACTrack(tr, dcd)
		case 0x69, 0x6b: // MPEG-2 and MPEG-1 audio
			pcm, channels, rate, err = decodeMP3Track(tr)
		default:
			return nil, 0, fmt.Errorf("its codec (MPEG-4 object type 0x%02x) is not decoded; AAC-LC, MP3 and PCM audio tracks are", dcd.ObjectType)
		}
	case ".mp3":
		pcm, channels, rate, err = decodeMP3Track(tr)
	case "lpcm", "sowt", "twos", "in24", "in32", "fl32", "fl64":
		pcm, channels, rate, err = decodePCMTrack(tr, st.Stsd.QtPcm)
	default:
		return nil, 0, fmt.Errorf("its codec %q is not decoded; AAC-LC, MP3 and PCM audio tracks are", entry.Type())
	}
	if err != nil {
		return nil, 0, err
	}
	if channels < 1 || rate < 1 {
		return nil, 0, fmt.Errorf("no audio format")
	}
	frames := len(pcm) / channels
	// The edit list, in the track's timescale, which for audio is almost
	// always its sample rate.
	scale := float64(rate) / float64(tr.timescale)
	start := min(frames, int(math.Round(float64(tr.shift)*scale)))
	end := frames
	if tr.keep > 0 {
		end = min(end, start+int(math.Round(float64(tr.keep)*scale)))
	}
	if secs := float64(end-start) / float64(rate); secs > MaxAudioSeconds+1 {
		return nil, 0, &tooLongError{secs}
	}
	return downmix(pcm[start*channels:end*channels], channels), rate, nil
}

// decodeAACTrack decodes an AAC-LC track's access units, each wrapped in
// the ADTS header its AudioSpecificConfig gives.
func decodeAACTrack(tr *soundTrack, dcd *mp4.DecoderConfigDescriptor) (pcm []float32, channels, rate int, err error) {
	objectType := byte(aac.AAClc)
	var asc *aac.AudioSpecificConfig
	if dcd.ObjectType == 0x40 {
		if dcd.DecSpecificInfo == nil || len(dcd.DecSpecificInfo.DecConfig) == 0 {
			return nil, 0, 0, fmt.Errorf("the AAC track has no AudioSpecificConfig")
		}
		if asc, err = aac.DecodeAudioSpecificConfig(bytes.NewReader(dcd.DecSpecificInfo.DecConfig)); err != nil {
			return nil, 0, 0, fmt.Errorf("the AAC track's AudioSpecificConfig: %v", err)
		}
		switch {
		case asc.ObjectType == aac.HEAACv1 || asc.ObjectType == aac.HEAACv2 || asc.SBRPresentFlag || asc.PSPresentFlag:
			return nil, 0, 0, errors.New("it is HE-AAC (SBR), which is not decoded; AAC-LC, MP3 and PCM audio tracks are")
		case asc.ObjectType != aac.AAClc:
			return nil, 0, 0, fmt.Errorf("its AAC audio object type %d is not decoded; AAC-LC is", asc.ObjectType)
		case asc.FrameLengthFlag:
			return nil, 0, 0, errors.New("its AAC frames are 960 samples, which are not decoded")
		}
	} else if dcd.ObjectType != 0x67 {
		return nil, 0, 0, fmt.Errorf("its MPEG-2 AAC profile (object type 0x%02x) is not decoded; AAC-LC is", dcd.ObjectType)
	}
	if asc == nil {
		return nil, 0, 0, errors.New("the MPEG-2 AAC track has no AudioSpecificConfig")
	}
	if asc.ChannelConfiguration != 1 && asc.ChannelConfiguration != 2 {
		return nil, 0, 0, fmt.Errorf("its AAC channel configuration %d is not decoded; mono and stereo are", asc.ChannelConfiguration)
	}
	d, err := aacdec.NewDecoder()
	if err != nil {
		return nil, 0, 0, err
	}
	var out []int16
	for i, au := range tr.samples {
		h, err := aac.NewADTSHeader(asc.SamplingFrequency, asc.ChannelConfiguration, objectType, uint16(min(len(au), 1<<13)))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("the AAC track: %v", err)
		}
		if len(au)+7 >= 1<<13 {
			return nil, 0, 0, fmt.Errorf("the AAC track's access unit %d is %d bytes, too long for a frame", i, len(au))
		}
		frame := append(h.Encode(), au...)
		p, err := d.Decode(frame)
		if err != nil {
			if errors.Is(err, aacdec.ErrUnsupported) {
				return nil, 0, 0, fmt.Errorf("its AAC uses a feature that is not decoded (%v)", err)
			}
			return nil, 0, 0, fmt.Errorf("the AAC track's access unit %d: %v", i, err)
		}
		out = append(out, p...)
		if len(out) > (MaxAudioSeconds+60)*asc.SamplingFrequency*int(asc.ChannelConfiguration) {
			return nil, 0, 0, &tooLongError{float64(len(out)) / float64(asc.SamplingFrequency) / float64(asc.ChannelConfiguration)}
		}
	}
	pcm = make([]float32, len(out))
	for i, v := range out {
		pcm[i] = float32(v) / 32768
	}
	return pcm, int(asc.ChannelConfiguration), asc.SamplingFrequency, nil
}

// decodeMP3Track decodes an MP3 track's frames as one MP3 stream, without
// the trimming an Info frame asks for: the edit list stands for it.
func decodeMP3Track(tr *soundTrack) (pcm []float32, channels, rate int, err error) {
	stream := bytes.Join(tr.samples, nil)
	s, err := parseMP3Stream(stream)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("its MP3: %v", err)
	}
	interleaved, rate, err := decodeMP3PCM(stream)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("its MP3: %v", err)
	}
	if s.mono {
		mono := make([]float32, len(interleaved)/2)
		for i := range mono {
			mono[i] = interleaved[2*i]
		}
		return mono, 1, rate, nil
	}
	return interleaved, 2, rate, nil
}

// decodePCMTrack reads a QuickTime linear PCM track.
func decodePCMTrack(tr *soundTrack, e *mp4.AudioSampleEntryBox) (pcm []float32, channels, rate int, err error) {
	if e == nil {
		return nil, 0, 0, errors.New("the PCM track has no sample entry")
	}
	channels, rate = int(e.ChannelCount), int(e.SampleRate)
	bits := int(e.SampleSize)
	float, big := false, false
	switch e.Type() {
	case "twos":
		big = true
	case "sowt":
	case "in24":
		bits, big = 24, true
	case "in32":
		bits, big = 32, true
	case "fl32":
		bits, big, float = 32, true, true
	case "fl64":
		bits, big, float = 64, true, true
	case "lpcm":
		v2 := e.QuickTimeV2
		if v2 == nil {
			return nil, 0, 0, errors.New("the lpcm track has no version 2 sound description")
		}
		channels, rate, bits = int(v2.NumAudioChannels), int(math.Round(v2.AudioSampleRate)), int(v2.ConstBitsPerChannel)
		float = v2.FormatSpecificFlags&mp4.QuickTimeFormatFlagIsFloat != 0
		big = v2.FormatSpecificFlags&mp4.QuickTimeFormatFlagIsBigEndian != 0
	}
	// A version 1 description of twos or sowt may give the real sample size.
	if v1 := e.QuickTimeV1; v1 != nil && v1.BytesPerSample > 0 && (e.Type() == "twos" || e.Type() == "sowt") {
		bits = int(v1.BytesPerSample) * 8
	}
	if channels < 1 || channels > 8 || rate < 1 || rate > 192000 {
		return nil, 0, 0, fmt.Errorf("its PCM format (%d channels at %d Hz) is not decoded", channels, rate)
	}
	if !(bits == 8 || bits == 16 || bits == 24 || bits == 32 || (float && bits == 64)) || (float && bits < 32) {
		return nil, 0, 0, fmt.Errorf("its %d-bit PCM is not decoded", bits)
	}
	raw := bytes.Join(tr.samples, nil)
	bps := bits / 8
	n := len(raw) / bps
	if n/channels > (MaxAudioSeconds+60)*rate {
		return nil, 0, 0, &tooLongError{float64(n/channels) / float64(rate)}
	}
	pcm = make([]float32, n)
	var order binary.ByteOrder = binary.LittleEndian
	if big {
		order = binary.BigEndian
	}
	for i := range pcm {
		b := raw[i*bps : (i+1)*bps]
		switch {
		case float && bits == 32:
			pcm[i] = math.Float32frombits(order.Uint32(b))
		case float:
			pcm[i] = float32(math.Float64frombits(order.Uint64(b)))
		case bits == 8:
			pcm[i] = float32(int8(b[0])) / 128 // QuickTime's 8-bit PCM is signed
		case bits == 16:
			pcm[i] = float32(int16(order.Uint16(b))) / 32768
		case bits == 24:
			v := int32(b[0])<<16 | int32(b[1])<<8 | int32(b[2])
			if !big {
				v = int32(b[2])<<16 | int32(b[1])<<8 | int32(b[0])
			}
			pcm[i] = float32(v<<8>>8) / (1 << 23)
		default:
			pcm[i] = float32(float64(int32(order.Uint32(b))) / (1 << 31))
		}
	}
	return pcm, channels, rate, nil
}
