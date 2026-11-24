# sms
sheepbao media server and super media server whith golang.

## Features
* modularization, expansibility
* high performance, easy deployment, cross-platform
* support rtmp 
* support http-flv
* support hls
* support push or pull media streams by http request
* support flv dvr (save rtmp stream to flv file)

## Build

```
git clone https://github.com/sheepbao/sms.git  
cd sms  
./build.sh  
```

## Cross compile
```
./build.sh linux arm // or ./build.sh os arch
```
## Run
./bin/sms

## Test
The project uses the GOPATH layout (`src/vendor`), so tests run with
`GO111MODULE=off` and `GOPATH` set to the repository root. `test.sh` does both:
```
./test.sh
```
It runs `go vet` and the unit/integration tests (with the race detector) for
RTMP stream takeover, HLS segment-cache handover, and the concurrent-map helpers.


