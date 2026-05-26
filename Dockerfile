# syntax=docker/dockerfile:1.7

# ─── builder ─────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
    -o /out/anamnesia ./cmd/anamnesia

# ─── runtime ─────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/anamnesia /usr/local/bin/anamnesia
ENV ANAMNESIA_HTTP_ADDR=":8181"
EXPOSE 8181
ENTRYPOINT ["anamnesia"]
CMD ["serve"]
