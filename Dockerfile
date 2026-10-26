# One image for the whole product: the SPA is built first and embedded into
# the Go binary (backend/internal/webui), so an admin runs a single container
# next to Postgres — no separate nginx/static server to configure.

# The build stages run on the builder's own platform and the Go one
# cross-compiles to the target: a multi-arch build (release workflow) then
# doesn't compile under QEMU, which is many times slower. The bundle is
# plain JS, the same for every target.
FROM --platform=$BUILDPLATFORM node:24-alpine AS frontend
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json frontend/.npmrc ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=frontend /src/dist/ ./internal/webui/dist/
ARG VERSION=dev
ARG TARGETOS TARGETARCH
# CGO off: previews run the vips command (runtime stage), not libvips
# linked in, so the binary stays static — and cross-compiles without a C
# toolchain for the target.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -ldflags "-s -w -X github.com/do-service/cloud/backend/internal/version.Version=${VERSION}" \
    -o /out/server ./cmd/server

FROM alpine:3.22
# vips makes previews (R6): HEIC through libheif, PDF through poppler,
# which needs fonts to draw text. Without it previews are just off.
RUN apk add --no-cache vips-tools vips-heif vips-poppler font-dejavu
# /data is created here owned by the app user so a fresh named volume
# mounted on it inherits that ownership.
RUN adduser -S -D -H -u 10001 app \
    && mkdir -p /data \
    && chown 10001 /data
ENV DATA_DIR=/data \
    HTTP_ADDR=:8080 \
    METRICS_ADDR=:9090
COPY --from=backend /out/server /usr/local/bin/server
USER 10001
EXPOSE 8080 9090
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/readyz >/dev/null || exit 1
ENTRYPOINT ["server"]
