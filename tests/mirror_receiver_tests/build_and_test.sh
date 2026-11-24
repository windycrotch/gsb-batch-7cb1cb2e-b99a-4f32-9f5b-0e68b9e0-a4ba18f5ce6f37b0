#!/usr/bin/env bash
#
# Builds and runs the native mirror-receiver lifecycle tests without any
# Apple hardware, libplist or OpenSSL: the test links the real
# raop_rtp_mirror.c together with the real socket/logger/byteutils support
# code and lightweight stubs for the NTP, AES and plist dependencies.
#
# Works with the MSYS2/MinGW toolchain used to build UxPlay on Windows and
# with plain gcc/pthread on Linux/macOS.
#
# Usage: bash build_and_test.sh [debug]
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
LIB_DIR="${REPO_ROOT}/core/lib"
BUILD_DIR="${SCRIPT_DIR}/build"

mkdir -p "${BUILD_DIR}"

# Locate a compiler (allow CC override from the environment).
CC_BIN="${CC:-gcc}"

WARN_FLAGS="-Wall -Wextra -Wno-unused-parameter"
if [[ "${1:-}" == "debug" ]]; then
    OPT_FLAGS="-O0 -g -DDEBUG"
else
    OPT_FLAGS="-O2 -g"
fi

# -I stubs first so the stub raop_ntp.h / mirror_buffer.h / plist/plist.h
# shadow the production headers that pull in OpenSSL and the real NTP code.
INCLUDES="-I${SCRIPT_DIR}/stubs -I${LIB_DIR}"

SOURCES=(
    "${LIB_DIR}/raop_rtp_mirror.c"
    "${LIB_DIR}/netutils.c"
    "${LIB_DIR}/logger.c"
    "${LIB_DIR}/byteutils.c"
    "${LIB_DIR}/utils.c"
    "${LIB_DIR}/compat.c"
    "${SCRIPT_DIR}/stubs/stubs_impl.c"
    "${SCRIPT_DIR}/test_mirror_lifecycle.c"
)

LIBS="-lpthread"
case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*)
        LIBS="-lwinpthread -lws2_32"
        ;;
esac

echo "Compiling mirror receiver tests with ${CC_BIN}..."
"${CC_BIN}" ${WARN_FLAGS} ${OPT_FLAGS} ${INCLUDES} \
    "${SOURCES[@]}" \
    -o "${BUILD_DIR}/test_mirror_lifecycle" \
    ${LIBS}

echo "Build OK: ${BUILD_DIR}/test_mirror_lifecycle"
echo
echo "Running tests..."
"${BUILD_DIR}/test_mirror_lifecycle"
