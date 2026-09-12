# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
# Pinned to the `go` directive in go.mod (1.26.0). The build stage is forced
# onto the *build* platform and cross-compiles via GOOS/GOARCH, so it never
# needs QEMU. The runtime stage below deliberately contains no RUN step either,
# which means a linux/arm64 image can be produced on an amd64 runner that has
# buildx but no binfmt handlers registered (the release workflow only runs
# setup-buildx-action).
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG TARGETOS
ARG TARGETARCH

# Stamped into the binary; the release workflow should pass
# --build-arg VERSION=${GITHUB_REF_NAME}.
ARG VERSION=dev

# Finding 19: the templ CLI generates code against the templ *runtime* library
# pinned in go.mod, and the two must match exactly. `@latest` here combined with
# `cache-to: type=gha,mode=max` in the release workflow meant this layer never
# invalidated while the library moved underneath it — that already produced
# `undefined: templ.ResolveAttributeValue` across six files. Bump this in
# lockstep with the github.com/a-h/templ require line in go.mod and with
# TEMPL_VERSION in the Makefile.
ARG TEMPL_VERSION=v0.3.1020
RUN go install github.com/a-h/templ/cmd/templ@${TEMPL_VERSION}

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN templ generate

# -trimpath keeps build-host paths out of the binary; -s -w drops the symbol
# table and DWARF, roughly a quarter of the output size.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/kontrolplane-feed ./cmd/server

# Prepared here rather than in the runtime stage precisely so the runtime stage
# stays RUN-free. Finding 18: docker-compose.yaml mounts a named volume at
# /data, a path the image did not previously contain, so docker created it
# root-owned and a bare USER switch would have broken SQLite writes at runtime.
# Shipping /data in the image already owned by uid 10001 makes docker seed the
# empty named volume from it, ownership included.
#
# This is a skeleton copied onto / rather than `COPY /out/data /data`, because
# --chown reliably applies to *copied entries* but not to destination
# directories BuildKit has to create implicitly.
RUN mkdir -p /skel/data

# ---- runtime --------------------------------------------------------------
# Alpine rather than distroless/scratch on purpose: HEALTHCHECK needs to make an
# HTTP request to /healthz, the binary has no -healthcheck flag, and a shell-less
# base leaves no way to run one. busybox wget is already in this base, costs
# ~3 MB over scratch, and keeps the healthcheck out of the application code.
FROM alpine:3.21

# Lifted from the build stage instead of `apk add ca-certificates` — identical
# bundle, but no RUN (and therefore no emulation) in this stage.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/kontrolplane-feed /usr/local/bin/kontrolplane-feed

# cmd/server/main.go serves static assets straight off disk with
# http.FileServer(http.Dir("static")), resolved relative to WORKDIR.
COPY --from=build /src/static /app/static

# templates/ is intentionally NOT copied: templ compiles .templ files to Go at
# build time and nothing reads them at runtime — shipping them only leaked source.

COPY --from=build --chown=10001:10001 /skel/ /

WORKDIR /app

# Numeric uid:gid rather than adduser, so no /etc/passwd mutation and no RUN.
USER 10001:10001

EXPOSE 8080

# Assumes the default PORT (8080). Override HEALTHCHECK or PORT together.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["kontrolplane-feed"]
