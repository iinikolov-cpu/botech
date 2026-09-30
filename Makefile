.PHONY: run test build lint docker

# Запуск локально (переменные берутся из файла .env)
run:
	set -a; . ./.env; set +a; go run ./cmd/bot

# Тесты (с проверкой гонок)
test:
	go test -race ./...

# Сборка бинарника в bin/bot
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/bot ./cmd/bot

lint:
	go vet ./...

# Запуск в Docker одной командой
docker:
	docker compose up -d --build
