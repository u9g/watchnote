# Cross-compiles on the build host (no QEMU), so multi-arch builds are fast.
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/watchnote .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/watchnote /watchnote
ENV DATABASE_PATH=/data/watchnote.db \
    LISTEN_ADDR=:8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/watchnote"]
