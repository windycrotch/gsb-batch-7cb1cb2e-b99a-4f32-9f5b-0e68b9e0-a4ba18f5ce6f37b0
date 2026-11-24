/*
 * External definitions for the lightweight test stubs. The corresponding
 * headers (raop_ntp.h, mirror_buffer.h, plist/plist.h) shadow the production
 * headers on the include path; this file provides the link symbols without
 * pulling in OpenSSL, the real NTP machinery or libplist.
 */

#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdbool.h>

#include "raop_ntp.h"
#include "mirror_buffer.h"
#include "plist/plist.h"
#include "logger.h"

/* ---- NTP timing stub: pass timestamps through unchanged -------------- */

uint64_t raop_ntp_timestamp_to_nano_seconds(raop_ntp_t *raop_ntp, uint64_t timestamp)
{
    (void) raop_ntp;
    return timestamp;
}

uint64_t raop_ntp_adjust_remote_timestamp_offset(raop_ntp_t *raop_ntp,
                                                 uint64_t ntp_timestamp_raw,
                                                 bool add_secs_1900_to_1970)
{
    (void) raop_ntp;
    (void) add_secs_1900_to_1970;
    return ntp_timestamp_raw;
}

uint64_t raop_ntp_get_local_time(void)
{
    return 123456789ULL;
}

uint64_t raop_ntp_convert_remote_time(raop_ntp_t *raop_ntp, uint64_t remote_time)
{
    (void) raop_ntp;
    return remote_time;
}

void raop_ntp_set_video_arrival_offset(raop_ntp_t *raop_ntp, const uint64_t *offset)
{
    (void) raop_ntp;
    (void) offset;
}

/* ---- mirror buffer stub: no crypto, identity copy -------------------- */

mirror_buffer_t *mirror_buffer_init(logger_t *logger, const unsigned char *aeskey)
{
    (void) logger;
    (void) aeskey;
    return (mirror_buffer_t *) calloc(1, sizeof(mirror_buffer_t));
}

void mirror_buffer_init_aes(mirror_buffer_t *mirror_buffer, const uint64_t *streamConnectionID)
{
    (void) mirror_buffer;
    (void) streamConnectionID;
}

void mirror_buffer_decrypt(mirror_buffer_t *mirror_buffer, unsigned char *input,
                           unsigned char *output, int datalen)
{
    (void) mirror_buffer;
    memmove(output, input, (size_t) datalen);
}

void mirror_buffer_destroy(mirror_buffer_t *mirror_buffer)
{
    free(mirror_buffer);
}

/* ---- libplist stub: only reachable when client FPS reporting is on ---- */

void plist_from_bin(const char *buf, uint32_t len, plist_t *node)
{
    (void) buf;
    (void) len;
    if (node) {
        *node = NULL;
    }
}

void plist_to_xml(plist_t node, char **xml, uint32_t *len)
{
    (void) node;
    if (xml) {
        *xml = NULL;
    }
    if (len) {
        *len = 0;
    }
}

void plist_free(plist_t node)
{
    (void) node;
}
