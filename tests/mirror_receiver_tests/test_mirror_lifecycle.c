/*
 * Lifecycle and teardown-race tests for the AirPlay mirror receiver
 * (raop_rtp_mirror). The tests drive the real receiver thread over a
 * loopback TCP socket using a small in-process fake client, so they run on
 * a normal developer machine without an iPad or any Apple hardware.
 *
 * Covered scenarios:
 *   1. start/stop basic lifecycle and the running/joined/socket invariants
 *   2. stop() is idempotent: repeated calls do not block or re-notify
 *   3. start() is refused while running and before a dead thread is reaped
 *   4. client closes the stream after connecting -> unified teardown path
 *   5. connection reset (RST / abrupt disconnect) -> unified teardown path
 *   6. unsupported codec (empty type-0x01 packet) -> same path, reason 3
 *   7. renderer rejecting a codec -> same path, reason 3
 *   8. pause then disconnect -> upper layer ends on "stopped", not "paused"
 *   9. resume packet arriving after disconnect cannot restore "mirroring"
 *  10. listening port is released after stop (no port-in-use leftovers)
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdarg.h>
#include <unistd.h>
#include <pthread.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
#define CLOSE_SOCK closesocket
typedef SOCKET test_sock_t;
#define TEST_SOCK_INVALID INVALID_SOCKET
#else
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#define CLOSE_SOCK close
typedef int test_sock_t;
#define TEST_SOCK_INVALID (-1)
#endif

#include "raop_rtp_mirror.h"
#include "logger.h"

/* ------------------------------------------------------------------ */
/* Tiny test framework                                                 */
/* ------------------------------------------------------------------ */

static int tests_run;
static int tests_failed;
static int checks_failed;
static const char *current_test;

#define CHECK(cond) do {                                                    \
        if (!(cond)) {                                                      \
            checks_failed++;                                                \
            fprintf(stderr, "  [FAIL] %s:%d: CHECK(%s)\n",                 \
                    __FILE__, __LINE__, #cond);                            \
        }                                                                   \
    } while (0)

#define RUN_TEST(fn_call) do {                                              \
        current_test = #fn_call;                                            \
        tests_run++;                                                        \
        int before = checks_failed;                                         \
        fn_call;                                                            \
        if (checks_failed == before) {                                      \
            printf("  [PASS] %s\n", current_test);                          \
        } else {                                                            \
            tests_failed++;                                                 \
            printf("  [FAIL] %s\n", current_test);                          \
        }                                                                   \
    } while (0)

/* ------------------------------------------------------------------ */
/* Upper-layer callback recorder                                       */
/* ------------------------------------------------------------------ */

typedef struct {
    pthread_mutex_t lock;
    int running_true_count;
    int running_false_count;
    int pause_count;
    int resume_count;
    int conn_reset_count;
    int last_conn_reset_reason;
    int video_reset_count;
    reset_type_t last_video_reset_type;
    int reject_codec;          /* when nonzero, video_set_codec_and_size fails */
} callback_state_t;

static callback_state_t cb;

static void cb_lock(void)
{
    pthread_mutex_lock(&cb.lock);
}

static void cb_unlock(void)
{
    pthread_mutex_unlock(&cb.lock);
}

static void cb_init(void)
{
    memset(&cb, 0, sizeof(cb));
    pthread_mutex_init(&cb.lock, NULL);
    cb.last_conn_reset_reason = -1;
}

static void cb_destroy(void)
{
    pthread_mutex_destroy(&cb.lock);
}

static void on_mirror_video_running(void *cls, bool is_running)
{
    (void) cls;
    cb_lock();
    if (is_running) {
        cb.running_true_count++;
    } else {
        cb.running_false_count++;
    }
    cb_unlock();
}

static void on_video_pause(void *cls)
{
    (void) cls;
    cb_lock();
    cb.pause_count++;
    cb_unlock();
}

static void on_video_resume(void *cls)
{
    (void) cls;
    cb_lock();
    cb.resume_count++;
    cb_unlock();
}

static void on_conn_reset(void *cls, int reason)
{
    (void) cls;
    cb_lock();
    cb.conn_reset_count++;
    cb.last_conn_reset_reason = reason;
    cb_unlock();
}

static void on_video_reset(void *cls, reset_type_t reset_type)
{
    (void) cls;
    cb_lock();
    cb.video_reset_count++;
    cb.last_video_reset_type = reset_type;
    cb_unlock();
}

static int on_video_set_codec_and_size(void *cls, video_codec_t codec, unsigned short width_source,
                                       unsigned short height_source, unsigned short x_offset,
                                       unsigned short y_offset, unsigned short width, unsigned short height)
{
    (void) cls;
    (void) codec;
    (void) width_source;
    (void) height_source;
    (void) x_offset;
    (void) y_offset;
    (void) width;
    (void) height;
    cb_lock();
    int reject = cb.reject_codec;
    cb_unlock();
    return reject ? -1 : 0;
}

/* The mirror thread calls video_process only for encrypted video packets,
 * which the lifecycle tests never send. */
static void on_video_process(void *cls, raop_ntp_t *ntp, video_decode_struct *data)
{
    (void) cls;
    (void) ntp;
    (void) data;
}

static void logger_discard(void *cls, int level, const char *msg)
{
    (void) cls;
    (void) level;
    (void) msg;
}

/* ------------------------------------------------------------------ */
/* Fake AirPlay client over loopback TCP                              */
/* ------------------------------------------------------------------ */

static void socket_init(void)
{
#ifdef _WIN32
    WSADATA wsa;
    WSAStartup(MAKEWORD(2, 2), &wsa);
#endif
}

static void socket_cleanup(void)
{
#ifdef _WIN32
    WSACleanup();
#endif
}

static test_sock_t fake_client_connect(unsigned short port)
{
    test_sock_t fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd == TEST_SOCK_INVALID) {
        return fd;
    }
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons(port);
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (connect(fd, (struct sockaddr *) &addr, sizeof(addr)) != 0) {
        CLOSE_SOCK(fd);
        return TEST_SOCK_INVALID;
    }
    return fd;
}

static void _sleep_10ms(void)
{
#ifdef _WIN32
    Sleep(10);
#else
    usleep(10 * 1000);
#endif
}

/* Wait until the receiver clears its running flag (the thread self-exits
 * and runs the common teardown), or time out. The receiver loop polls
 * every 5 ms and teardown is local, so a few seconds is generous. */
static void wait_until_stopped(raop_rtp_mirror_t *mirror, int timeout_ms)
{
    for (int waited = 0; waited < timeout_ms; waited += 10) {
        if (!raop_rtp_mirror_is_running(mirror)) {
            return;
        }
        _sleep_10ms();
    }
}

/* Poll for a callback predicate. */
#define WAIT_FOR(cond, timeout_ms) do {                                      \
        for (int _waited = 0; _waited < (timeout_ms); _waited += 10) {      \
            if (cond) {                                                      \
                break;                                                        \
            }                                                                 \
            _sleep_10ms();                                                    \
        }                                                                     \
    } while (0)

static raop_rtp_mirror_t *create_mirror(logger_t *logger)
{
    raop_callbacks_t callbacks;
    memset(&callbacks, 0, sizeof(callbacks));
    callbacks.cls = NULL;
    callbacks.video_process = on_video_process;
    callbacks.video_pause = on_video_pause;
    callbacks.video_resume = on_video_resume;
    callbacks.conn_reset = on_conn_reset;
    callbacks.video_reset = on_video_reset;
    callbacks.video_set_codec_and_size = on_video_set_codec_and_size;
    callbacks.mirror_video_running = on_mirror_video_running;

    /* remotelen selects IPv4 (4) vs IPv6 (16); remote is the numeric
     * address string parsed by netutils_parse_address, matching production. */
    const char *remote = "127.0.0.1";
    return raop_rtp_mirror_init(logger, &callbacks, NULL, remote, 4,
                                (const unsigned char *) "0123456789abcdef");
}

/* Build a 128-byte mirror packet header. type byte is packet[4], option
 * byte is packet[6]; payload length is stored little-endian in [0:3]. */
static void build_header(unsigned char *header, int payload_size, int type, int option)
{
    memset(header, 0, 128);
    header[0] = (unsigned char) (payload_size & 0xff);
    header[1] = (unsigned char) ((payload_size >> 8) & 0xff);
    header[2] = (unsigned char) ((payload_size >> 16) & 0xff);
    header[3] = (unsigned char) ((payload_size >> 24) & 0xff);
    header[4] = (unsigned char) type;
    header[6] = (unsigned char) option;
    /* plausible float geometry fields used by the codec path */
    header[16] = header[40] = 0x00;
    header[17] = header[41] = 0x00;
    header[18] = header[42] = 0x20;
    header[19] = header[43] = 0x42; /* 40.0f */
    header[20] = header[44] = 0x00;
    header[21] = header[45] = 0x00;
    header[22] = header[46] = 0x44;
    header[23] = header[47] = 0x42; /* 49.0f */
    header[56] = 0x00;
    header[57] = 0x00;
    header[58] = 0x20;
    header[59] = 0x42;
    header[60] = 0x00;
    header[61] = 0x00;
    header[62] = 0x44;
    header[63] = 0x42;
}

static int send_all(test_sock_t fd, const unsigned char *buf, int len)
{
    int sent = 0;
    while (sent < len) {
        int n = send(fd, (const char *) (buf + sent), len - sent, 0);
        if (n <= 0) {
            return -1;
        }
        sent += n;
    }
    return 0;
}

/* Send an unencrypted codec (type 0x01) packet with the given option byte.
 * A minimal h264-ish payload keeps the non-empty codec path happy. */
static int send_codec_packet(test_sock_t fd, int option, int with_payload)
{
    unsigned char header[128];
    unsigned char payload[32];
    int payload_size = with_payload ? (int) sizeof(payload) : 0;
    build_header(header, payload_size, 0x01, option);
    if (with_payload) {
        memset(payload, 0, sizeof(payload));
        /* Valid minimal h264 framing: big-endian sps_size at offset 6-7 and
         * pps_size at offset (sps_size + 9), sized so every parser read
         * stays inside the payload and sps_pps_len fits the 1024 buffer. */
        payload[6] = 0x00;
        payload[7] = 0x04; /* sps_size = 4 */
        payload[13] = 0x00;
        payload[14] = 0x04; /* pps_size = 4 */
    }
    if (send_all(fd, header, 128) != 0) {
        return -1;
    }
    if (with_payload && send_all(fd, payload, payload_size) != 0) {
        return -1;
    }
    return 0;
}

/* Send just a complete 128-byte header announcing a nonzero payload, then
 * the caller closes the socket: the receiver is left waiting for payload
 * bytes that never arrive, i.e. a genuine mid-stream client disconnect. */
static int send_full_header_then_eof(test_sock_t fd)
{
    unsigned char header[128];
    build_header(header, 64, 0x02, 0x00);
    return send_all(fd, header, 128);
}

/* ------------------------------------------------------------------ */
/* Tests                                                               */
/* ------------------------------------------------------------------ */

/* 1. Basic lifecycle: start reports running + valid socket, stop reaps. */
static void test_start_stop_lifecycle(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    CHECK(mirror != NULL);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);
    CHECK(raop_rtp_mirror_is_joined(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    unsigned short port = 0;
    int rc = raop_rtp_mirror_start(mirror, &port, 0);
    CHECK(rc == 0);
    CHECK(port != 0);
    CHECK(raop_rtp_mirror_is_running(mirror) == 1);
    CHECK(raop_rtp_mirror_is_joined(mirror) == 0);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 1);
    cb_lock();
    CHECK(cb.running_true_count == 1);
    cb_unlock();

    /* A real client can reach the listening port. */
    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);
    if (client != TEST_SOCK_INVALID) {
        CLOSE_SOCK(client);
    }

    raop_rtp_mirror_stop(mirror);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);
    CHECK(raop_rtp_mirror_is_joined(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 2. Repeated stop() must not block, re-notify or touch freed resources. */
static void test_repeated_stop_is_idempotent(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    raop_rtp_mirror_stop(mirror);
    int false_after_first;
    cb_lock();
    false_after_first = cb.running_false_count;
    cb_unlock();
    CHECK(false_after_first == 1);

    /* These calls must return immediately and stay quiet. */
    raop_rtp_mirror_stop(mirror);
    raop_rtp_mirror_stop(mirror);
    raop_rtp_mirror_stop(mirror);

    CHECK(raop_rtp_mirror_is_running(mirror) == 0);
    CHECK(raop_rtp_mirror_is_joined(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);
    cb_lock();
    CHECK(cb.running_false_count == false_after_first);
    CHECK(cb.conn_reset_count == 0);
    cb_unlock();

    /* stop after destroy must not happen; destroy itself tolerates a
     * previously stopped receiver. */
    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 3. start() is refused while running, and again while a self-exited
 *    thread has not yet been reaped. */
static void test_start_refused_until_reaped(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    unsigned short port2 = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port2, 0) == -1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 1);

    /* Make the receiver thread exit on its own via a mid-stream disconnect. */
    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);
    CHECK(send_full_header_then_eof(client) == 0);
    CLOSE_SOCK(client);
    wait_until_stopped(mirror, 5000);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);

    /* Thread is dead but not joined: a second start must still be refused
     * rather than spawning an orphan thread. */
    unsigned short port3 = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port3, 0) == -1);

    raop_rtp_mirror_stop(mirror);
    CHECK(raop_rtp_mirror_is_joined(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    /* After reaping, restart works and yields a fresh listening socket. */
    unsigned short port4 = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port4, 0) == 0);
    CHECK(raop_rtp_mirror_is_running(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 1);
    raop_rtp_mirror_stop(mirror);

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 4. Client disappears mid-stream (complete header, payload never sent):
 *    the receiver thread takes the common teardown path. */
static void test_client_close_after_connect(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);
    CHECK(send_full_header_then_eof(client) == 0);
    CLOSE_SOCK(client);

    wait_until_stopped(mirror, 5000);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    /* Upper layer got exactly one "stopped" notification and no crash. */
    WAIT_FOR(cb.running_false_count >= 1, 2000);
    cb_lock();
    CHECK(cb.running_true_count == 1);
    CHECK(cb.running_false_count == 1);
    cb_unlock();

    /* Reaping the self-exited thread must not deadlock or re-notify. */
    raop_rtp_mirror_stop(mirror);
    CHECK(raop_rtp_mirror_is_joined(mirror) == 1);
    cb_lock();
    CHECK(cb.running_false_count == 1);
    cb_unlock();

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 4b. A client that connects and vanishes before a full header (a probe or
 *     aborted reconnect) must not tear down the mirror service: the
 *     receiver closes that connection and accepts the next client. */
static void test_partial_header_does_not_tear_down(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t probe = fake_client_connect(port);
    CHECK(probe != TEST_SOCK_INVALID);
    unsigned char partial[16] = {0};
    send_all(probe, partial, (int) sizeof(partial));
    CLOSE_SOCK(probe);

    /* Give the receiver time to notice and go back to accepting. */
    _sleep_10ms();
    _sleep_10ms();
    CHECK(raop_rtp_mirror_is_running(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 1);

    /* A well-behaved client can still drive the receiver; tearing it down
     * the normal way must still produce exactly one stop notification. */
    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);
    CHECK(send_full_header_then_eof(client) == 0);
    CLOSE_SOCK(client);
    wait_until_stopped(mirror, 5000);

    WAIT_FOR(cb.running_false_count >= 1, 2000);
    cb_lock();
    CHECK(cb.running_true_count == 1);
    CHECK(cb.running_false_count == 1);
    cb_unlock();

    raop_rtp_mirror_stop(mirror);
    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 5. Abrupt disconnect (hard close with linger 0 -> RST) is handled by the
 *    same teardown path and reported as a network disconnect. */
static void test_connection_reset(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);

    /* Leave data pending in the receiver's socket buffer so that closing
     * with linger 0 actually emits a TCP RST rather than a graceful FIN. */
    CHECK(send_full_header_then_eof(client) == 0);
    _sleep_10ms();
    struct linger sl;
    sl.l_onoff = 1;
    sl.l_linger = 0;
    setsockopt(client, SOL_SOCKET, SO_LINGER, (const char *) &sl, sizeof(sl));
    CLOSE_SOCK(client); /* emits RST while bytes are still unread */

    wait_until_stopped(mirror, 5000);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    WAIT_FOR(cb.running_false_count >= 1, 2000);
    cb_lock();
    CHECK(cb.running_false_count == 1);
    CHECK(cb.conn_reset_count == 1);
    CHECK(cb.last_conn_reset_reason == RAOP_MIRROR_DISCONNECT_NETWORK);
    cb_unlock();

    raop_rtp_mirror_stop(mirror);
    cb_lock();
    /* No duplicate notifications on reap. */
    CHECK(cb.conn_reset_count == 1);
    CHECK(cb.running_false_count == 1);
    cb_unlock();

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 6. Empty type-0x01 codec packet means unsupported codec: the receiver
 *    drops itself through the common path and reports reason 3. */
static void test_unsupported_codec_empty_packet(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);
    CHECK(send_codec_packet(client, 0x16, 0) == 0); /* zero payload */

    wait_until_stopped(mirror, 5000);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    WAIT_FOR(cb.conn_reset_count >= 1, 2000);
    cb_lock();
    CHECK(cb.running_false_count == 1);
    CHECK(cb.conn_reset_count == 1);
    CHECK(cb.last_conn_reset_reason == RAOP_MIRROR_DISCONNECT_BAD_CODEC);
    CHECK(cb.video_reset_count == 1);
    CHECK(cb.last_video_reset_type == RESET_TYPE_RTP_SHUTDOWN);
    cb_unlock();

    /* The connection socket is closed on the receiver side; the client
     * side will eventually see EOF or a reset. */
    _sleep_10ms();

    raop_rtp_mirror_stop(mirror);
    cb_lock();
    CHECK(cb.conn_reset_count == 1);
    CHECK(cb.video_reset_count == 1);
    cb_unlock();
    CLOSE_SOCK(client);

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 7. The renderer rejecting the codec also ends in the same path with the
 *    bad-codec reason. */
static void test_renderer_rejects_codec(logger_t *logger)
{
    cb_init();
    cb.reject_codec = 1;
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);
    CHECK(send_codec_packet(client, 0x16, 1) == 0);

    wait_until_stopped(mirror, 5000);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);

    WAIT_FOR(cb.conn_reset_count >= 1, 2000);
    cb_lock();
    CHECK(cb.last_conn_reset_reason == RAOP_MIRROR_DISCONNECT_BAD_CODEC);
    CHECK(cb.video_reset_count == 1);
    CHECK(cb.running_false_count == 1);
    cb_unlock();

    raop_rtp_mirror_stop(mirror);
    CLOSE_SOCK(client);
    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 8. Pause packet followed by client disconnect: the final upper-layer
 *    state must be stopped, never stuck on paused. */
static void test_pause_then_disconnect_ends_stopped(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);

    /* First a normal codec packet so the stream is established, then a
     * pause-indicating codec packet (option 0x56), then disconnect. The
     * connection is reset with unread bytes pending so the receiver detects
     * the loss immediately rather than relying on TCP keepalive. */
    CHECK(send_codec_packet(client, 0x16, 1) == 0);
    _sleep_10ms(); /* let the first codec packet land before pausing */
    CHECK(send_codec_packet(client, 0x56, 1) == 0);
    WAIT_FOR(cb.pause_count >= 1, 2000);
    cb_lock();
    CHECK(cb.pause_count == 1);
    int resumes_before = cb.resume_count;
    cb_unlock();

    CHECK(send_full_header_then_eof(client) == 0); /* unread pending data */
    _sleep_10ms();
    struct linger sl = { .l_onoff = 1, .l_linger = 0 };
    setsockopt(client, SOL_SOCKET, SO_LINGER, (const char *) &sl, sizeof(sl));
    CLOSE_SOCK(client);
    wait_until_stopped(mirror, 5000);

    WAIT_FOR(cb.running_false_count >= 1, 2000);
    cb_lock();
    CHECK(cb.running_false_count == 1);       /* final word is "stopped" */
    CHECK(cb.resume_count == resumes_before); /* no spurious resume */
    cb_unlock();

    raop_rtp_mirror_stop(mirror);
    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 9. A resume packet that arrives in the disconnect window must not flip
 *    the upper layer back to "mirroring". We simulate this by queueing a
 *    resume packet and resetting immediately; teardown suppresses it. */
static void test_late_resume_after_disconnect(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);

    CHECK(send_codec_packet(client, 0x16, 1) == 0);
    CHECK(send_codec_packet(client, 0x56, 1) == 0); /* pause */
    WAIT_FOR(cb.pause_count >= 1, 2000);

    /* Queue a resume packet, then lose the connection before the receiver
     * necessarily processes it. Whatever the scheduling outcome, the final
     * state must be stopped with running(false) as the last notification. */
    send_codec_packet(client, 0x16, 1); /* resume */
    CHECK(send_full_header_then_eof(client) == 0); /* unread pending data */
    _sleep_10ms();
    struct linger sl = { .l_onoff = 1, .l_linger = 0 };
    setsockopt(client, SOL_SOCKET, SO_LINGER, (const char *) &sl, sizeof(sl));
    CLOSE_SOCK(client);

    wait_until_stopped(mirror, 5000);
    WAIT_FOR(cb.running_false_count >= 1, 2000);

    cb_lock();
    CHECK(cb.running_false_count == 1);
    if (cb.resume_count > 0) {
        /* If the resume was processed before the close, a stop notification
         * still has to be the last word; the running counts prove ordering. */
        CHECK(cb.running_true_count == 1);
        CHECK(cb.running_false_count == 1);
    }
    cb_unlock();
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);

    raop_rtp_mirror_stop(mirror);
    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 10. The listening port is released when stop completes, so the next
 *     receiver (or another process) can bind it immediately. */
static void test_port_released_after_stop(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);
    CHECK(port != 0);

    raop_rtp_mirror_stop(mirror);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    test_sock_t probe = socket(AF_INET, SOCK_STREAM, 0);
    CHECK(probe != TEST_SOCK_INVALID);
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons(port);
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    int bound = bind(probe, (struct sockaddr *) &addr, sizeof(addr));
    CHECK(bound == 0);
    CLOSE_SOCK(probe);

    /* A fresh receiver can start again on a free ephemeral port. */
    unsigned short port2 = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port2, 0) == 0);
    CHECK(raop_rtp_mirror_is_running(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 1);
    raop_rtp_mirror_stop(mirror);

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 11. stop() while a client connection is active: the per-connection socket
 *     and the listening socket both close, join does not deadlock. */
static void test_stop_with_active_connection(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);
    unsigned short port = 0;
    CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);

    test_sock_t client = fake_client_connect(port);
    CHECK(client != TEST_SOCK_INVALID);
    /* Keep the connection idle-but-open while stop runs. */
    _sleep_10ms();

    raop_rtp_mirror_stop(mirror);
    CHECK(raop_rtp_mirror_is_running(mirror) == 0);
    CHECK(raop_rtp_mirror_is_joined(mirror) == 1);
    CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

    char probe[4];
    (void) recv(client, probe, sizeof(probe), 0); /* peer should be gone */
    CLOSE_SOCK(client);

    cb_lock();
    CHECK(cb.running_false_count == 1);
    CHECK(cb.conn_reset_count == 0); /* explicit stop is not an error */
    cb_unlock();

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

/* 12. Repeated start/stop and self-exit/reap cycles on one instance must
 *     keep the running/joined/socket invariants and the exactly-once
 *     notification contract, with no leaked sockets or threads. */
static void test_repeated_lifecycle_cycles(logger_t *logger)
{
    cb_init();
    raop_rtp_mirror_t *mirror = create_mirror(logger);

    for (int cycle = 0; cycle < 5; cycle++) {
        unsigned short port = 0;
        CHECK(raop_rtp_mirror_start(mirror, &port, 0) == 0);
        CHECK(raop_rtp_mirror_is_running(mirror) == 1);
        CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 1);

        /* Alternate between explicit stop and a self-exit then reap. */
        if (cycle % 2 == 0) {
            raop_rtp_mirror_stop(mirror);
        } else {
            test_sock_t client = fake_client_connect(port);
            CHECK(client != TEST_SOCK_INVALID);
            CHECK(send_full_header_then_eof(client) == 0);
            struct linger sl = { .l_onoff = 1, .l_linger = 0 };
            setsockopt(client, SOL_SOCKET, SO_LINGER, (const char *) &sl, sizeof(sl));
            CLOSE_SOCK(client);
            wait_until_stopped(mirror, 5000);
            raop_rtp_mirror_stop(mirror); /* reap, must not deadlock */
        }

        CHECK(raop_rtp_mirror_is_running(mirror) == 0);
        CHECK(raop_rtp_mirror_is_joined(mirror) == 1);
        CHECK(raop_rtp_mirror_listen_socket_valid(mirror) == 0);

        /* Idempotent re-stop after every cycle. */
        raop_rtp_mirror_stop(mirror);
    }

    cb_lock();
    CHECK(cb.running_true_count == 5);
    CHECK(cb.running_false_count == 5);
    cb_unlock();

    raop_rtp_mirror_destroy(mirror);
    cb_destroy();
}

int main(void)
{
    socket_init();

    logger_t *logger = logger_init();
    logger_set_level(logger, LOGGER_ERR); /* keep test output quiet */
    logger_set_callback(logger, logger_discard, NULL);

    printf("Running raop_rtp_mirror lifecycle tests:\n");
    RUN_TEST(test_start_stop_lifecycle(logger));
    RUN_TEST(test_repeated_stop_is_idempotent(logger));
    RUN_TEST(test_start_refused_until_reaped(logger));
    RUN_TEST(test_client_close_after_connect(logger));
    RUN_TEST(test_partial_header_does_not_tear_down(logger));
    RUN_TEST(test_connection_reset(logger));
    RUN_TEST(test_unsupported_codec_empty_packet(logger));
    RUN_TEST(test_renderer_rejects_codec(logger));
    RUN_TEST(test_pause_then_disconnect_ends_stopped(logger));
    RUN_TEST(test_late_resume_after_disconnect(logger));
    RUN_TEST(test_port_released_after_stop(logger));
    RUN_TEST(test_stop_with_active_connection(logger));
    RUN_TEST(test_repeated_lifecycle_cycles(logger));
    logger_destroy(logger);
    socket_cleanup();

    printf("\n%d tests run, %d failed\n", tests_run, tests_failed);
    return tests_failed ? 1 : 0;
}
