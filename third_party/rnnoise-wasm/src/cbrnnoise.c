// CloudBurrow's exports of rnnoise.wasm: a thin C layer over RNNoise's API
// (include/rnnoise.h) that internal/speech calls through wazero. Each
// cb_process denoises one frame of 480 samples at 48 kHz, in the 16-bit
// range, with the built-in model.
#include <stdlib.h>

#include "rnnoise.h"

#define CB_EXPORT(name) __attribute__((export_name(#name)))

CB_EXPORT(cb_malloc) void *cb_malloc(size_t n) { return malloc(n); }
CB_EXPORT(cb_free) void cb_free(void *p) { free(p); }
CB_EXPORT(cb_frame_size) int cb_frame_size(void) { return rnnoise_get_frame_size(); }
CB_EXPORT(cb_create) DenoiseState *cb_create(void) { return rnnoise_create(NULL); }
CB_EXPORT(cb_destroy) void cb_destroy(DenoiseState *st) { rnnoise_destroy(st); }
CB_EXPORT(cb_process) float cb_process(DenoiseState *st, float *out, const float *in) {
	return rnnoise_process_frame(st, out, in);
}
