# Ballast container image.
#
# Build:  docker build --build-arg VERSION=v0.7.0 -t ballast .
# Run:    see RELEASING.md and docs/systemd.md for volumes and the Docker socket.
#
# The web UI (web/dist) is committed, so no Node stage is needed.

FROM golang:1.27-alpine AS build
ARG VERSION=dev
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/ballast ./cmd/ballast

# Ballast shells out to restic and the docker CLI. Database dumps run with
# `docker exec` inside the service's own container, so pg_dump and friends
# are not needed here.
FROM alpine:3.22
RUN apk add --no-cache restic docker-cli ca-certificates tzdata
COPY --from=build /out/ballast /usr/local/bin/ballast

# Ballast needs root to use the Docker socket and read service data,
# so the image runs as root (same as the systemd unit).
ENV BALLAST_CONFIG_DIR=/etc/ballast \
    BALLAST_DATA_DIR=/var/lib/ballast \
    XDG_CACHE_HOME=/var/cache/ballast
VOLUME ["/etc/ballast", "/var/lib/ballast"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ballast"]
CMD ["serve"]
