# ============================================================
# Stage 1: Build
# ============================================================
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -trimpath \
    -o /bin/agentmail \
    ./cmd/server

# ============================================================
# Stage 2: Runtime
# ============================================================
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl \
    && addgroup -g 1000 -S appgroup \
    && adduser -u 1000 -S appuser -G appgroup

COPY --from=builder /bin/agentmail /bin/agentmail

USER appuser:appgroup

EXPOSE 8080

ENTRYPOINT ["/bin/agentmail"]
