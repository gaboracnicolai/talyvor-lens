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

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/lens /lens
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/lens"]
