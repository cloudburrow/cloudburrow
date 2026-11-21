/* CloudBurrow's stand-in for the os_support.h RNNoise 0.2's vec.h includes
 * on its portable (no SSE, no NEON) path, which the release tarball does not
 * ship: the one macro that path uses, as Opus's os_support.h defines it. */
#ifndef CB_OS_SUPPORT_H
#define CB_OS_SUPPORT_H
#include <string.h>
#define OPUS_CLEAR(dst, n) (memset((dst), 0, (n)*sizeof(*(dst))))
#endif
