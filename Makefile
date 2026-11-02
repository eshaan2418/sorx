# sorx: common entry points.
CAP ?= $(HOME)/quantfactory/_data/sorx/cap-20261102
.PHONY: build test race bench lint fuzz demo data replay sim clean

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

race:
	go test -race ./...

# Record 40 minutes of public L2 depth from 4 venues (no API keys; public market data only).
data: build
	bin/sorx record -dir $(CAP) -dur $${DUR:-40m}

replay: build
	bin/sorx replay -dir $(CAP) -out research/results/replay.json

# Strategy comparison (default assumptions, zero-latency, hypothetical fee schedule).
sim: build
	bin/sorx sim -dir $(CAP) -out research/results/sim_default.json
	bin/sorx sim -dir $(CAP) -zero-latency -out research/results/sim_zero_latency.json
	bin/sorx sim -dir $(CAP) -fees kraken=25,coinbase=40,bitstamp=30,gemini=35 -out research/results/sim_fees_hypothetical.json

bench: build
	bin/sorx bench -dir $(CAP) -out bench/results/bench.json

# Small committed sample capture.
demo: build
	bin/sorx replay -dir testdata/sample
	bin/sorx sim -dir testdata/sample -warmup 20s -every 10s -horizon 10s

fuzz:
	go test -run '^$$' -fuzz FuzzOMS -fuzztime $${FUZZTIME:-30s} ./internal/oms/
	go test -run '^$$' -fuzz FuzzDecoders -fuzztime $${FUZZTIME:-30s} ./internal/mm/venue/

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

clean:
	rm -rf bin
