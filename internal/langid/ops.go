// Ported from MediaPipe's language detector custom operators
// (mediapipe/tasks/cc/text/language_detector/custom_ops),
// Copyright 2023 The MediaPipe Authors, licensed under the Apache License,
// Version 2.0; see third_party/mediapipe-langid/PROVENANCE.md.

package langid

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// The language detector's two custom operators, ported from MediaPipe's
// kernels (mediapipe/tasks/cc/text/language_detector/custom_ops, Google's,
// Apache-2.0; see third_party/mediapipe-langid/PROVENANCE.md).

// defaultMaxSplits is ngram_hash.cc's kDefaultMaxSplits.
const defaultMaxSplits = 128

// tokenize is ngram_hash_ops_utils.cc's Tokenize with
// exclude_nonalphaspace_tokens: "^", then each rune of s (a rune that is
// not a letter becomes " "), at most maxTokens-2 of them, then "$". It
// gives the string and each token's byte offset and length.
func tokenize(s string, maxTokens int) (string, [][2]int) {
	buf := make([]byte, 0, len(s)+2)
	var toks [][2]int
	add := func(p string) {
		toks = append(toks, [2]int{len(buf), len(p)})
		buf = append(buf, p...)
	}
	add("^")
	for i := 0; i < len(s) && len(toks)+1 < maxTokens; {
		r, n := utf8.DecodeRuneInString(s[i:])
		if n == 0 {
			break
		}
		if r == utf8.RuneError && n == 1 || !unicode.IsLetter(r) {
			add(" ")
		} else {
			add(s[i : i+n])
		}
		i += n
	}
	add("$")
	return string(buf), toks
}

// lower is LowercaseUnicodeStr: each letter lower-cased.
func lower(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if unicode.IsLetter(r) {
			r = unicode.ToLower(r)
		}
		out = append(out, r)
	}
	return string(out)
}

const murmurMul = 0xc6a4a7935bd1e995

func shiftMix(v uint64) uint64 { return v ^ (v >> 47) }

// murmur64 is murmur.cc's MurmurHash64WithSeed.
func murmur64(b []byte, seed uint64) uint64 {
	n := len(b)
	aligned := n &^ 7
	h := seed ^ (uint64(n) * murmurMul)
	for p := 0; p < aligned; p += 8 {
		d := binary.LittleEndian.Uint64(b[p:])
		h ^= shiftMix(d*murmurMul) * murmurMul
		h *= murmurMul
	}
	if n&7 != 0 {
		var d uint64
		for i, c := range b[aligned:] {
			d |= uint64(c) << (8 * i)
		}
		h ^= d
		h *= murmurMul
	}
	h = shiftMix(h) * murmurMul
	return shiftMix(h)
}

// ngramHash is ngram_hash.cc's Eval: for each n-gram length, the hash of
// the n tokens from each token on, modulo its vocabulary size, plus one;
// shape [1, n-gram lengths, tokens], INT32.
func ngramHash(opts []byte, in []*tflitemodel.Tensor) (*tflitemodel.Tensor, error) {
	m, err := flexMap(opts)
	if err != nil {
		return nil, err
	}
	lengths, vocab := m["ngram_lengths"].ints, m["vocab_sizes"].ints
	if len(lengths) == 0 || len(lengths) != len(vocab) {
		return nil, errors.New("NGramHash: ngram_lengths and vocab_sizes must be non-empty and of the same size")
	}
	maxSplits := defaultMaxSplits
	if v, ok := m["max_splits"]; ok && v.isNum {
		maxSplits = int(v.num)
	}
	if maxSplits <= 0 {
		return nil, errors.New("NGramHash: max_splits must be > 0")
	}
	lowercase := true
	if v, ok := m["lowercase_input"]; ok && v.isNum {
		lowercase = v.num != 0
	}
	seed := uint64(m["seed"].num)
	if len(in) < 1 || in[0] == nil || in[0].Type != tflitemodel.String || len(in[0].S) == 0 || in[0].S[0] == "" {
		return nil, errors.New("NGramHash: Empty input not supported")
	}
	s := in[0].S[0]
	if lowercase {
		// ngram_hash.cc tokenizes the lower-cased text with the input's
		// byte length.
		l := lower(s)
		if len(l) > len(s) {
			l = l[:len(s)]
		}
		s = l
	}
	str, toks := tokenize(s, maxSplits)
	nt := len(toks)
	out := &tflitemodel.Tensor{Type: tflitemodel.Int32, Shape: []int{1, len(lengths), nt}, I: make([]int64, len(lengths)*nt)}
	for g, n := range lengths {
		if vocab[g] <= 0 {
			return nil, fmt.Errorf("NGramHash: vocab size %d", vocab[g])
		}
		for start := range toks {
			bytes := 0
			for i := start; i < nt && i < start+int(n); i++ {
				bytes += toks[i][1]
			}
			off := toks[start][0]
			h := murmur64([]byte(str[off:off+bytes]), seed)
			out.I[g*nt+start] = int64(h%uint64(vocab[g])) + 1
		}
	}
	return out, nil
}

// kmeansEmbeddingLookup is kmeans_embedding_lookup.cc's Eval: the mean,
// over the input's tokens up to the first 0, of each token's embedding,
// which the encoding table spells as codebook rows.
func kmeansEmbeddingLookup(_ []byte, in []*tflitemodel.Tensor) (*tflitemodel.Tensor, error) {
	if len(in) != 3 || in[0] == nil || in[1] == nil || in[2] == nil || len(in[1].Shape) != 2 || len(in[2].Shape) != 2 {
		return nil, errors.New("KmeansEmbeddingLookup: it takes the input, the encoding table and the codebook")
	}
	input, table, book := in[0], in[1], in[2]
	if len(input.Shape) < 2 || input.Shape[0] != 1 {
		return nil, errors.New("KmeansEmbeddingLookup: batch_size must be == 1")
	}
	enc, block := table.Shape[1], book.Shape[1]
	sum := make([]float32, enc*block)
	n := 0
	for _, tok := range input.I[:input.Shape[1]] {
		if tok == 0 {
			break
		}
		n++
		for e := range enc {
			k := int(table.I[int(tok)*enc+e])
			for b := range block {
				sum[e*block+b] += book.F[k*block+b]
			}
		}
	}
	out := &tflitemodel.Tensor{Type: tflitemodel.Float32, Shape: []int{1, enc * block}, F: make([]float32, enc*block)}
	for i, v := range sum {
		out.F[i] = v / float32(max(n, 1))
	}
	return out, nil
}
