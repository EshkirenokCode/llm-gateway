# ---- build stage -------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy manifests first so dependency download is cached independently
# of source changes.
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0 produces a static binary that runs on distroless/static.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gateway ./cmd/gateway

# ---- runtime stage -----------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /gateway

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/gateway"]
