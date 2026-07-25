FROM registry.access.redhat.com/ubi9/go-toolset:1.26 AS builder
ARG GO_INIT_VERSION="1.1.0"
COPY go.mod go.mod
COPY main.go main.go
# Build a static binary so it can be copied into any image, and trim local
# build paths out of the result so the build is reproducible.
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X 'main.versionString=${GO_INIT_VERSION}'" \
      -o go-init .

FROM registry.access.redhat.com/ubi9/ubi-micro:9.8

COPY --from=builder /opt/app-root/src/go-init /usr/bin/go-init
ENTRYPOINT [ "/usr/bin/go-init" ]
