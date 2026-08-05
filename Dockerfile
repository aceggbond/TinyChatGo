FROM golang:1.24-alpine AS build
WORKDIR /src
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /tinychatgo-server .

FROM alpine:3.22
RUN addgroup -S tinychatgo && adduser -S -G tinychatgo tinychatgo
COPY --from=build /tinychatgo-server /usr/local/bin/tinychatgo-server
RUN mkdir -p /data && chown tinychatgo:tinychatgo /data
USER tinychatgo
VOLUME ["/data"]
EXPOSE 8080 8443 8081
ENTRYPOINT ["/usr/local/bin/tinychatgo-server", "-data-dir", "/data", "-listen", ":8080"]
