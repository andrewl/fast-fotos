FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /fast-fotos ./cmd/fast-fotos

FROM debian:bookworm-slim
RUN apt-get update && apt-get install --no-install-recommends -y ca-certificates curl gosu libgomp1 && rm -rf /var/lib/apt/lists/*
RUN useradd --create-home --uid 10001 appuser
COPY --from=build /fast-fotos /usr/local/bin/fast-fotos
COPY scripts/install-geonames.sh /usr/local/bin/install-geonames.sh
COPY scripts/install-onnx.sh /usr/local/bin/install-onnx.sh
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
