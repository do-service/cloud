VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/do-service/cloud/backend/internal/version.Version=$(VERSION)

.PHONY: fmt lint test test-e2e test-webdav build run bench-gen bench-server bench-listing bench-workday \
	bench-upload-image bench-upload bench-upload-resume bench-upload-restart

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt found unformatted files (run 'make fmt')" && exit 1)
	go vet ./...

# -p 1: packages share one test database.
test:
	go test ./... -count=1 -p 1

# Sign-in against a real Keycloak: `docker compose --profile e2e up -d
# keycloak` first. In the default run the test skips itself.
test-e2e:
	E2E_KEYCLOAK_URL=$${E2E_KEYCLOAK_URL:-http://localhost:8180} go test ./internal/auth -run 'E2E$$' -count=1 -v

# WebDAV compatibility (E7-5, scripts/webdav-compat.sh), as CI's
# backend:webdav runs it: a server from the compose image on a throwaway
# database and data dir, so the dev account's trash stays clean. Needs
# `docker compose up -d` (postgres and the built image).
WEBDAV_DB = cloud_webdav
WEBDAV_PSQL = $(COMPOSE) exec -T postgres psql -q -U cloud -d postgres
WEBDAV_ENV = BASE_URL=http://cloud-webdav:8080 USERNAME=webdav PASSWORD=webdav-compat
test-webdav:
	@docker rm -f cloud-webdav >/dev/null 2>&1 || true
	@$(WEBDAV_PSQL) -c 'DROP DATABASE IF EXISTS $(WEBDAV_DB) WITH (FORCE)' -c 'CREATE DATABASE $(WEBDAV_DB)'
	@$(COMPOSE) run -d --no-deps --name cloud-webdav \
		-e DATABASE_URL=postgres://cloud:cloud@postgres:5432/$(WEBDAV_DB)?sslmode=disable \
		-e DATA_DIR=/tmp/webdav-data -e ADMIN_USERNAME=webdav -e ADMIN_PASSWORD=webdav-compat \
		-e WOPI_DISCOVERY_URL= cloud >/dev/null
	@docker run --rm --network cloud_default -v "$(CURDIR)/scripts:/scripts:ro" $(addprefix -e ,$(WEBDAV_ENV)) \
		debian:trixie-slim sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends curl ca-certificates litmus rclone >/dev/null && /scripts/webdav-compat.sh'; \
		status=$$?; [ $$status = 0 ] || docker logs cloud-webdav 2>&1 | tail -50; \
		docker rm -f cloud-webdav >/dev/null; $(WEBDAV_PSQL) -c 'DROP DATABASE $(WEBDAV_DB) WITH (FORCE)'; exit $$status

build:
	go build -ldflags "$(LDFLAGS)" -o bin/server ./cmd/server

# :8090/:9091 locally — chat's dev backend holds :8080 on this machine, and
# the frontend's vite proxy expects this one on :8090.
run:
	HTTP_ADDR=$${HTTP_ADDR:-:8090} METRICS_ADDR=$${METRICS_ADDR-:9091} go run -ldflags "$(LDFLAGS)" ./cmd/server

# Benchmark stand (bench/README.md), on its own database: benchgen refuses
# one that already has users, so the dev data in "cloud" is safe anyway.
BENCH_DATABASE_URL ?= postgres://cloud:cloud@localhost:55433/cloud_bench?sslmode=disable
BENCH_PORT ?= 8091
# k6 honours HTTP(S)_PROXY, and a proxy in front of localhost would be
# measured along with the server.
K6 = env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy BASE_URL=http://localhost:$(BENCH_PORT) k6 run

# Full size by default (20M nodes, 2 000 users); BENCH_FLAGS='-nodes 300000'
# for a quick one.
bench-gen:
	go run ./cmd/benchgen -database-url '$(BENCH_DATABASE_URL)' -tokens ../bench/tokens.jsonl $(BENCH_FLAGS)

bench-server:
	DATABASE_URL='$(BENCH_DATABASE_URL)' HTTP_ADDR=:$(BENCH_PORT) METRICS_ADDR= DATA_DIR=bin/bench-data \
		PUBLIC_BASE_URL=http://localhost:$(BENCH_PORT) go run -ldflags "$(LDFLAGS)" ./cmd/server

bench-listing:
	$(K6) ../bench/k6/listing-50k.js

bench-workday:
	$(K6) ../bench/k6/workday.js

# Upload benchmark (E3-9): benchupload in a container next to "cloud",
# behind netem at RATE (1gbit) and each RTT. Needs SESSION, or BENCH_USER
# and BENCH_PASSWORD, in the environment (bench/README.md);
# UPLOAD_FLAGS='-size 4GiB' for a smaller file. The client never uses a
# proxy, so HTTP(S)_PROXY doesn't matter here.
COMPOSE = docker compose -f ../docker-compose.yml
BENCH_UPLOAD = $(COMPOSE) --profile bench run --rm -T
RTTS ?= 0 20ms 50ms
RTT ?= 0
# The restart comes once this much has reached the server's staging dir.
RESTART_AT_MB ?= 2000

bench-upload-image:
	$(COMPOSE) --profile bench build bench-upload

bench-upload: bench-upload-image
	@for rtt in $(RTTS); do \
		$(BENCH_UPLOAD) -e RTT=$$rtt bench-upload -label rtt=$$rtt $(UPLOAD_FLAGS) || exit 1; \
	done

# The connection drops mid-chunk halfway through; the client resumes from
# the server's offset.
bench-upload-resume: bench-upload-image
	$(BENCH_UPLOAD) -e RTT=$(RTT) bench-upload -label break-at=0.5 -break-at 0.5 $(UPLOAD_FLAGS)

# The server restarts mid-upload; the client rides it out with retries and
# resumes from the offset the server kept on disk.
bench-upload-restart: bench-upload-image
	@$(BENCH_UPLOAD) -e RTT=$(RTT) bench-upload -label restart $(UPLOAD_FLAGS) & client=$$!; \
	until [ "$$($(COMPOSE) exec -T cloud du -sm /data/uploads/tus | cut -f1)" -ge $(RESTART_AT_MB) ]; do \
		kill -0 $$client 2>/dev/null || { wait $$client; exit 1; }; sleep 1; \
	done; \
	echo "--- restarting cloud" >&2; $(COMPOSE) restart cloud; \
	wait $$client
