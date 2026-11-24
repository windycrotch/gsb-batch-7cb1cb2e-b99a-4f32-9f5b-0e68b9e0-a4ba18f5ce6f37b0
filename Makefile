CARGO   ?= cargo
GO      ?= go
CMAKE   ?= cmake

BACKEND ?= sim
MODELS  ?= models
PROFILE ?= balanced
WHISPER ?=
TRANSLATOR ?=
LISTEN  ?= 127.0.0.1:8080
SOCKET  ?= /tmp/ambivox-media.sock
SERVER_FLAGS ?=
BENCH_ARGS ?=

MEDIA_BIN  := $(CURDIR)/target/release/ambivox-media
SERVER_BIN := $(CURDIR)/bin/ambivox
FEATURES   := $(if $(filter sussurro,$(BACKEND)),--features sussurro,)
MODEL_ARGS := $(if $(filter sussurro,$(BACKEND)),--models $(MODELS) --profile $(PROFILE)$(if $(WHISPER), --whisper $(WHISPER))$(if $(TRANSLATOR), --translator $(TRANSLATOR)),)

# The bundled libopus build predates CMake 4's minimum policy version.
export CMAKE_POLICY_VERSION_MINIMUM ?= 3.5

.PHONY: all build media server models test test-engine test-rust test-go e2e offline lint check run lan bench browser-check clean

all: build

build: media server

media:
	$(CARGO) build --release -p ambivox-media $(FEATURES)

server:
	cd server && $(GO) build -trimpath -o $(SERVER_BIN) ./cmd/ambivox

models:
	AMBIVOX_MODELS=$(abspath $(MODELS)) AMBIVOX_PROFILE=$(PROFILE) AMBIVOX_WHISPER=$(WHISPER) AMBIVOX_TRANSLATOR=$(TRANSLATOR) scripts/setup-sussurro.sh

test: test-engine test-rust test-go

test-engine:
	$(CMAKE) -S engine -B build/engine -DCMAKE_BUILD_TYPE=Release
	$(CMAKE) --build build/engine --parallel
	ctest --test-dir build/engine --output-on-failure

test-rust:
	$(CARGO) test --workspace
	$(CARGO) test -p ambivox-dsp --no-default-features

test-go:
	cd server && $(GO) vet -tags e2e ./... && $(GO) test -race ./...

e2e: media
	cd server && AMBIVOX_MEDIA_BIN=$(MEDIA_BIN) $(GO) test -tags e2e -count=1 ./e2e/

offline: media
	cd server && $(GO) test -c -tags e2e -o $(CURDIR)/bin/e2e.test ./e2e/
	unshare -rn sh -c 'ip link set lo up && AMBIVOX_OFFLINE=1 AMBIVOX_MEDIA_BIN=$(MEDIA_BIN) $(CURDIR)/bin/e2e.test -test.count=1 -test.v'

lint:
	$(CARGO) fmt --all --check
	$(CARGO) clippy --workspace --all-targets -- -D warnings
	@unformatted="$$(gofmt -l server)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

check: lint test e2e
	@echo "ambivox: all checks passed"

run: build
	@trap 'kill 0' INT TERM EXIT; \
	$(MEDIA_BIN) serve --socket $(SOCKET) --backend $(BACKEND) $(MODEL_ARGS) & \
	$(SERVER_BIN) -listen $(LISTEN) -media-socket $(SOCKET) $(SERVER_FLAGS) & \
	wait

lan:
	$(MAKE) run LISTEN=:8443 SERVER_FLAGS="-tls-self-signed -udp-port 8443 -data-dir $(CURDIR)/.ambivox $(SERVER_FLAGS)"

bench: media
	$(MEDIA_BIN) bench --backend $(BACKEND) $(MODEL_ARGS) $(BENCH_ARGS)

browser-check: build
	cd scripts/browser-check && npm install --no-audit --no-fund && \
	MEDIA_BIN=$(MEDIA_BIN) SERVER_BIN=$(SERVER_BIN) node check.mjs

clean:
	$(CARGO) clean
	rm -rf build bin scripts/browser-check/node_modules
