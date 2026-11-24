# syntax=docker/dockerfile:1

FROM rust:1-trixie AS media
RUN apt-get update \
 && apt-get install -y --no-install-recommends cmake \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY Cargo.toml Cargo.lock LICENSE ./
COPY .cargo .cargo
COPY crates crates
COPY engine engine
COPY scripts/setup-sussurro.sh scripts/collect-licenses.sh scripts/
COPY scripts/licenses scripts/licenses
RUN scripts/setup-sussurro.sh sherpa
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    set -eu; \
    AMBIVOX_PORTABLE=1 cargo build --release --locked -p ambivox-media --features sussurro; \
    mkdir -p /out/bin /out/lib; \
    cp target/release/ambivox-media /out/bin/; \
    cp -L "$(find target/release/build -path '*/out/lib/libambivox_engine.so' -print -quit)" /out/lib/; \
    cp -L third_party/sherpa-onnx/lib/*.so /out/lib/; \
    scripts/collect-licenses.sh media /out/licenses

FROM golang:1-trixie AS server
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server ./
COPY scripts/collect-licenses.sh /src/scripts/
RUN CGO_ENABLED=0 go build -trimpath -o /out/bin/ambivox ./cmd/ambivox \
 && /src/scripts/collect-licenses.sh server /out/licenses

FROM python:3.13-slim-trixie AS models
RUN apt-get update \
 && apt-get install -y --no-install-recommends bzip2 ca-certificates curl git \
 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cpu torch \
 && pip install --no-cache-dir gguf numpy sentencepiece transformers
COPY scripts/setup-sussurro.sh /opt/ambivox/scripts/
ENV AMBIVOX_CONVERT_PYTHON=python3 \
    AMBIVOX_MODELS=/models \
    HF_HOME=/tmp/huggingface
RUN /opt/ambivox/scripts/setup-sussurro.sh converter \
 && useradd --system --uid 10001 --home-dir /models ambivox \
 && mkdir -p /models \
 && chown ambivox /models
LABEL org.opencontainers.image.source="https://github.com/whispem/ambivox" \
      org.opencontainers.image.description="Downloads and prepares the models ambivox needs for real speech." \
      org.opencontainers.image.licenses="MIT"
USER ambivox
ENTRYPOINT ["/opt/ambivox/scripts/setup-sussurro.sh"]
CMD ["voices", "whisper", "translation"]

FROM debian:trixie-slim AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgomp1 \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --home-dir /data ambivox \
 && mkdir -p /data /run/ambivox /models \
 && chown ambivox /data /run/ambivox /models
COPY --from=media /out/lib/ /usr/local/lib/ambivox/
COPY --from=media /out/bin/ /usr/local/bin/
COPY --from=server /out/bin/ /usr/local/bin/
COPY --from=media /out/licenses/ /usr/share/licenses/ambivox/
COPY --from=server /out/licenses/ /usr/share/licenses/ambivox/
LABEL org.opencontainers.image.source="https://github.com/whispem/ambivox" \
      org.opencontainers.image.description="Self-hosted translated voice calls." \
      org.opencontainers.image.licenses="MIT"
ENV LD_LIBRARY_PATH=/usr/local/lib/ambivox \
    AMBIVOX_LISTEN=:8443 \
    AMBIVOX_MEDIA_SOCKET=/run/ambivox/media.sock \
    AMBIVOX_DATA_DIR=/data \
    AMBIVOX_TLS_SELF_SIGNED=1 \
    AMBIVOX_UDP_PORT=8443
USER ambivox
EXPOSE 8443/tcp 8443/udp
CMD ["ambivox"]
