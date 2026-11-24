/*
 * Test stub for mirror_buffer (the AES-CTR decryption helper).
 * The lifecycle tests never feed valid encrypted video, so the buffer only
 * needs to exist and be destroyable; decrypt() copies input to output to
 * keep any packet processing that reaches it well defined.
 */
#ifndef TEST_STUB_MIRROR_BUFFER_H
#define TEST_STUB_MIRROR_BUFFER_H

#include <stdint.h>
#include "logger.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef struct mirror_buffer_s {
    int dummy;
} mirror_buffer_t;

mirror_buffer_t *mirror_buffer_init(logger_t *logger, const unsigned char *aeskey);
void mirror_buffer_init_aes(mirror_buffer_t *mirror_buffer, const uint64_t *streamConnectionID);
void mirror_buffer_decrypt(mirror_buffer_t *mirror_buffer, unsigned char *input,
                           unsigned char *output, int datalen);
void mirror_buffer_destroy(mirror_buffer_t *mirror_buffer);

#ifdef __cplusplus
}
#endif

#endif /* TEST_STUB_MIRROR_BUFFER_H */
