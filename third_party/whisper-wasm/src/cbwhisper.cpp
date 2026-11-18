// CloudBurrow's exports of whisper.wasm: a thin C layer over whisper.cpp's
// API (include/whisper.h) that internal/speech calls through wazero. The
// module is single-threaded (wasm32-wasip1): every call runs on the
// caller's goroutine, one context per module instance.
#include <cstdint>
#include <cstdlib>
#include <cstring>

#include "whisper.h"

#define CB_EXPORT(name) extern "C" __attribute__((export_name(#name)))

CB_EXPORT(cb_malloc) void *cb_malloc(size_t n) { return malloc(n); }
CB_EXPORT(cb_free) void cb_free(void *p) { free(p); }

static void quiet(enum ggml_log_level, const char *, void *) {}

// cb_init loads a ggml model from the n bytes at buf (whisper.cpp copies
// what it needs; the caller frees buf). It returns the context, 0 on error.
CB_EXPORT(cb_init) whisper_context *cb_init(void *buf, size_t n) {
	whisper_log_set(quiet, nullptr);
	whisper_context_params cp = whisper_context_default_params();
	cp.use_gpu = false;
	cp.flash_attn = false;
	return whisper_init_from_buffer_with_params(buf, n, cp);
}

CB_EXPORT(cb_free_ctx) void cb_free_ctx(whisper_context *ctx) { whisper_free(ctx); }

// cb_lang_id is the id of a language code ("en", "de", ...), -1 when
// whisper does not know it.
CB_EXPORT(cb_lang_id) int cb_lang_id(const char *code) { return whisper_lang_id(code); }

// cb_full transcribes n mono 16 kHz samples. lang is a whisper language code
// or "auto" (detect), translate is 0, offsets 1 computes token timestamps.
// It returns 0 on success.
CB_EXPORT(cb_full) int cb_full(whisper_context *ctx, const float *samples, int n, const char *lang, int offsets) {
	whisper_full_params p = whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
	p.n_threads = 1;
	p.language = lang;
	p.detect_language = false;
	p.translate = false;
	p.print_progress = false;
	p.print_realtime = false;
	p.print_timestamps = false;
	p.print_special = false;
	p.no_context = true;
	p.token_timestamps = offsets != 0;
	p.suppress_nst = true;
	return whisper_full(ctx, p, samples, n);
}

CB_EXPORT(cb_lang) const char *cb_lang(whisper_context *ctx) { return whisper_lang_str(whisper_full_lang_id(ctx)); }
CB_EXPORT(cb_n_segments) int cb_n_segments(whisper_context *ctx) { return whisper_full_n_segments(ctx); }
CB_EXPORT(cb_segment_text) const char *cb_segment_text(whisper_context *ctx, int i) {
	return whisper_full_get_segment_text(ctx, i);
}
// cb_segment_times writes the segment's start and end, in centiseconds, to
// out[0] and out[1].
CB_EXPORT(cb_segment_times) void cb_segment_times(whisper_context *ctx, int i, int64_t *out) {
	out[0] = whisper_full_get_segment_t0(ctx, i);
	out[1] = whisper_full_get_segment_t1(ctx, i);
}
CB_EXPORT(cb_n_tokens) int cb_n_tokens(whisper_context *ctx, int i) { return whisper_full_n_tokens(ctx, i); }
CB_EXPORT(cb_token_text) const char *cb_token_text(whisper_context *ctx, int i, int j) {
	return whisper_full_get_token_text(ctx, i, j);
}
// cb_token_data writes a token's start and end (centiseconds) to out[0..1],
// and its probability and whether it is a text token (not a special or
// timestamp token) to the floats after them: f[0] p, f[1] 1 or 0.
CB_EXPORT(cb_token_data) void cb_token_data(whisper_context *ctx, int i, int j, int64_t *out) {
	whisper_token_data d = whisper_full_get_token_data(ctx, i, j);
	out[0] = d.t0;
	out[1] = d.t1;
	float *f = reinterpret_cast<float *>(out + 2);
	f[0] = d.p;
	f[1] = d.id < whisper_token_eot(ctx) ? 1.0f : 0.0f;
}
