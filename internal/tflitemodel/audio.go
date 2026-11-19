package tflitemodel

import (
	"fmt"
	"math"
)

// AudioSpectrogram and Mfcc (#1775): the custom operators of TensorFlow's
// speech models, which LiteRT's builtin resolver runs
// (kernels/audio_spectrogram.cc, mfcc.cc, and internal/spectrogram.cc,
// mfcc_mel_filterbank.cc, mfcc_dct.cc), in float64 as those do: a periodic
// Hann window, the squared magnitude of each frame's DFT zero-padded to a
// power of two (the magnitude unless magnitude_squared), and HTK-style
// mel filterbank energies, their log and a DCT-II.
const (
	opAudioSpectrogram = -3
	opMfcc             = -4
)

func init() {
	builtinCustom["AudioSpectrogram"] = opAudioSpectrogram
	builtinCustom["Mfcc"] = opMfcc
}

// audioSpectrogram is AudioSpectrogram: input [samples, channels];
// window_size, stride and magnitude_squared; output [channels, frames,
// fft_length/2 + 1].
func audioSpectrogram(o op, x *Tensor) (*Tensor, error) {
	m, err := flexMap(o.customOpts)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", o.name, err)
	}
	win, stride, sq := int(m["window_size"]), int(m["stride"]), m["magnitude_squared"] != 0
	if len(x.Shape) != 2 || !x.Type.IsFloat() {
		return nil, fmt.Errorf("operator %s takes float input [samples, channels], not %v", o.name, x.Shape)
	}
	if win < 2 || stride < 1 {
		return nil, fmt.Errorf("operator %s: window_size %d and stride %d", o.name, win, stride)
	}
	samples, chans := x.Shape[0], x.Shape[1]
	fft := 1
	for fft < win {
		fft <<= 1
	}
	bins := fft/2 + 1
	frames := 0
	if samples >= win {
		frames = 1 + (samples-win)/stride
	}
	window := make([]float64, win)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(win))
	}
	// cos and sin of 2*pi*k*n/fft, by k*n mod fft.
	cs, sn := make([]float64, fft), make([]float64, fft)
	for i := range fft {
		cs[i], sn[i] = math.Cos(2*math.Pi*float64(i)/float64(fft)), math.Sin(2*math.Pi*float64(i)/float64(fft))
	}
	r := &Tensor{Type: Float32, Shape: []int{chans, frames, bins}, F: make([]float32, chans*frames*bins)}
	in := make([]float64, win)
	for c := range chans {
		for f := range frames {
			for i := range win {
				in[i] = window[i] * float64(x.F[(f*stride+i)*chans+c])
			}
			for k := range bins {
				var re, im float64
				for i, v := range in {
					p := k * i % fft
					re += v * cs[p]
					im -= v * sn[p]
				}
				p := re*re + im*im
				if !sq {
					p = math.Sqrt(p)
				}
				r.F[(c*frames+f)*bins+k] = float32(p)
			}
		}
	}
	return r, nil
}

func freqToMel(f float64) float64 { return 1127.0 * math.Log1p(f/700.0) }

// mfcc is Mfcc: spectrogram [channels, frames, bins] (squared magnitudes)
// and sample_rate; upper_frequency_limit, lower_frequency_limit,
// filterbank_channel_count and dct_coefficient_count; output [channels,
// frames, dct_coefficient_count].
func mfcc(o op, spec, rate *Tensor) (*Tensor, error) {
	m, err := flexMap(o.customOpts)
	if err != nil {
		return nil, fmt.Errorf("operator %s: %w", o.name, err)
	}
	// LiteRT reads each option without a default: a missing one is 0.
	upper, lower := m["upper_frequency_limit"], m["lower_frequency_limit"]
	nch, ncoef := int(m["filterbank_channel_count"]), int(m["dct_coefficient_count"])
	if len(spec.Shape) != 3 || !spec.Type.IsFloat() || rate == nil || rate.Len() != 1 {
		return nil, fmt.Errorf("operator %s takes a spectrogram [channels, frames, bins] and one sample rate", o.name)
	}
	bins := spec.Shape[2]
	sr := float64(rate.f(0))
	if nch < 1 || ncoef < 1 || bins < 2 || sr <= 0 || lower < 0 || upper <= lower || upper > sr/2 {
		return nil, fmt.Errorf("operator %s: invalid filterbank (%d channels, %v..%v Hz at %v Hz, %d bins)", o.name, nch, lower, upper, sr, bins)
	}
	center := make([]float64, nch+1)
	melLow, melHi := freqToMel(lower), freqToMel(upper)
	spacing := (melHi - melLow) / float64(nch+1)
	for i := range center {
		center[i] = melLow + spacing*float64(i+1)
	}
	hzPerBin := 0.5 * sr / float64(bins-1)
	start, end := int(1.5+lower/hzPerBin), int(upper/hzPerBin)
	band := make([]int, bins)
	weight := make([]float64, bins)
	ch := 0
	for i := range bins {
		melf := freqToMel(float64(i) * hzPerBin)
		if i < start || i > end {
			band[i] = -2
			continue
		}
		for ch < nch && center[ch] < melf {
			ch++
		}
		band[i] = ch - 1
	}
	for i := range bins {
		if i < start || i > end {
			continue
		}
		melf := freqToMel(float64(i) * hzPerBin)
		if c := band[i]; c >= 0 {
			weight[i] = (center[c+1] - melf) / (center[c+1] - center[c])
		} else {
			weight[i] = (center[0] - melf) / (center[0] - melLow)
		}
	}
	norm, arg := math.Sqrt(2.0/float64(nch)), math.Pi/float64(nch)
	cosines := make([]float64, ncoef*nch)
	for i := range ncoef {
		for j := range nch {
			cosines[i*nch+j] = norm * math.Cos(float64(i)*arg*(float64(j)+0.5))
		}
	}
	frames := spec.Shape[0] * spec.Shape[1]
	r := &Tensor{Type: Float32, Shape: []int{spec.Shape[0], spec.Shape[1], ncoef}, F: make([]float32, frames*ncoef)}
	energies := make([]float64, nch)
	for f := range frames {
		row := spec.F[f*bins : (f+1)*bins]
		clear(energies)
		for i := start; i <= end && i < bins; i++ {
			v := math.Sqrt(float64(row[i]))
			w := v * weight[i]
			c := band[i]
			if c >= 0 {
				energies[c] += w
			}
			if c+1 < nch {
				energies[c+1] += v - w
			}
		}
		for i, v := range energies {
			energies[i] = math.Log(max(v, 1e-12))
		}
		for i := range ncoef {
			var s float64
			for j, v := range energies {
				s += cosines[i*nch+j] * v
			}
			r.F[f*ncoef+i] = float32(s)
		}
	}
	return r, nil
}
