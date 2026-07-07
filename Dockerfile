# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -o /out/piperd ./cmd/piperd

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/piperd /usr/local/bin/piperd
ENV PIPER_DATA_DIR=/var/lib/piper
VOLUME /var/lib/piper
EXPOSE 80 443
ENTRYPOINT ["/usr/local/bin/piperd"]
