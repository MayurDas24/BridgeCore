# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.22-bookworm AS build

WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/bridgecore-api ./cmd/api

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/bridgecore-seed ./cmd/seed

# ---- Runtime stage ----
FROM alpine:3.19 AS runtime

RUN apk add --no-cache ca-certificates wget && \
    addgroup -S bridgecore && adduser -S bridgecore -G bridgecore

WORKDIR /app

COPY --from=build /out/bridgecore-api /app/bridgecore-api
COPY --from=build /out/bridgecore-seed /app/bridgecore-seed
COPY --from=build /src/docs /app/docs

ENV APP_PORT=8080
EXPOSE 8080

USER bridgecore

ENTRYPOINT ["/app/bridgecore-api"]
