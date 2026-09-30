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

Подробная инструкция по установке на сервер появится в конце разработки.
