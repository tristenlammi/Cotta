# Build: a single static binary with the frontend embedded.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cotta ./cmd/cotta \
 && mkdir /out/data

# Run: no shell, no package manager, non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cotta /cotta
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV COTTA_DATA=/data COTTA_ADDR=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/cotta", "healthcheck"]
ENTRYPOINT ["/cotta"]
