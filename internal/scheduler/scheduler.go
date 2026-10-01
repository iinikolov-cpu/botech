// Пакет scheduler запускает периодические задания внутри приложения (без внешних сервисов).
// Что именно выполняется, решает вызывающий код; здесь только аккуратный запуск по таймеру.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Job периодическое задание.
type Job struct {
	Name  string
	Every time.Duration
	Run   func(ctx context.Context) error
}

// Start запускает задания в фоне. Каждое выполняется в своей горутине строго последовательно
// (следующий запуск не начнётся, пока не закончился предыдущий), первый раз сразу при старте.
// Паника или ошибка в одном запуске логируются и не останавливают ни это задание, ни остальные.
// Возвращаемая функция ждёт завершения всех заданий (они завершаются при отмене ctx).
func Start(ctx context.Context, log *slog.Logger, jobs ...Job) (wait func()) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			run(ctx, log, j)
			t := time.NewTicker(j.Every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					run(ctx, log, j)
				}
			}
		}(j)
	}
	return wg.Wait
}

// run один безопасный запуск задания.
func run(ctx context.Context, log *slog.Logger, j Job) {
	if ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("паника в плановом задании", "job", j.Name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	if err := j.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("ошибка планового задания", "job", j.Name, "err", err)
	}
}
