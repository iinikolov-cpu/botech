# Этап 1: сборка статического бинарника (без CGO).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot \
 && mkdir /out/data

# Этап 2: минимальный образ. Внутри только бинарник, сертификаты и папка данных.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/bot /bot
USER 65532:65532
ENV DB_PATH=/data/bot.db
ENTRYPOINT ["/bot"]
