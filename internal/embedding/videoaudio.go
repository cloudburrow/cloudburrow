package embedding

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/cloudburrow/cloudburrow/internal/ac3"
	aacdec "github.com/cloudburrow/cloudburrow/third_party/aac-go"
	"github.com/kazzmir/opus-go/opus"
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
// MOV) with github.com/arabian9ts/aac-go (pure Go, Apache 2.0; copied into
// third_party/aac-go, where #1739 added HE-AAC v1 and v2), MP3 (mp4a
// with the MPEG-1/2 audio object types, or .mp3) with third_party/go-mp3, and
// QuickTime linear PCM (lpcm, sowt, twos, in24, in32, fl32, fl64), and
// (#1739) Opus with github.com/kazzmir/opus-go (libopus transpiled to Go,
// BSD 3-Clause), and AC-3 and E-AC-3 with internal/ac3 (written from ATSC
// A/52); the audio formats Google lists are MP3 and WAV. Other codecs
// (ALAC, FLAC...) are refused naming them.

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
		return nil, fmt.Errorf("the video's audio track: %w", err)
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
		layout   []string // the channels' names, where the codec fixes them
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
			pcm, channels, rate, layout, err = decodeAACTrack(tr, dcd)
		case 0x69, 0x6b: // MPEG-2 and MPEG-1 audio
			pcm, channels, rate, err = decodeMP3Track(tr)
		case 0xa5, 0xa6: // AC-3 and E-AC-3 (ISO/IEC 14496-1's registered object types)
			pcm, rate, err = decodeAC3Track(tr)
			channels = 1
		default:
			return nil, 0, notDecodedf("its codec (MPEG-4 object type 0x%02x) is not decoded; AAC, MP3, Opus, AC-3, E-AC-3, FLAC, ALAC, G.711 and PCM audio tracks are", dcd.ObjectType)
		}
	case ".mp3":
		pcm, channels, rate, err = decodeMP3Track(tr)
	case "lpcm", "sowt", "twos", "in24", "in32", "fl32", "fl64":
		pcm, channels, rate, err = decodePCMTrack(tr, st.Stsd.QtPcm)
	case "ulaw", "alaw":
		ase, _ := entry.(*mp4.AudioSampleEntryBox)
		pcm, channels, rate, err = decodeG711Track(tr, ase)
	case "fLaC":
		ase, _ := entry.(*mp4.AudioSampleEntryBox)
		pcm, channels, rate, layout, err = decodeFLACTrack(tr, ase)
	case "alac":
		pcm, channels, rate, layout, err = decodeALACTrack(tr, st.Stsd.Alac)
	case "Opus":
		pcm, channels, rate, err = decodeOpusTrack(tr, st.Stsd.Opus, trak.Edts != nil && len(trak.Edts.Elst) > 0)
	case "ac-3", "ec-3":
		pcm, rate, err = decodeAC3Track(tr)
		channels = 1
	default:
		return nil, 0, notDecodedf("its codec %q is not decoded; AAC, MP3, Opus, AC-3, E-AC-3, FLAC, ALAC, G.711 and PCM audio tracks are", entry.Type())
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
	if layout != nil {
		mono, err := downmixNamed(pcm[start*channels:end*channels], layout)
		return mono, rate, err
	}
	return downmix(pcm[start*channels:end*channels], channels), rate, nil
}

// decodeAACTrack decodes an AAC track's raw access units with
// third_party/aac-go: AAC-LC, and (#1739) HE-AAC, SBR (v1) and SBR with
// parametric stereo (v2), signalled in the AudioSpecificConfig or only in
// the stream, as ffmpeg decodes them (SBR at twice the core rate, PS to
// stereo).
//
// More than two channels (#1651), channel configurations 3 to 6 or a
// program_config_element (configuration 0, as ffmpeg writes 7.1), with SBR
// or without, come with their speakers' names for the downmix.
func decodeAACTrack(tr *soundTrack, dcd *mp4.DecoderConfigDescriptor) (pcm []float32, channels, rate int, layout []string, err error) {
	if dcd.ObjectType != 0x40 && dcd.ObjectType != 0x67 {
		return nil, 0, 0, nil, notDecodedf("its MPEG-2 AAC profile (object type 0x%02x) is not decoded; AAC-LC is", dcd.ObjectType)
	}
	if dcd.DecSpecificInfo == nil || len(dcd.DecSpecificInfo.DecConfig) == 0 {
		return nil, 0, 0, nil, fmt.Errorf("the AAC track has no AudioSpecificConfig")
	}
	asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(dcd.DecSpecificInfo.DecConfig))
	if err != nil {
		return nil, 0, 0, nil, fmt.Errorf("the AAC track's AudioSpecificConfig: %v", err)
	}
	switch {
	case asc.ObjectType != aac.AAClc && asc.ObjectType != aac.HEAACv1 && asc.ObjectType != aac.HEAACv2:
		return nil, 0, 0, nil, notDecodedf("its AAC audio object type %d is not decoded; AAC-LC and HE-AAC (v1, v2) are", asc.ObjectType)
	case asc.FrameLengthFlag:
		return nil, 0, 0, nil, notDecodedf("its AAC frames are 960 samples, which are not decoded")
	case asc.ChannelConfiguration > 7 && asc.ChannelConfiguration != 11 && asc.ChannelConfiguration != 12:
		return nil, 0, 0, nil, notDecodedf("its AAC channel configuration %d is not decoded; 0 (a program_config_element), 1 to 7, 11 and 12 (mono to 7.1) are", asc.ChannelConfiguration)
	}
	d, err := aacdec.NewDecoderASC(dcd.DecSpecificInfo.DecConfig)
	if err != nil {
		if errors.Is(err, aacdec.ErrUnsupported) {
			return nil, 0, 0, nil, notDecodedf("its AAC uses a feature that is not decoded (%v)", err)
		}
		return nil, 0, 0, nil, fmt.Errorf("the AAC track's AudioSpecificConfig: %v", err)
	}
	for i, au := range tr.samples {
		p, err := d.DecodeAU(au)
		if err != nil {
			if errors.Is(err, aacdec.ErrUnsupported) {
				return nil, 0, 0, nil, notDecodedf("its AAC uses a feature that is not decoded (%v)", err)
			}
			return nil, 0, 0, nil, fmt.Errorf("the AAC track's access unit %d: %v", i, err)
		}
		pcm = append(pcm, p...)
		if r, c := d.OutputSampleRate(), d.OutputChannels(); r > 0 && c > 0 && len(pcm) > (MaxAudioSeconds+60)*r*c {
			return nil, 0, 0, nil, &tooLongError{float64(len(pcm)) / float64(r) / float64(c)}
		}
	}
	return pcm, d.OutputChannels(), d.OutputSampleRate(), d.ChannelNames(), nil
}

// decodeAC3Track decodes an AC-3 (ac-3) or E-AC-3 (ec-3) track with
// internal/ac3, written for CloudBurrow from ATSC A/52 (#1739), and downmixes
// it to mono as ffmpeg's resampler does for this layout: the front pair at
// √½ each, the centre at the bitstream's centre mix level times √2, each
// surround at its surround mix level times √½, the LFE dropped.
func decodeAC3Track(tr *soundTrack) (mono []float32, rate int, err error) {
	d := ac3.NewDecoder()
	var pcm []float32
	channels := 0
	for i, s := range tr.samples {
		p, ch, r, err := d.Decode(s)
		if err != nil {
			return nil, 0, fmt.Errorf("the AC-3 track's sample %d: %v", i+1, err)
		}
		if channels == 0 {
			channels, rate = ch, r
		} else if ch != channels || r != rate {
			return nil, 0, fmt.Errorf("the AC-3 track changes from %d channels at %d Hz to %d at %d at sample %d", channels, rate, ch, r, i+1)
		}
		pcm = append(pcm, p...)
		if len(pcm) > (MaxAudioSeconds+60)*rate*channels {
			return nil, 0, &tooLongError{float64(len(pcm)) / float64(rate) / float64(channels)}
		}
	}
	if channels == 0 {
		return nil, 0, errors.New("the AC-3 track has no audio")
	}
	center, surround := d.MixLevels()
	return downmixLayout(pcm, d.Layout(), center, surround), rate, nil
}

// downmixLayout downmixes interleaved samples of the named channels (ffmpeg's
// names: FL, FR, FC, LFE, BL, BR, SL, SR, BC...) to mono as ffmpeg's
// resampler does (measured with `-ac 1`): the front pair at √½ each, the
// centre at the centre mix level times √2, each surround at the surround
// mix level times √½, the LFE dropped; the default levels (√½, √½) give
// 1 and ½. A single channel is itself.
func downmixLayout(pcm []float32, names []string, center, surround float64) []float32 {
	channels := len(names)
	if channels == 1 {
		return pcm
	}
	weights := make([]float32, channels)
	for c, name := range names {
		switch name {
		case "FC":
			weights[c] = float32(center * math.Sqrt2)
		case "LFE":
		case "FL", "FR":
			weights[c] = math.Sqrt2 / 2
		default: // the surrounds
			weights[c] = float32(surround * math.Sqrt2 / 2)
		}
	}
	mono := make([]float32, len(pcm)/channels)
	for i := range mono {
		var v float32
		for c, w := range weights {
			v += w * pcm[i*channels+c]
		}
		mono[i] = v
	}
	return mono
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

// decodeG711Track reads a QuickTime µ-law (ulaw) or A-law (alaw) track
// (#1651): one byte a sample, channels interleaved, decoded as ffmpeg's
// pcm_mulaw and pcm_alaw decoders do (ITU-T G.711).
func decodeG711Track(tr *soundTrack, e *mp4.AudioSampleEntryBox) (pcm []float32, channels, rate int, err error) {
	if e == nil {
		return nil, 0, 0, errors.New("the G.711 track has no sample entry")
	}
	channels, rate = int(e.ChannelCount), int(e.SampleRate)
	if channels < 1 || channels > 8 || rate < 1 || rate > 192000 {
		return nil, 0, 0, notDecodedf("its G.711 format (%d channels at %d Hz) is not decoded", channels, rate)
	}
	raw := bytes.Join(tr.samples, nil)
	if len(raw)/channels > (MaxAudioSeconds+60)*rate {
		return nil, 0, 0, &tooLongError{float64(len(raw)/channels) / float64(rate)}
	}
	alaw := e.Type() == "alaw"
	pcm = make([]float32, len(raw)/channels*channels)
	for i := range pcm {
		pcm[i] = float32(g711(raw[i], alaw)) / 32768
	}
	return pcm, channels, rate, nil
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
		return nil, 0, 0, notDecodedf("its PCM format (%d channels at %d Hz) is not decoded", channels, rate)
	}
	if !(bits == 8 || bits == 16 || bits == 24 || bits == 32 || (float && bits == 64)) || (float && bits < 32) {
		return nil, 0, 0, notDecodedf("its %d-bit PCM is not decoded", bits)
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

// vorbisLayouts are the channels of Opus's mapping family 1 (RFC 7845
// 5.1.1.2, the Vorbis order), by count, in ffmpeg's names.
var vorbisLayouts = [9][]string{
	3: {"FL", "FC", "FR"},
	4: {"FL", "FR", "BL", "BR"},
	5: {"FL", "FC", "FR", "BL", "BR"},
	6: {"FL", "FC", "FR", "BL", "BR", "LFE"},
	7: {"FL", "FC", "FR", "SL", "SR", "BC", "LFE"},
	8: {"FL", "FC", "FR", "SL", "SR", "BL", "BR", "LFE"},
}

// decodeOpusTrack decodes an Opus track (ISO/IEC 14496-12 Opus sample
// entry, its dOps box), mono, stereo or (#1739) surround, at 48 kHz, as
// ffmpeg's libopus decoder does: the output gain applied, and the pre-skip
// dropped when no edit list stands for it. Surround is downmixed to mono
// with ffmpeg's default levels.
func decodeOpusTrack(tr *soundTrack, e *mp4.AudioSampleEntryBox, edited bool) (pcm []float32, channels, rate int, err error) {
	if e == nil || e.Dops == nil {
		return nil, 0, 0, errors.New("the Opus track has no dOps box")
	}
	dops := e.Dops
	channels = int(dops.OutputChannelCount)
	const opusRate = 48000
	var d *opus.Decoder
	switch {
	case dops.ChannelMappingFamily == 0 && channels >= 1 && channels <= 2:
		d, err = opus.NewDecoder(opusRate, channels)
	case dops.ChannelMappingFamily == 1 && channels >= 1 && channels <= 8:
		// #1739: surround (RFC 7845's Vorbis channel order), several
		// streams through libopus's multistream decoder.
		if len(dops.ChannelMapping) != channels || int(dops.StreamCount) < 1 || dops.CoupledCount > dops.StreamCount {
			return nil, 0, 0, fmt.Errorf("the Opus track's channel mapping (%d streams, %d coupled, %d entries) does not fit its %d channels",
				dops.StreamCount, dops.CoupledCount, len(dops.ChannelMapping), channels)
		}
		d, err = opus.NewMultistreamDecoder(opusRate, channels, int(dops.StreamCount), int(dops.CoupledCount), dops.ChannelMapping)
	default:
		return nil, 0, 0, fmt.Errorf("its Opus has %d channels (mapping family %d), which are not decoded; families 0 and 1 (mono to 7.1) are",
			channels, dops.ChannelMappingFamily)
	}
	if err != nil {
		return nil, 0, 0, fmt.Errorf("the Opus track: %v", err)
	}
	defer d.Close()
	gain := float32(math.Pow(10, float64(dops.OutputGain)/(20*256)))
	buf := make([]float32, 5760*channels) // 120 ms, the longest packet
	for i, packet := range tr.samples {
		n, err := d.DecodeF32(packet, buf, 5760, false)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("the Opus track's packet %d: %v", i+1, err)
		}
		for _, v := range buf[:n*channels] {
			pcm = append(pcm, v*gain)
		}
		if len(pcm) > (MaxAudioSeconds+60)*opusRate*channels {
			return nil, 0, 0, &tooLongError{float64(len(pcm)) / opusRate / float64(channels)}
		}
	}
	if !edited {
		pcm = pcm[min(len(pcm), int(dops.PreSkip)*channels):]
	}
	if channels > 2 {
		return downmixLayout(pcm, vorbisLayouts[channels], math.Sqrt2/2, math.Sqrt2/2), 1, opusRate, nil
	}
	return pcm, channels, opusRate, nil
}
