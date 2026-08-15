# Build stage: CGO is required (mattn/go-sqlite3 with the vendored SQLite
# amalgamation), and the sqlite_fts5 tag is mandatory - without it the binary
# fails at startup on the logs migration (no such module: fts5).
FROM golang:1.26-alpine AS build

RUN apk add --no-cache build-base

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -ldflags="-s -w" -o /pooml .

# Runtime stage: migrations, templates, and the logo are embedded in the
# binary, so the image is just the binary plus TLS roots.
FROM alpine:latest

RUN apk add --no-cache ca-certificates && \
    addgroup -g 1001 -S pooml && \
    adduser -u 1001 -S pooml -G pooml && \
    mkdir -p /data && chown pooml:pooml /data

WORKDIR /app
COPY --from=build /pooml .
USER pooml

EXPOSE 8080 8081

ENV POOML_ENV=pro \
    POOML_DB_DIR=/data \
    POOML_API_ADDR=0.0.0.0:8080 \
    POOML_UI_ADDR=0.0.0.0:8081

VOLUME /data

CMD ["./pooml"]
