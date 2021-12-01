FROM golang:1.16 as builder
LABEL stage=builder
WORKDIR /build

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# copy just enough of the git repo to parse HEAD, used to record version in OLM binaries
COPY .git/HEAD .git/HEAD
COPY .git/refs/heads/. .git/refs/heads
RUN mkdir -p .git/objects
COPY Makefile Makefile
COPY OLM_VERSION OLM_VERSION
COPY pkg pkg
COPY cmd cmd
COPY util util
COPY test test
RUN CGO_ENABLED=0 MOD_FLAGS="" make build
RUN make build-util

# use debug tag to keep a shell around for backwards compatibility with the previous alpine image
FROM gcr.io/distroless/static:debug
LABEL stage=olm
WORKDIR /
# bundle unpack Jobs require cp at /bin/cp
RUN ["/busybox/ln", "-s", "/busybox/cp", "/bin/cp"]
COPY --from=builder /build/bin/olm /bin/olm
COPY --from=builder /build/bin/catalog /bin/catalog
COPY --from=builder /build/bin/package-server /bin/package-server
COPY --from=builder /build/bin/cpb /bin/cpb
EXPOSE 8080
EXPOSE 5443
CMD ["/bin/olm"]
