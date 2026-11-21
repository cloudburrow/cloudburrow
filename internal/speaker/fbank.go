package speaker

import (
	"math"
	"math/cmplx"
)

// Kaldi's filter-bank features (compute-fbank-feats, and torchaudio's
// compliance.kaldi.fbank, which 3D-Speaker computes its CAM++ input with):
// 25 ms frames every 10 ms of 16 kHz audio (snip_edges), each frame's DC
// offset removed, pre-emphasised by 0.97, Povey-windowed, zero-padded to a
// 512-point FFT, its power spectrum weighed by 80 triangular mel filters
// from 20 Hz to the Nyquist frequency, and the log of each, floored at
// float32's epsilon. No dither, no energy. FbankMeanNormalized then
// subtracts each bin's mean over the frames (3D-Speaker's, and the model's
// metadata's feature_normalize_type=global-mean).
const (
	sampleRate  = 16000
	frameLength = 400 // 25 ms
	frameShift  = 160 // 10 ms
	fftSize     = 512
	// NumMelBins is the feature dimension the model takes.
	NumMelBins = 80
	lowFreq    = 20.0
	preemph    = 0.97
)

var (
	poveyWindow [frameLength]float64
	melBanks    [NumMelBins][fftSize / 2]float64
	twiddles    [fftSize / 2]complex128
)

func melScale(f float64) float64 { return 1127 * math.Log(1+f/700) }

func init() {
	for i := range poveyWindow {
		poveyWindow[i] = math.Pow(0.5-0.5*math.Cos(2*math.Pi*float64(i)/float64(frameLength-1)), 0.85)
	}
	lo, hi := melScale(lowFreq), melScale(sampleRate/2)
	delta := (hi - lo) / float64(NumMelBins+1)
	width := float64(sampleRate) / fftSize
	for m := 0; m < NumMelBins; m++ {
		left := lo + float64(m)*delta
		center, right := left+delta, left+2*delta
		for k := 0; k < fftSize/2; k++ {
			mel := melScale(width * float64(k))
			switch {
			case mel > left && mel <= center:
				melBanks[m][k] = (mel - left) / (center - left)
			case mel > center && mel < right:
				melBanks[m][k] = (right - mel) / (right - center)
			}
		}
	}
	for i := range twiddles {
		twiddles[i] = cmplx.Exp(complex(0, -2*math.Pi*float64(i)/fftSize))
	}
}

// fft is an in-place radix-2 FFT of fftSize points.
func fft(x *[fftSize]complex128) {
	for i, j := 1, 0; i < fftSize; i++ {
		bit := fftSize >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= fftSize; size <<= 1 {
		step := fftSize / size
		for start := 0; start < fftSize; start += size {
			for k := 0; k < size/2; k++ {
				t := twiddles[k*step] * x[start+k+size/2]
				x[start+k+size/2] = x[start+k] - t
				x[start+k] += t
			}
		}
	}
}

// NumFrames is the number of feature frames of n samples.
func NumFrames(n int) int {
	if n < frameLength {
		return 0
	}
	return 1 + (n-frameLength)/frameShift
}

// Fbank is the features of 16 kHz samples, one row of NumMelBins a frame.
func Fbank(samples []float32) [][]float32 {
	n := NumFrames(len(samples))
	out := make([][]float32, n)
	var frame [frameLength]float64
	var buf [fftSize]complex128
	const eps = 1.1920928955078125e-07 // float32's machine epsilon, Kaldi's floor
	for f := 0; f < n; f++ {
		off := f * frameShift
		mean := 0.0
		for i := range frame {
			frame[i] = float64(samples[off+i])
			mean += frame[i]
		}
		mean /= frameLength
		for i := range frame {
			frame[i] -= mean
		}
		for i := frameLength - 1; i > 0; i-- {
			frame[i] -= preemph * frame[i-1]
		}
		frame[0] -= preemph * frame[0]
		for i := range buf {
			if i < frameLength {
				buf[i] = complex(frame[i]*poveyWindow[i], 0)
			} else {
				buf[i] = 0
			}
		}
		fft(&buf)
		row := make([]float32, NumMelBins)
		for m := range melBanks {
			e := 0.0
			for k, w := range melBanks[m] {
				if w != 0 {
					re, im := real(buf[k]), imag(buf[k])
					e += w * (re*re + im*im)
				}
			}
			row[m] = float32(math.Log(math.Max(e, eps)))
		}
		out[f] = row
	}
	return out
}

// FbankMeanNormalized is Fbank with each bin's mean over the frames
// subtracted.
func FbankMeanNormalized(samples []float32) [][]float32 {
	feats := Fbank(samples)
	if len(feats) == 0 {
		return feats
	}
	var mean [NumMelBins]float64
	for _, row := range feats {
		for m, v := range row {
			mean[m] += float64(v)
		}
	}
	for m := range mean {
		mean[m] /= float64(len(feats))
	}
	for _, row := range feats {
		for m := range row {
			row[m] -= float32(mean[m])
		}
	}
	return feats
}
