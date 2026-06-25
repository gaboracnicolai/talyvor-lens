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

# distill-worker is the killable, memory-limited conversion subprocess that
# distill.ProcessIsolator spawns (the stage-3 resource-isolation envelope). It
# ships in the image beside /lens so the default worker path resolves with no
# config. Nothing on the serving path spawns it yet — this just makes the binary
# available for the request-path integration that lands in a later PR.
RUN go build -trimpath -ldflags="-w -s" -o /bin/distill-worker ./cmd/distill-worker

# cmd/node is the PoVI inference-node daemon (registers, serves via its provider, signs + submits
# receipts, answers challenges). Shipped beside /lens for the CLOSED-TEST harness
# (docker-compose.trial.yaml runs /node). Not part of a standard gateway deploy — operator/edge-infra
# runs nodes separately in production.
RUN go build -trimpath -ldflags="-w -s" -o /bin/node ./cmd/node

# Alpine runtime gives us busybox wget for the docker-compose healthcheck
# while keeping the image small (~10 MB) and ca-certificates available
# for outbound TLS to the LLM providers. We still drop to a non-root
# user so distroless's hardening profile is largely preserved.
FROM alpine:3.19
RUN apk add --no-cache ca-certificates wget && \
    addgroup -S lens && adduser -S -G lens -u 65532 lens
COPY --from=builder /bin/lens /lens
COPY --from=builder /bin/distill-worker /distill-worker
COPY --from=builder /bin/node /node
USER lens
EXPOSE 8080
ENTRYPOINT ["/lens"]
