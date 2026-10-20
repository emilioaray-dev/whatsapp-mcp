# Bridge Go (whatsmeow) para correr en Kubernetes. mattn/go-sqlite3 exige CGO, por eso no es un
# build estático con scratch. El estado (sesión + mensajes) vive en /data (volumen persistente).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/whatsapp-bridge .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 10001 wa && useradd -u 10001 -g 10001 -d /data -s /usr/sbin/nologin wa \
 && mkdir -p /data && chown 10001:10001 /data
COPY --from=build /out/whatsapp-bridge /usr/local/bin/whatsapp-bridge
USER 10001:10001
WORKDIR /data
# El bridge escribe `store/` relativo al cwd; la API REST no tiene autenticación: en el pod se
# bindea a loopback (WA_BRIDGE_ADDR) detrás del gateway.
ENV WA_BRIDGE_ADDR=127.0.0.1:8080
ENTRYPOINT ["/usr/local/bin/whatsapp-bridge"]
