/*
 * Test stub for the NTP timing service used by raop_rtp_mirror.
 * Timestamps are passed through unchanged so packet ordering in the tests
 * is deterministic; none of the lifecycle logic under test depends on the
 * real clock-sync machinery.
 */
#ifndef TEST_STUB_RAOP_NTP_H
#define TEST_STUB_RAOP_NTP_H

#include <stdint.h>
#include <stdbool.h>
#include <sys/types.h>

typedef struct raop_ntp_s raop_ntp_t;

typedef enum timing_protocol_e { NTP, TP_NONE, TP_OTHER, TP_UNSPECIFIED } timing_protocol_t;

#ifdef __cplusplus
extern "C" {
#endif

uint64_t raop_ntp_timestamp_to_nano_seconds(raop_ntp_t *raop_ntp, uint64_t timestamp);
uint64_t raop_ntp_adjust_remote_timestamp_offset(raop_ntp_t *raop_ntp,
                                                 uint64_t ntp_timestamp_raw,
                                                 bool add_secs_1900_to_1970);
uint64_t raop_ntp_get_local_time(void);
uint64_t raop_ntp_convert_remote_time(raop_ntp_t *raop_ntp, uint64_t remote_time);
void raop_ntp_set_video_arrival_offset(raop_ntp_t *raop_ntp, const uint64_t *offset);

#ifdef __cplusplus
}
#endif

#endif /* TEST_STUB_RAOP_NTP_H */
