# One Dockerfile, two binaries: the api and the worker differ only in which main they run,
# and building them from one image keeps their toolchain and dependencies identical by
# construction.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .

ARG CMD
RUN test -n "$CMD" || (echo "build-arg CMD is required (api|worker)" && exit 1)

# No `go mod download` step. go.mod carries tool directives for sqlc, goose and
# golangci-lint, so a bare download would pull their whole dependency tree -- hundreds of
# modules that no runtime binary imports. Building the one command instead fetches only
# what it actually needs.
#
# The cache mounts are what make a rebuild cheap; they replace the dependency layer that
# the removed download step used to provide, and they survive a source change.
# CGO off so the binary runs on a distroless static base with no libc at all.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/plimsoll ./cmd/${CMD}

# nonroot, static, no shell and no package manager: nothing to exec into if the process is
# ever compromised, and nothing to keep patched.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/plimsoll /plimsoll
USER nonroot:nonroot
ENTRYPOINT ["/plimsoll"]
