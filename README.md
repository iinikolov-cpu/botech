# Бот «Тайный покупатель»

Telegram-бот для раздачи заданий тайным покупателям. Go, SQLite, один бинарник.

## Быстрый запуск (Docker)

```bash
cp .env.example .env     # впишите BOT_TOKEN и FIRST_ADMIN_ID
make docker              # или: docker compose up -d --build
docker compose logs -f   # посмотреть логи
```

## Локально без Docker

```bash
cp .env.example .env
make run
make test
```

Полная инструкция для новичков (установка, работа, обновление, копии базы, проблемы): [docs/DEPLOY.md](docs/DEPLOY.md). Подробные проверки по этапам: [docs/ZAPUSK.md](docs/ZAPUSK.md).
