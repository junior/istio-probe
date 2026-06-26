# syntax=docker/dockerfile:1

# --- build a static binary ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.appVersion=${VERSION} -X main.buildTime=${BUILD_TIME}" \
    -o /probe .

# --- minimal, non-root runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="istio-probe" \
      org.opencontainers.image.description="In-cluster diagnostics page to test & validate Istio after an upgrade" \
      org.opencontainers.image.authors="Adao Oliveira Jr" \
      org.opencontainers.image.url="https://adao.dev" \
      org.opencontainers.image.source="https://github.com/junior/istio-probe" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /probe /probe
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/probe"]
