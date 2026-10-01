package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("условие не выполнилось за 3 секунды")
}

func TestJobRunsImmediatelyAndRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	wait := Start(ctx, quiet, Job{Name: "t", Every: 10 * time.Millisecond, Run: func(context.Context) error { n.Add(1); return nil }})
	waitFor(t, func() bool { return n.Load() >= 3 })
	cancel()
	wait()
	after := n.Load()
	time.Sleep(50 * time.Millisecond)
	if n.Load() != after {
		t.Fatal("после отмены задание не должно запускаться")
	}
}

// Паника и ошибка в задании не останавливают расписание и другие задания.
func TestJobSurvivesPanicAndError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var bad, good atomic.Int32
	wait := Start(ctx, quiet,
		Job{Name: "panic", Every: 5 * time.Millisecond, Run: func(context.Context) error {
			if bad.Add(1)%2 == 1 {
				panic("сбой")
			}
			return errors.New("ошибка")
		}},
		Job{Name: "good", Every: 5 * time.Millisecond, Run: func(context.Context) error { good.Add(1); return nil }},
	)
	waitFor(t, func() bool { return bad.Load() >= 4 && good.Load() >= 4 })
	cancel()
	wait()
}

// Запуски одного задания не накладываются друг на друга.
func TestJobsDoNotOverlap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var running, maxRunning, runs atomic.Int32
	wait := Start(ctx, quiet, Job{Name: "slow", Every: time.Millisecond, Run: func(context.Context) error {
		cur := running.Add(1)
		for {
			m := maxRunning.Load()
			if cur <= m || maxRunning.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		running.Add(-1)
		runs.Add(1)
		return nil
	}})
	waitFor(t, func() bool { return runs.Load() >= 3 })
	cancel()
	wait()
	if maxRunning.Load() != 1 {
		t.Fatalf("одновременно выполнялось %d запусков", maxRunning.Load())
	}
}
