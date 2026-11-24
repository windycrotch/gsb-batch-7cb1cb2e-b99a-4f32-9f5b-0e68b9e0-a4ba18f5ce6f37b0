/*
 * Minimal libplist stub for the mirror receiver unit tests.
 * The production mirror code only touches plist when the optional client
 * FPS reporting switch is enabled; the tests keep it disabled, so these are
 * inert declarations sufficient to compile/link without a real libplist.
 */
#ifndef TEST_STUB_PLIST_H
#define TEST_STUB_PLIST_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct plist_s *plist_t;

void plist_from_bin(const char *buf, uint32_t len, plist_t *node);
void plist_to_xml(plist_t node, char **xml, uint32_t *len);
void plist_free(plist_t node);

#ifdef __cplusplus
}
#endif

#endif /* TEST_STUB_PLIST_H */
