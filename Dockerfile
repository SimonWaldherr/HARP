# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum go.work go.work.sum ./
COPY harp/go.mod harp/go.sum ./harp/
COPY harpserver/go.mod harpserver/go.sum ./harpserver/
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -trimpath -ldflags="-s -w" -o /out/harp-proxy .
RUN go build -trimpath -ldflags="-s -w" -o /out/harp-gateway ./cmd/harp-gateway
RUN go build -trimpath -ldflags="-s -w" -o /out/harpctl ./cmd/harpctl
RUN go build -trimpath -ldflags="-s -w" -o /out/demo-headers ./demos/headers-go
RUN go build -trimpath -ldflags="-s -w" -o /out/demo-webhook-catcher ./demos/webhook-catcher-go

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && \
    addgroup -S harp && \
    adduser -S -G harp -H -h /var/lib/harp harp && \
    mkdir -p /etc/harp /var/lib/harp/cache && \
    chown -R harp:harp /etc/harp /var/lib/harp

COPY --from=build /out/harp-proxy /usr/local/bin/harp-proxy
COPY --from=build /out/harp-gateway /usr/local/bin/harp-gateway
COPY --from=build /out/harpctl /usr/local/bin/harpctl
COPY --from=build /out/demo-headers /usr/local/bin/harp-demo-headers
COPY --from=build /out/demo-webhook-catcher /usr/local/bin/harp-demo-webhook-catcher
COPY deploy/configs/proxy.docker.json /etc/harp/config.json

USER harp
EXPOSE 50054 8080 9091
ENTRYPOINT ["/usr/local/bin/harp-proxy"]
CMD ["-config", "/etc/harp/config.json"]
