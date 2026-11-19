# syntax=docker/dockerfile:1
# The build stage runs on the builder's own platform and cross-compiles, so
# multi-arch images build fast without emulating Go.
FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/openmailwire ./cmd/openmailwire

FROM alpine:3
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 openmailwire
USER openmailwire
WORKDIR /home/openmailwire
COPY --from=build /out/openmailwire /usr/local/bin/openmailwire
# Inside a container the control plane must listen on all interfaces; the
# binary's own default is loopback.
ENV OMW_LISTEN=:8080
ENTRYPOINT ["openmailwire"]
CMD ["help"]
