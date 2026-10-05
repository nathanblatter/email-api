# Two-stage build: tests + vet run inside the build stage, so an image cannot
# exist unless the suite passed. That is the CI gate; the workflow just builds.
FROM golang:1.26-alpine AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -z "$(gofmt -l .)" || (echo "gofmt: files need formatting:" && gofmt -l . && exit 1)
RUN go vet ./... && go test ./...
ARG VERSION=docker
RUN go build -ldflags="-s -w -X main.Version=${VERSION}" -o /email-api ./cmd/email-api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 app && mkdir -p /data && chown app /data
COPY --from=build /email-api /usr/local/bin/email-api
USER app
VOLUME /data
EXPOSE 4500
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:4500/health >/dev/null || exit 1
ENTRYPOINT ["email-api"]
