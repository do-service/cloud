# Upload benchmark client (E3-9, bench/README.md): benchupload on the
# compose network next to "cloud", shaping its own egress with netem.
# Build context is the repo root, as for the app's image.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -o /out/benchupload ./cmd/benchupload

FROM alpine:3.22
# tc for netem; root, because shaping needs NET_ADMIN in its own netns.
RUN apk add --no-cache iproute2
COPY --from=build /out/benchupload /usr/local/bin/benchupload
COPY --chmod=755 bench/upload/shape.sh /usr/local/bin/shape
ENTRYPOINT ["shape", "benchupload", "-url", "http://cloud:8080"]
