# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.2-trixie AS build
ARG GOAMD64=v1 TARGETOS TARGETARCH
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOAMD64=$GOAMD64 \
    go build -trimpath -ldflags="-s" -o /out/app .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
