# Builds all three binaries into one image. Each Railway service picks the one
# it runs via its start command.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off so the binaries run on a distroless base with no libc.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    go build -trimpath -ldflags="-s -w" -o /out/crawler ./cmd/crawler && \
    go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# distroless static: no shell, no package manager, non-root by default, and it
# carries the CA certificates needed to fetch feeds over HTTPS.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/api /out/crawler /out/migrate /app/

USER nonroot:nonroot
EXPOSE 8000

CMD ["/app/api"]
