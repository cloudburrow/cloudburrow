/* Leptonica configuration for CloudBurrow's WASI build: no image codec
 * libraries. Tesseract receives raw pixels (cb_set_image), so Leptonica
 * never reads or writes a compressed image file. */
#define HAVE_LIBJPEG 0
#define HAVE_LIBTIFF 0
#define HAVE_LIBPNG 0
#define HAVE_LIBZ 0
#define HAVE_LIBGIF 0
#define HAVE_LIBUNGIF 0
#define HAVE_LIBWEBP 0
#define HAVE_LIBWEBP_ANIM 0
#define HAVE_LIBJP2K 0
#define HAVE_FMEMOPEN 1
#define HAVE_FSTATAT 0
#define HAVE_DIRFD 0
