// CloudBurrow's C entry points into Tesseract, exported from the WASI module
// that build.sh produces. internal/ocr calls them through wazero. Every
// function takes and returns plain integers and pointers into the module's
// linear memory, so the host needs no other glue.
#include <cstdlib>
#include <cstring>

#include <tesseract/baseapi.h>

#define CB_EXPORT(name) extern "C" __attribute__((export_name(#name)))

CB_EXPORT(cb_malloc) void *cb_malloc(size_t n) { return std::malloc(n); }
CB_EXPORT(cb_free) void cb_free(void *p) { std::free(p); }

// cb_create returns a new engine handle.
CB_EXPORT(cb_create) tesseract::TessBaseAPI *cb_create() { return new tesseract::TessBaseAPI(); }

// cb_init loads language lang (for example "eng", "eng+fra", "osd") from the
// tessdata directory datapath with engine mode oem (1 LSTM, 0 legacy). It
// returns 0 on success.
CB_EXPORT(cb_init) int cb_init(tesseract::TessBaseAPI *api, const char *datapath, const char *lang, int oem) {
  return api->Init(datapath, lang, static_cast<tesseract::OcrEngineMode>(oem));
}

// cb_set_variable sets a Tesseract parameter; 1 when it exists.
CB_EXPORT(cb_set_variable) int cb_set_variable(tesseract::TessBaseAPI *api, const char *name, const char *value) {
  return api->SetVariable(name, value) ? 1 : 0;
}

// cb_set_psm sets the page segmentation mode (tesseract::PageSegMode).
CB_EXPORT(cb_set_psm) void cb_set_psm(tesseract::TessBaseAPI *api, int psm) {
  api->SetPageSegMode(static_cast<tesseract::PageSegMode>(psm));
}

// cb_set_image passes raw pixels: bpp bytes a pixel (1 grey, 3 RGB, 4 RGBA),
// bpl bytes a row. Tesseract copies them, so the caller may free data after.
CB_EXPORT(cb_set_image) void cb_set_image(tesseract::TessBaseAPI *api, const unsigned char *data, int width,
                                          int height, int bpp, int bpl, int ppi) {
  api->SetImage(data, width, height, bpp, bpl);
  if (ppi > 0) api->SetSourceResolution(ppi);
}

// cb_get_text recognises the image and returns its UTF-8 text in reading
// order, to be released with cb_free_text; 0 on failure.
CB_EXPORT(cb_get_text) char *cb_get_text(tesseract::TessBaseAPI *api) { return api->GetUTF8Text(); }
CB_EXPORT(cb_free_text) void cb_free_text(char *t) { delete[] t; }

// cb_osd runs orientation and script detection (an engine initialised with
// "osd"). out receives, as int32: the rotation in degrees that makes the
// page upright, the orientation confidence x100, the script confidence x100,
// and a pointer to the script name (owned by the engine). Returns 1 on
// success.
CB_EXPORT(cb_osd) int cb_osd(tesseract::TessBaseAPI *api, int *out) {
  int deg = 0;
  float oc = 0, sc = 0;
  const char *script = nullptr;
  if (!api->DetectOrientationScript(&deg, &oc, &script, &sc)) return 0;
  out[0] = deg;
  out[1] = static_cast<int>(oc * 100);
  out[2] = static_cast<int>(sc * 100);
  out[3] = static_cast<int>(reinterpret_cast<intptr_t>(script));
  return 1;
}

CB_EXPORT(cb_clear) void cb_clear(tesseract::TessBaseAPI *api) { api->Clear(); }
CB_EXPORT(cb_delete) void cb_delete(tesseract::TessBaseAPI *api) {
  api->End();
  delete api;
}
