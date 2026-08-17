# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.22-bookworm AS build

WORKDIR /src

# Module downloads are cached in their own layer, so a source-only change does
# not re-download the dependency graph on every build.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG BUILD_TIME=unknown
ARG GIT_COMMIT=unknown

# CGO_ENABLED=0 produces a static binary, which is what makes the distroless
# runtime stage possible. -trimpath keeps build paths out of the binary, and
# -s -w drops the symbol table and DWARF data.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bridgecore-api    ./cmd/api  && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bridgecore-worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bridgecore-seed   ./cmd/seed

# ---- Runtime stage ----
# Distroless rather than Alpine: no shell, no package manager, no busybox. If
# the API is ever compromised, there is nothing in the image to pivot with —
# an attacker cannot curl a payload or spawn /bin/sh, because neither exists.
# The nonroot variant runs as UID 65532 with no way to escalate.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app

COPY --from=build /out/bridgecore-api    /app/bridgecore-api
COPY --from=build /out/bridgecore-worker /app/bridgecore-worker
COPY --from=build /out/bridgecore-seed   /app/bridgecore-seed
COPY --from=build /src/docs              /app/docs
COPY --from=build /src/graph/schema      /app/graph/schema

ENV APP_PORT=8080
EXPOSE 8080

USER nonroot:nonroot

# No HEALTHCHECK instruction: the health check lives in the ECS task
# definition and the ALB target group, which are what actually act on it.
# A distroless image has no shell or wget to run one with anyway.
ENTRYPOINT ["/app/bridgecore-api"]
