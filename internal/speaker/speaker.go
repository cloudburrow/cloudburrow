// Package speaker diarizes speech (#2131): it tells who speaks each word of
// a transcription, for Speech-to-Text's diarizationConfig, which Google's
// supported languages tables mark on chirp_3 (v2) and on v1's models.
//
// Google publishes no speaker model, so the voices are told apart with
// 3D-Speaker's CAM++ speaker-embedding network (Alibaba DAMO's
// speech_campplus_sv_zh_en_16k-common_advanced, Apache-2.0), in the ONNX
// export sherpa-onnx's authors (k2-fsa, Apache-2.0) publish on their GitHub
// release, pinned by SHA-256 in internal/localai (SpeakerCAMPlusPlus). It
// runs on ONNX Runtime, which only the opt-in `-tags onnx` build loads
// (onnx.go); the default build has stub.go, and diarization is 501 there.
//
// The pipeline:
//
//  1. Segmentation: the transcription's words, merged across pauses
//     shorter than pauseSeconds, are speech regions (whisper's word
//     timestamps are the voice activity; no gated segmentation model is
//     used), each cut into windows of WindowSeconds every HopSeconds.
//  2. Embedding: each window's Kaldi fbank features (fbank.go), mean
//     normalised, through CAM++: one 192-dimensional vector a window.
//  3. Clustering: agglomerative, average linkage on cosine similarity,
//     merging while the closest clusters are more alike than
//     mergeThreshold, and always until there are at most Max clusters,
//     never below Min (Options). Beyond maxClustered windows, an even
//     subsample is clustered and the rest join their nearest centroid.
//  4. Labelling: each word takes the speaker of the windows around its
//     midpoint, each weighed by how centred the word is in it. Speakers
//     are numbered 1, 2, ... in the order they first speak.
package speaker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// ErrNotCompiled means this binary was built without the onnx tag.
var ErrNotCompiled = errors.New("speaker diarization runs 3D-Speaker's CAM++ speaker-embedding model on ONNX Runtime, " +
	"which only CloudBurrow's `-tags onnx` build (make build-onnx) run with ONNXRUNTIME_LIB set loads (docs/embeddings.md)")

const (
	// WindowSeconds and HopSeconds are the segmentation's windows.
	WindowSeconds = 1.5
	HopSeconds    = 0.25
	// pauseSeconds is the shortest pause between words that ends a
	// speech region; minRegionSeconds the shortest region embedded.
	pauseSeconds     = 0.3
	minRegionSeconds = 0.5
	// mergeThreshold is the cosine similarity of CAM++ embeddings above
	// which two clusters are taken for one speaker.
	mergeThreshold = 0.45
	// maxClustered bounds the windows clustered with average linkage
	// (O(n^3)); the others are assigned to the nearest centroid.
	maxClustered = 600
	// minClusterWindows is the fewest windows a speaker found without a
	// minSpeakerCount has: 0.75 s of hops.
	minClusterWindows = 3
	// embedBatch is the windows run through the model at once.
	embedBatch = 32
)

// Span is a word's time in the audio.
type Span struct{ Start, End time.Duration }

// Options bound the number of speakers. Zero leaves a bound free.
type Options struct{ Min, Max int }

var (
	srcMu sync.Mutex
	srcFn func(ctx context.Context) (string, error)
)

// SetModelSource installs the function that returns the path of the CAM++
// ONNX model, fetching it first when it is missing.
func SetModelSource(f func(ctx context.Context) (string, error)) {
	srcMu.Lock()
	defer srcMu.Unlock()
	srcFn = f
}

func modelPath(ctx context.Context) (string, error) {
	srcMu.Lock()
	f := srcFn
	srcMu.Unlock()
	if f == nil {
		return "", errors.New("no speaker model source is installed")
	}
	return f(ctx)
}

// Unavailable is why diarization cannot run in this binary, nil when it
// can.
func Unavailable() error {
	if !Available {
		return ErrNotCompiled
	}
	return nil
}

// Diarize returns the speaker, from 1, of each span of samples (16 kHz
// mono).
func Diarize(ctx context.Context, samples []float32, spans []Span, opt Options) ([]int, error) {
	if !Available {
		return nil, ErrNotCompiled
	}
	return diarize(ctx, samples, spans, opt, embedWindows)
}

type window struct{ start, end int } // samples

// windows are the segmentation's windows: the spans, merged across
// pauses shorter than pauseSeconds, are speech regions, and each region
// has windows of WindowSeconds every HopSeconds, the last ending with it,
// so that no window straddles a pause, where speakers mostly change. A
// region shorter than a window is one window when it is at least
// minRegionSeconds long.
func windows(n int, spans []Span) []window {
	size, hop := int(WindowSeconds*sampleRate), int(HopSeconds*sampleRate)
	at := func(d time.Duration) int { return min(max(int(d.Seconds()*sampleRate), 0), n) }
	sorted := append([]Span(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	var regions []window
	for _, s := range sorted {
		a, b := at(s.Start), at(s.End)
		if b <= a {
			continue
		}
		if k := len(regions) - 1; k >= 0 && a-regions[k].end < int(pauseSeconds*sampleRate) {
			regions[k].end = max(regions[k].end, b)
			continue
		}
		regions = append(regions, window{a, b})
	}
	var out []window
	for _, r := range regions {
		if r.end-r.start < size {
			if r.end-r.start >= int(minRegionSeconds*sampleRate) {
				out = append(out, r)
			}
			continue
		}
		s := r.start
		for ; s+size <= r.end; s += hop {
			out = append(out, window{s, s + size})
		}
		if last := out[len(out)-1]; last.end < r.end {
			out = append(out, window{r.end - size, r.end})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

type embedder func(ctx context.Context, feats [][][]float32) ([][]float32, error)

func diarize(ctx context.Context, samples []float32, spans []Span, opt Options, embed embedder) ([]int, error) {
	if opt.Min > 0 && opt.Max > 0 && opt.Min > opt.Max {
		return nil, fmt.Errorf("minSpeakerCount %d is greater than maxSpeakerCount %d", opt.Min, opt.Max)
	}
	labels := make([]int, len(spans))
	wins := windows(len(samples), spans)
	if len(wins) == 0 {
		for i := range labels {
			labels[i] = 1
		}
		return labels, nil
	}
	embs := make([][]float32, 0, len(wins))
	for i := 0; i < len(wins); i += embedBatch {
		batch := wins[i:min(i+embedBatch, len(wins))]
		feats := make([][][]float32, len(batch))
		for j, w := range batch {
			feats[j] = FbankMeanNormalized(samples[w.start:w.end])
		}
		out, err := embed(ctx, feats)
		if err != nil {
			return nil, err
		}
		embs = append(embs, out...)
	}
	for _, e := range embs {
		normalize(e)
	}
	cl := cluster(embs, opt)
	// Each span's speaker: the windows around its midpoint, weighed by how
	// centred it is in each.
	half := float64(WindowSeconds*sampleRate) / 2
	scores := make([]map[int]float64, len(spans))
	for i, s := range spans {
		mid := (s.Start + s.End).Seconds() / 2 * sampleRate
		score := map[int]float64{}
		scores[i] = score
		best, bestDist := 0, math.Inf(1)
		// The windows sorted by start: those that may hold mid, and the
		// nearest one on each side for a span no window holds.
		lo := sort.Search(len(wins), func(j int) bool { return float64(wins[j].end) > mid }) - 1
		hi := sort.Search(len(wins), func(j int) bool { return float64(wins[j].start) > mid }) + 1
		for j := max(lo, 0); j < min(hi, len(wins)); j++ {
			w := wins[j]
			c := float64(w.start+w.end) / 2
			h := float64(w.end-w.start) / 2
			if h == 0 {
				h = half
			}
			if d := math.Abs(mid - c); d < h {
				score[cl[j]] += 1 - d/h
			} else if d < bestDist {
				best, bestDist = cl[j], d
			}
		}
		if len(score) == 0 {
			labels[i] = best
			continue
		}
		top, topScore := -1, -1.0
		for k, v := range score {
			if v > topScore || (v == topScore && k < top) {
				top, topScore = k, v
			}
		}
		labels[i] = top
	}
	keepClusters(labels, scores, cl)
	// Number the speakers in the order they first speak.
	order := map[int]int{}
	for i, l := range labels {
		if _, ok := order[l]; !ok {
			order[l] = len(order) + 1
		}
		labels[i] = order[l]
	}
	return labels, nil
}

// keepClusters gives each cluster that won no span the span it scores
// best on, taken from a cluster that keeps another span, so that a
// minSpeakerCount the clustering honoured is honoured in the labels too.
func keepClusters(labels []int, scores []map[int]float64, cl []int) {
	count := map[int]int{}
	for _, l := range labels {
		count[l]++
	}
	seen := map[int]bool{}
	for _, c := range cl {
		if seen[c] || count[c] > 0 {
			seen[c] = true
			continue
		}
		seen[c] = true
		best, bs := -1, 0.0
		for i, sc := range scores {
			if count[labels[i]] > 1 && sc[c] > bs {
				best, bs = i, sc[c]
			}
		}
		if best >= 0 {
			count[labels[best]]--
			labels[best] = c
			count[c]++
		}
	}
}

func normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n = math.Sqrt(n); n > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / n)
		}
	}
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// cluster returns each embedding's cluster (0-based), the embeddings unit
// vectors.
func cluster(embs [][]float32, opt Options) []int {
	n := len(embs)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	if n > maxClustered {
		idx = idx[:0]
		for k := 0; k < maxClustered; k++ {
			idx = append(idx, k*n/maxClustered)
		}
	}
	m := len(idx)
	sim := make([][]float64, m)
	for i := range sim {
		sim[i] = make([]float64, m)
		for j := range sim[i] {
			sim[i][j] = dot(embs[idx[i]], embs[idx[j]])
		}
	}
	size := make([]int, m)
	alive := make([]bool, m)
	member := make([]int, m) // each sampled point's cluster root
	for i := range size {
		size[i], alive[i], member[i] = 1, true, i
	}
	count := m
	for count > 1 {
		bi, bj, bs := -1, -1, math.Inf(-1)
		for i := 0; i < m; i++ {
			if !alive[i] {
				continue
			}
			for j := i + 1; j < m; j++ {
				if alive[j] && sim[i][j] > bs {
					bi, bj, bs = i, j, sim[i][j]
				}
			}
		}
		mustMerge := opt.Max > 0 && count > opt.Max
		mayMerge := opt.Min <= 0 || count > opt.Min
		if !mustMerge && (!mayMerge || bs < mergeThreshold) {
			// A cluster of fewer than minClusterWindows windows is a
			// turn's edge or a noise, not a speaker: it joins the cluster
			// it is most like, unless minSpeakerCount needs it.
			if !mayMerge {
				break
			}
			small := -1
			for i := 0; i < m; i++ {
				if alive[i] && size[i] < minClusterWindows && (small < 0 || size[i] < size[small]) {
					small = i
				}
			}
			if small < 0 {
				break
			}
			bi, bj, bs = -1, -1, math.Inf(-1)
			for k := 0; k < m; k++ {
				if alive[k] && k != small && sim[small][k] > bs {
					bi, bj, bs = min(small, k), max(small, k), sim[small][k]
				}
			}
		}
		// Average linkage (Lance-Williams).
		for k := 0; k < m; k++ {
			if alive[k] && k != bi && k != bj {
				s := (float64(size[bi])*sim[bi][k] + float64(size[bj])*sim[bj][k]) / float64(size[bi]+size[bj])
				sim[bi][k], sim[k][bi] = s, s
			}
		}
		size[bi] += size[bj]
		alive[bj] = false
		for k := range member {
			if member[k] == bj {
				member[k] = bi
			}
		}
		count--
	}
	// Centroids of the clusters.
	roots := map[int]int{}
	var cents [][]float64
	for k, r := range member {
		c, ok := roots[r]
		if !ok {
			c = len(cents)
			roots[r] = c
			cents = append(cents, make([]float64, len(embs[0])))
		}
		for d, x := range embs[idx[k]] {
			cents[c][d] += float64(x)
		}
	}
	out := make([]int, n)
	if n <= maxClustered {
		for k, r := range member {
			out[k] = roots[r]
		}
		return out
	}
	for i, e := range embs {
		best, bs := 0, math.Inf(-1)
		for c, cent := range cents {
			var s float64
			for d, x := range e {
				s += float64(x) * cent[d]
			}
			if s /= norm(cent); s > bs {
				best, bs = c, s
			}
		}
		out[i] = best
	}
	return out
}

func norm(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	if s == 0 {
		return 1
	}
	return math.Sqrt(s)
}

// Speakers is the number of distinct labels.
func Speakers(labels []int) int {
	seen := map[int]bool{}
	for _, l := range labels {
		seen[l] = true
	}
	return len(seen)
}
