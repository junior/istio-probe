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
COPY --from=build /probe /probe
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/probe"]
