package bigqueryfront

import (
	"math"
	"sort"
)

// STL, Seasonal and Trend decomposition using Loess, which BigQuery
// documents as the ARIMA_PLUS pipeline's way to "Detect multiple seasonal
// patterns within a single time series"
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-time-series).
// This is Cleveland, Cleveland, McRae and Terpenning's procedure (1990,
// "STL: A Seasonal-Trend Decomposition Procedure Based on Loess", Journal
// of Official Statistics 6, 3–73), ported from their netlib Fortran
// (stl.f: stl, stlstp, stlss, stlfts, stless, stlest, stlrwt), with the
// parameters R's stl() takes by default
// (https://stat.ethz.ch/R-manual/R-devel/library/stats/html/stl.html):
// t.window nextodd(ceiling(1.5·period / (1 − 1.5/s.window))), l.window
// nextodd(period), s.degree 0, t.degree and l.degree 1, each jump
// ceiling(window/10), and inner 2 and outer 0 iterations (robust: 1 and
// 15). Google documents none of them; s.window, which R leaves to the
// caller, is 7 + 4·i for the i-th period (from 0, shortest first), MSTL's
// (Bandara, Hyndman and Bergmeir 2021) default.

// stlFit is one STL decomposition: x = Seasonal + Trend + remainder.
type stlFit struct {
	Seasonal, Trend []float64
}

func nextOdd(x int) int {
	if x%2 == 0 {
		return x + 1
	}
	return x
}

// stl decomposes x, of period np, with seasonal smoother span ns.
func stl(x []float64, np, ns int, robust bool) stlFit {
	n := len(x)
	np = max(np, 2)
	ns = max(nextOdd(ns), 3)
	nt := max(nextOdd(int(math.Ceil(1.5*float64(np)/(1-1.5/float64(ns))))), 3)
	nl := max(nextOdd(np), 3)
	const isdeg, itdeg, ildeg = 0, 1, 1
	jump := func(w int) int { return int(math.Ceil(float64(w) / 10)) }
	ni, no := 2, 0
	if robust {
		ni, no = 1, 15
	}
	season := make([]float64, n)
	trend := make([]float64, n)
	rw := make([]float64, n)
	userw := false
	for k := 0; ; k++ {
		stlStep(x, np, ns, nt, nl, isdeg, itdeg, ildeg, jump(ns), jump(nt), jump(nl), ni, userw, rw, season, trend)
		if k+1 > no {
			break
		}
		fit := make([]float64, n)
		for i := range fit {
			fit[i] = trend[i] + season[i]
		}
		stlRobustWeights(x, fit, rw)
		userw = true
	}
	return stlFit{Seasonal: season, Trend: trend}
}

// stlStep is stlstp: ni inner (backfitting) passes.
func stlStep(y []float64, np, ns, nt, nl, isdeg, itdeg, ildeg, nsjump, ntjump, nljump, ni int, userw bool, rw, season, trend []float64) {
	n := len(y)
	w := make([]float64, n)
	for j := 0; j < ni; j++ {
		for i := range w {
			w[i] = y[i] - trend[i]
		}
		c := stlCycleSubseries(w, np, ns, isdeg, nsjump, userw, rw) // n + 2np
		low := stlLowPass(c, np)                                    // n
		l := stlLoess(low, nl, ildeg, nljump, false, nil)
		for i := range season {
			season[i] = c[np+i] - l[i]
		}
		for i := range w {
			w[i] = y[i] - season[i]
		}
		copy(trend, stlLoess(w, nt, itdeg, ntjump, userw, rw))
	}
}

// stlCycleSubseries is stlss: each cycle-subseries smoothed by loess and
// extended one point at each end, so n + 2·np values.
func stlCycleSubseries(y []float64, np, ns, isdeg, nsjump int, userw bool, rw []float64) []float64 {
	n := len(y)
	out := make([]float64, n+2*np)
	for j := 0; j < np; j++ {
		k := (n-j-1)/np + 1
		sub := make([]float64, k)
		var subw []float64
		if userw {
			subw = make([]float64, k)
		}
		for i := 0; i < k; i++ {
			sub[i] = y[i*np+j]
			if userw {
				subw[i] = rw[i*np+j]
			}
		}
		sm := make([]float64, k+2)
		copy(sm[1:], stlLoess(sub, ns, isdeg, nsjump, userw, subw))
		nright := min(ns, k)
		if v, ok := stlEstimate(sub, ns, isdeg, 0, 1, nright, userw, subw); ok {
			sm[0] = v
		} else {
			sm[0] = sm[1]
		}
		nleft := max(1, k-ns+1)
		if v, ok := stlEstimate(sub, ns, isdeg, float64(k+1), nleft, k, userw, subw); ok {
			sm[k+1] = v
		} else {
			sm[k+1] = sm[k]
		}
		for m := 0; m < k+2; m++ {
			out[m*np+j] = sm[m]
		}
	}
	return out
}

// stlLowPass is stlfts: moving averages of np, np and 3 points, so
// len(x) − 2·np values.
func stlLowPass(x []float64, np int) []float64 {
	return stlMovingAverage(stlMovingAverage(stlMovingAverage(x, np), np), 3)
}

// stlMovingAverage is stlma: the len(x) − w + 1 means of w points.
func stlMovingAverage(x []float64, w int) []float64 {
	out := make([]float64, len(x)-w+1)
	var s float64
	for i := 0; i < w; i++ {
		s += x[i]
	}
	out[0] = s / float64(w)
	for i := 1; i < len(out); i++ {
		s += x[i+w-1] - x[i-1]
		out[i] = s / float64(w)
	}
	return out
}

// stlLoess is stless: y smoothed by loess of span span and degree deg,
// fitted at every jump-th point and linearly interpolated between.
func stlLoess(y []float64, span, deg, jump int, userw bool, rw []float64) []float64 {
	n := len(y)
	ys := make([]float64, n)
	if n < 2 {
		copy(ys, y)
		return ys
	}
	nj := min(jump, n-1)
	nleft, nright := 1, n
	fit := func(i int) {
		if v, ok := stlEstimate(y, span, deg, float64(i), nleft, nright, userw, rw); ok {
			ys[i-1] = v
		} else {
			ys[i-1] = y[i-1]
		}
	}
	switch {
	case span >= n:
		for i := 1; i <= n; i += nj {
			fit(i)
		}
	case nj == 1:
		nsh := (span + 1) / 2
		nleft, nright = 1, span
		for i := 1; i <= n; i++ {
			if i > nsh && nright != n {
				nleft++
				nright++
			}
			fit(i)
		}
	default:
		nsh := (span + 1) / 2
		for i := 1; i <= n; i += nj {
			switch {
			case i < nsh:
				nleft, nright = 1, span
			case i >= n-nsh+1:
				nleft, nright = n-span+1, n
			default:
				nleft, nright = i-nsh+1, span+i-nsh
			}
			fit(i)
		}
	}
	if nj != 1 {
		for i := 1; i <= n-nj; i += nj {
			delta := (ys[i+nj-1] - ys[i-1]) / float64(nj)
			for j := i + 1; j <= i+nj-1; j++ {
				ys[j-1] = ys[i-1] + delta*float64(j-i)
			}
		}
		k := ((n-1)/nj)*nj + 1
		if k != n {
			fit(n)
			if k != n-1 {
				delta := (ys[n-1] - ys[k-1]) / float64(n-k)
				for j := k + 1; j <= n-1; j++ {
					ys[j-1] = ys[k-1] + delta*float64(j-k)
				}
			}
		}
	}
	return ys
}

// stlEstimate is stlest: the loess fit at xs (1-based) over the points
// nleft..nright (1-based) with tricube weights, times rw when userw; ok
// false when every weight is 0.
func stlEstimate(y []float64, span, deg int, xs float64, nleft, nright int, userw bool, rw []float64) (float64, bool) {
	n := len(y)
	rng := float64(n) - 1
	h := math.Max(xs-float64(nleft), float64(nright)-xs)
	if span > n {
		h += float64((span - n) / 2)
	}
	h9, h1 := 0.999*h, 0.001*h
	w := make([]float64, nright-nleft+1)
	var a float64
	for j := nleft; j <= nright; j++ {
		r := math.Abs(float64(j) - xs)
		if r > h9 {
			continue
		}
		wj := 1.0
		if r > h1 {
			wj = math.Pow(1-math.Pow(r/h, 3), 3)
		}
		if userw {
			wj *= rw[j-1]
		}
		w[j-nleft] = wj
		a += wj
	}
	if a <= 0 {
		return 0, false
	}
	for i := range w {
		w[i] /= a
	}
	if h > 0 && deg > 0 {
		var m float64
		for j := nleft; j <= nright; j++ {
			m += w[j-nleft] * float64(j)
		}
		b := xs - m
		var c float64
		for j := nleft; j <= nright; j++ {
			c += w[j-nleft] * (float64(j) - m) * (float64(j) - m)
		}
		if math.Sqrt(c) > 0.001*rng {
			b /= c
			for j := nleft; j <= nright; j++ {
				w[j-nleft] *= b*(float64(j)-m) + 1
			}
		}
	}
	var ys float64
	for j := nleft; j <= nright; j++ {
		ys += w[j-nleft] * y[j-1]
	}
	return ys, true
}

// stlRobustWeights is stlrwt: bisquare weights of the residuals, in units
// of 6 times their median absolute value.
func stlRobustWeights(y, fit, rw []float64) {
	n := len(y)
	r := make([]float64, n)
	for i := range r {
		r[i] = math.Abs(y[i] - fit[i])
	}
	s := append([]float64(nil), r...)
	sort.Float64s(s)
	m1 := n/2 + 1
	m2 := n - m1 + 1
	cmad := 3 * (s[m1-1] + s[m2-1])
	c9, c1 := 0.999*cmad, 0.001*cmad
	for i, v := range r {
		switch {
		case v <= c1:
			rw[i] = 1
		case v <= c9:
			u := v / cmad
			rw[i] = (1 - u*u) * (1 - u*u)
		default:
			rw[i] = 0
		}
	}
}
