# syntax=docker/dockerfile:1
# Image of the service: a static binary on distroless (no shell, no package manager,
# user nonroot). The same binary runs "serve" in the Deployment and "migrate up" in the Job.
FROM golang:1.26-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOPRIVATE=github.com/fin-assist/*
COPY go.mod go.sum ./
# Private fin-assist modules: pass a token as the build secret "netrc"
# (machine github.com login x-access-token password <token>); public ones need nothing.
RUN --mount=type=secret,id=netrc,target=/root/.netrc,required=false \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/user ./cmd/user

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/user /user
USER nonroot:nonroot
EXPOSE 9090 9091
ENTRYPOINT ["/user"]
CMD ["serve"]
