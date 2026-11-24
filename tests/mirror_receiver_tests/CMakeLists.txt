# Native lifecycle tests for the AirPlay mirror receiver (raop_rtp_mirror).
#
# These tests compile the REAL raop_rtp_mirror.c with the real socket and
# logging support, stubbing only the heavyweight dependencies (NTP clock
# machinery, OpenSSL-backed AES mirror buffer and libplist). They drive the
# receiver thread over loopback TCP and therefore need no iPad/Bonjour/GStreamer.
#
# Standalone usage:
#   cmake -S tests/mirror_receiver_tests -B tests/mirror_receiver_tests/build
#   cmake --build tests/mirror_receiver_tests/build
#   ctest --test-dir tests/mirror_receiver_tests/build --output-on-failure
# or simply:  bash tests/mirror_receiver_tests/build_and_test.sh

cmake_minimum_required(VERSION 3.13)
project(mirror_receiver_tests C)

set(CMAKE_C_STANDARD 99)
set(CMAKE_C_STANDARD_REQUIRED ON)

set(UXPLAY_LIB_DIR "${CMAKE_CURRENT_SOURCE_DIR}/../../core/lib")

add_executable(test_mirror_lifecycle
    test_mirror_lifecycle.c
    stubs/stubs_impl.c
    "${UXPLAY_LIB_DIR}/raop_rtp_mirror.c"
    "${UXPLAY_LIB_DIR}/netutils.c"
    "${UXPLAY_LIB_DIR}/logger.c"
    "${UXPLAY_LIB_DIR}/byteutils.c"
    "${UXPLAY_LIB_DIR}/utils.c"
    "${UXPLAY_LIB_DIR}/compat.c"
)

target_include_directories(test_mirror_lifecycle PRIVATE
    "${CMAKE_CURRENT_SOURCE_DIR}/stubs"
    "${UXPLAY_LIB_DIR}"
)

target_compile_options(test_mirror_lifecycle PRIVATE -Wall -Wextra -Wno-unused-parameter)

if(WIN32)
    target_link_libraries(test_mirror_lifecycle winpthread ws2_32)
else()
    find_package(Threads REQUIRED)
    target_link_libraries(test_mirror_lifecycle Threads::Threads)
endif()

enable_testing()
add_test(NAME mirror_lifecycle COMMAND test_mirror_lifecycle)
