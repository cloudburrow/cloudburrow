// CloudBurrow's exports of amr.wasm: a thin C layer over opencore-amr's
// decoder interfaces (amrnb/interf_dec.h, amrwb/dec_if.h) that
// internal/speech calls through wazero. Each call decodes one frame in the
// RFC 4867 storage format (a TOC byte, then the frame's bytes) to 160
// (AMR, 8 kHz) or 320 (AMR-WB, 16 kHz) 16-bit samples.
#include <stdlib.h>

#include "interf_dec.h"
#include "dec_if.h"

#define CB_EXPORT(name) __attribute__((export_name(#name)))

CB_EXPORT(cb_malloc) void *cb_malloc(size_t n) { return malloc(n); }
CB_EXPORT(cb_free) void cb_free(void *p) { free(p); }

CB_EXPORT(cb_nb_init) void *cb_nb_init(void) { return Decoder_Interface_init(); }
CB_EXPORT(cb_nb_exit) void cb_nb_exit(void *s) { Decoder_Interface_exit(s); }
CB_EXPORT(cb_nb_decode) void cb_nb_decode(void *s, const unsigned char *in, short *out) {
	Decoder_Interface_Decode(s, in, out, 0);
}

CB_EXPORT(cb_wb_init) void *cb_wb_init(void) { return D_IF_init(); }
CB_EXPORT(cb_wb_exit) void cb_wb_exit(void *s) { D_IF_exit(s); }
CB_EXPORT(cb_wb_decode) void cb_wb_decode(void *s, const unsigned char *in, short *out) {
	D_IF_decode(s, in, out, _good_frame);
}
