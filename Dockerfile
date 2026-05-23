# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS builder
WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -trimpath -ldflags="-w -s" -o /bin/lens ./cmd/lens

# Alpine runtime gives us busybox wget for the docker-compose healthcheck
# while keeping the image small (~10 MB) and ca-certificates available
# for outbound TLS to the LLM providers. We still drop to a non-root
# user so distroless's hardening profile is largely preserved.
FROM alpine:3.19
RUN apk add --no-cache ca-certificates wget && \
    addgroup -S lens && adduser -S -G lens -u 65532 lens
COPY --from=builder /bin/lens /lens
USER lens
EXPOSE 8080
ENTRYPOINT ["/lens"]
