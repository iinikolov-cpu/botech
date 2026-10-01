// Команда bot запускает Telegram-бота «Тайный покупатель».
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // встроенная база часовых поясов: не нужна в образе

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/config"
	"botech/internal/handlers"
	"botech/internal/scheduler"
	"botech/internal/service"
	"botech/internal/storage/sqlite"
)

func main() {
	if err := run(); err != nil {
		slog.Error("бот остановлен с ошибкой", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	// Остановка по Ctrl+C / docker stop: даём горутинам завершиться корректно.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := sqlite.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	access := service.NewAccess(store, cfg.FirstAdminID)
	if err := access.EnsureFirstAdmin(ctx); err != nil {
		return err
	}

	tasks := service.NewTasks(store, cfg.PromoMaxUses)
	settings := service.NewSettings(store)
	app := handlers.New(handlers.Services{
		Access:    access,
		Scenarios: service.NewScenarios(store),
		Tasks:     tasks,
		Dialog:    service.NewDialog(store),
		Promos:    service.NewPromos(store, cfg.PromoMaxUses),
		Reports:   service.NewReports(store, tasks),
		Reminders: service.NewReminders(store, tasks, settings, cfg.Location),
		Settings:  settings,
		Backups:   service.NewBackups(store, cfg.BackupDir, cfg.BackupKeep),

		BackupChatID: cfg.FirstAdminID,
	}, log, cfg.Location)
	b, err := bot.New(cfg.BotToken,
		bot.WithMiddlewares(app.Middleware),
		bot.WithDefaultHandler(app.DefaultHandler),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
		bot.WithErrorsHandler(func(err error) {
			// Токен не должен попадать в логи, даже если он есть в тексте ошибки.
			log.Error("ошибка Telegram-клиента", "err", strings.ReplaceAll(err.Error(), cfg.BotToken, "***"))
		}),
	)
	if err != nil {
		return err
	}

	me, err := b.GetMe(ctx)
	if err != nil {
		return err
	}
	app.Register(b, me.Username)

	_, _ = b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: []models.BotCommand{
		{Command: "start", Description: "Главное меню"},
		{Command: "tasks", Description: "Мои задания"},
		{Command: "help", Description: "Справка"},
	}})

	// Плановые задания работают внутри приложения: просрочка и напоминания каждую минуту,
	// проверка времени ежедневного бэкапа каждые 5 минут. Состояние хранится в БД и переживает рестарт.
	wait := scheduler.Start(ctx, log,
		scheduler.Job{Name: "напоминания", Every: time.Minute, Run: func(c context.Context) error { return app.RunReminders(c, b) }},
		scheduler.Job{Name: "бэкап", Every: 5 * time.Minute, Run: func(c context.Context) error { return app.RunBackup(c, b) }},
	)

	log.Info("бот запущен", "username", me.Username)
	b.Start(ctx) // блокируется до сигнала остановки
	wait()       // дожидаемся завершения плановых заданий до закрытия базы
	log.Info("бот остановлен")
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
