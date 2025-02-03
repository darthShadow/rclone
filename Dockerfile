# Mounts of type=cache are not exported:
# RUN --mount=type=cache,target=/root/.cache/go-build
# They can possible be re-used by separating the import & export stages:
# https://github.com/docker/buildx/issues/244#issuecomment-602750160
# Further references:
# https://github.com/FerretDB/FerretDB/issues/2170
# https://github.com/moby/buildkit/issues/1512

FROM golang:alpine AS builder

ARG CGO_ENABLED=0

WORKDIR /go/src/github.com/rclone/rclone/

RUN echo "**** Set Go Environment Variables ****" && \
    go env -w GOCACHE=/root/.cache/go-build

RUN echo "**** Install Dependencies ****" && \
    apk add --no-cache \
        make \
        bash \
        gawk \
        git

RUN --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    echo "**** Download Go Dependencies ****" && \
    go mod download -x && \
    echo "**** Verify Go Dependencies ****" && \
    go mod verify

# The written content will not be persisted back to the host
RUN --mount=type=bind,source=.,target=.,rw \
    echo "**** Build Binary ****" && \
    make && \
    cp -vf ./rclone /tmp/rclone

RUN echo "**** Print Version Binary ****" && \
    /tmp/rclone version

# Begin final image
FROM alpine:latest

RUN echo "**** Install Dependencies ****" && \
    apk add --no-cache \
        ca-certificates \
        fuse3 \
        tzdata && \
    echo "Enable user_allow_other in fuse" && \
    echo "user_allow_other" >> /etc/fuse.conf

COPY --from=builder /tmp/rclone /usr/local/bin/

RUN addgroup -g 1009 rclone && adduser -u 1009 -Ds /bin/sh -G rclone rclone

ENTRYPOINT [ "rclone" ]

WORKDIR /data
ENV XDG_CONFIG_HOME=/config
