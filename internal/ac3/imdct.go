package ac3

import (
	"math"
	"math/cmplx"
)

// The inverse transforms of clause 6.9.4, by its steps: the pre-IFFT
// twiddle, an N/4- (or two N/8-) point complex IFFT, the post-IFFT twiddle,
// windowing and de-interleaving, then overlap and add.

const n512 = 512

var (
	xcos1, xsin1 [128]float64
	xcos2, xsin2 [64]float64
)

func init() {
	for k := range 128 {
		a := 2 * math.Pi * float64(8*k+1) / (8 * n512)
		xcos1[k], xsin1[k] = -math.Cos(a), -math.Sin(a)
	}
	for k := range 64 {
		a := 2 * math.Pi * float64(8*k+1) / (4 * n512)
		xcos2[k], xsin2[k] = -math.Cos(a), -math.Sin(a)
	}
}

// ifft computes z[n] = sum_k Z[k] e^(+j 2 pi k n / len) in place
// (len a power of two).
func ifft(z []complex128) {
	n := len(z)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			z[i], z[j] = z[j], z[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, 2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := range size / 2 {
				a, b := z[start+k], z[start+k+size/2]*wk
				z[start+k], z[start+k+size/2] = a+b, a-b
				wk *= w
			}
		}
	}
}

// imdct transforms one block's 256 coefficients to 256 output samples,
// overlapping with delay.
func imdct(X *[256]float64, short bool, delay *[256]float64, pcm []float64) {
	const N = n512
	var x [N]float64
	w := &window
	if !short {
		var z [128]complex128
		for k := range N / 4 {
			re, im := X[N/2-2*k-1], X[2*k]
			z[k] = complex(re*xcos1[k]-im*xsin1[k], im*xcos1[k]+re*xsin1[k])
		}
		ifft(z[:])
		var yr, yi [128]float64
		for n := range N / 4 {
			zr, zi := real(z[n]), imag(z[n])
			yr[n], yi[n] = zr*xcos1[n]-zi*xsin1[n], zi*xcos1[n]+zr*xsin1[n]
		}
		for n := range N / 8 {
			x[2*n] = -yi[N/8+n] * w[2*n]
			x[2*n+1] = yr[N/8-n-1] * w[2*n+1]
			x[N/4+2*n] = -yr[n] * w[N/4+2*n]
			x[N/4+2*n+1] = yi[N/4-n-1] * w[N/4+2*n+1]
			x[N/2+2*n] = -yr[N/8+n] * w[N/2-2*n-1]
			x[N/2+2*n+1] = yi[N/8-n-1] * w[N/2-2*n-2]
			x[3*N/4+2*n] = yi[n] * w[N/4-2*n-1]
			x[3*N/4+2*n+1] = -yr[N/4-n-1] * w[N/4-2*n-2]
		}
	} else {
		var z1, z2 [64]complex128
		for k := range N / 8 {
			// x1[k] = X[2k], x2[k] = X[2k+1]
			a1, b1 := X[2*(N/4-2*k-1)], X[2*(2*k)]
			a2, b2 := X[2*(N/4-2*k-1)+1], X[2*(2*k)+1]
			z1[k] = complex(a1*xcos2[k]-b1*xsin2[k], b1*xcos2[k]+a1*xsin2[k])
			z2[k] = complex(a2*xcos2[k]-b2*xsin2[k], b2*xcos2[k]+a2*xsin2[k])
		}
		ifft(z1[:])
		ifft(z2[:])
		var yr1, yi1, yr2, yi2 [64]float64
		for n := range N / 8 {
			zr, zi := real(z1[n]), imag(z1[n])
			yr1[n], yi1[n] = zr*xcos2[n]-zi*xsin2[n], zi*xcos2[n]+zr*xsin2[n]
			zr, zi = real(z2[n]), imag(z2[n])
			yr2[n], yi2[n] = zr*xcos2[n]-zi*xsin2[n], zi*xcos2[n]+zr*xsin2[n]
		}
		for n := range N / 8 {
			x[2*n] = -yi1[n] * w[2*n]
			x[2*n+1] = yr1[N/8-n-1] * w[2*n+1]
			x[N/4+2*n] = -yr1[n] * w[N/4+2*n]
			x[N/4+2*n+1] = yi1[N/8-n-1] * w[N/4+2*n+1]
			x[N/2+2*n] = -yr2[n] * w[N/2-2*n-1]
			x[N/2+2*n+1] = yi2[N/8-n-1] * w[N/2-2*n-2]
			x[3*N/4+2*n] = yi2[n] * w[N/4-2*n-1]
			x[3*N/4+2*n+1] = -yr2[N/8-n-1] * w[N/4-2*n-2]
		}
	}
	for n := range N / 2 {
		pcm[n] = 2 * (x[n] + delay[n])
		delay[n] = x[N/2+n]
	}
}
