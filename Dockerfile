FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /uhf-server-tvh-proxy .

FROM alpine:3.23

RUN apk add --no-cache ca-certificates \
    && addgroup -S proxy \
    && adduser -S -G proxy proxy \
    && mkdir /data \
    && chown proxy:proxy /data

COPY --from=build /uhf-server-tvh-proxy /usr/local/bin/uhf-server-tvh-proxy

USER proxy
EXPOSE 8000
VOLUME ["/data"]
ENTRYPOINT ["uhf-server-tvh-proxy"]
