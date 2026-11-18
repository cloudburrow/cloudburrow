/* WASI (wasip1) has no processes and no temporary files. Leptonica's
 * utils2.c refers to system() and mkstemp() for its debug display and
 * temporary-file helpers, which Tesseract never calls on this path; these
 * fail as a host without them would. */
#include <errno.h>

int system(const char *command) {
  (void)command;
  errno = ENOSYS;
  return -1;
}

int mkstemp(char *template_) {
  (void)template_;
  errno = ENOSYS;
  return -1;
}
