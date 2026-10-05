package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"botech/internal/domain"
)

// Часовой пояс Ташкента без зависимости от базы tz.
var uzt = time.FixedZone("UZT", 5*3600)

// clock управляемые часы: тесты «перематывают» время, не дожидаясь суток.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *clock) Add(d time.Duration) { c.Set(c.Now().Add(d)) }

// t0 утро рабочего дня по Ташкенту: напоминания через 24/48/72 часа тоже попадают на день.
var t0 = time.Date(2026, 10, 5, 10, 0, 0, 0, uzt)

// useClock подключает часы ко всем сервисам окружения.
func (e *env) useClock(c *clock) {
	e.tasks.now = c.Now
}

// newReminders создаёт «процесс» планировщика; повторный вызов имитирует перезапуск бота.
func (e *env) newReminders(c *clock) *Reminders {
	return e.newRemindersCfg(c, defaultRemindCfg())
}

// defaultRemindCfg: напоминания через 24 и 48 часов, тихие часы 22-9.
func defaultRemindCfg() RemindConfig {
	d := MinutesToDurations([]int{1440, 2880})
	return RemindConfig{Accept: d, Report: d, QuietOn: true, QuietFrom: 22, QuietTo: 9, Stale: 24 * time.Hour}
}

func (e *env) newRemindersCfg(c *clock, cfg RemindConfig) *Reminders {
	r := NewReminders(e.store, e.tasks, cfg, uzt)
	r.now = c.Now
	return r
}

func (e *env) reportsAt(c *clock) *Reports {
	r := NewReports(e.store, e.tasks)
	r.now = c.Now
	return r
}

// tick выполняет проход и возвращает уведомления.
func tick(t *testing.T, r *Reminders) []Notice {
	t.Helper()
	n, err := r.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func kinds(ns []Notice) []NoticeKind {
	out := make([]NoticeKind, len(ns))
	for i, n := range ns {
		out[i] = n.Kind
	}
	return out
}

func TestTickAcceptReminders(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	rem := e.newReminders(c)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1) // отправлено в t0

	step := func(after time.Duration, wantKinds ...NoticeKind) []Notice {
		t.Helper()
		c.Set(t0.Add(after))
		got := tick(t, rem)
		if len(got) != len(wantKinds) {
			t.Fatalf("через %v: уведомления %v, ожидали %v", after, kinds(got), wantKinds)
		}
		for i, k := range wantKinds {
			if got[i].Kind != k {
				t.Fatalf("через %v: уведомления %v, ожидали %v", after, kinds(got), wantKinds)
			}
		}
		return got
	}

	step(23*time.Hour + 59*time.Minute)
	n := step(24*time.Hour, NoticeReminder)
	if n[0].Phase != PhaseAccept || n[0].Seq != 1 || n[0].Total != 2 || n[0].Card.Task.ID != id {
		t.Fatalf("первое напоминание: %+v", n[0])
	}
	step(24 * time.Hour) // тот же момент: повторов нет
	step(47 * time.Hour)
	n = step(48*time.Hour, NoticeReminder)
	if n[0].Seq != 2 {
		t.Fatalf("второе напоминание: %+v", n[0])
	}
	step(72 * time.Hour)  // эскалации админу нет
	step(200 * time.Hour) // больше ничего

	// Всё зафиксировано в истории задания.
	events, _ := e.tasks.Events(ctx, id)
	count := map[string]int{}
	for _, ev := range events {
		count[ev.Kind]++
	}
	if count["reminder"] != 2 || count["escalation"] != 0 {
		t.Fatalf("история: %v", count)
	}
}

// Бот не работал сутки и больше: покупатель получает одно напоминание, а не пачку.
func TestTickCatchUpSendsOnlyLatest(t *testing.T) {
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	rem := e.newReminders(c)
	sc := e.importScenario(t, scenarioYAML).Scenario
	e.assignOne(t, sc.ID, 1)

	c.Set(t0.Add(58 * time.Hour)) // 20:00 по Ташкенту, вне тихих часов
	got := tick(t, rem)
	if len(got) != 1 || got[0].Seq != 2 || got[0].Kind != NoticeReminder {
		t.Fatalf("ожидали одно напоминание №2: %+v", got)
	}
	if got := tick(t, rem); len(got) != 0 {
		t.Fatalf("пропущенное первое не должно отправляться потом: %v", kinds(got))
	}
}

// Перезапуск бота не повторяет уже отправленное: состояние хранится в БД.
func TestTickSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	sc := e.importScenario(t, scenarioYAML).Scenario
	e.assignOne(t, sc.ID, 1)

	c.Set(t0.Add(24 * time.Hour))
	if got := tick(t, e.newReminders(c)); len(got) != 1 {
		t.Fatalf("первый процесс: %v", kinds(got))
	}
	// «Перезапуск»: новый экземпляр с теми же данными в БД.
	if got := tick(t, e.newReminders(c)); len(got) != 0 {
		t.Fatalf("после перезапуска повтор: %v", kinds(got))
	}
}

func TestTickQuietHours(t *testing.T) {
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	cfg := defaultRemindCfg()
	cfg.Accept = MinutesToDurations([]int{13 * 60}) // через 13 ч = 23:00 по Ташкенту
	rem := e.newRemindersCfg(c, cfg)
	sc := e.importScenario(t, scenarioYAML).Scenario
	e.assignOne(t, sc.ID, 1)

	c.Set(t0.Add(13 * time.Hour)) // 23:00, тихие часы 22-9
	if got := tick(t, rem); len(got) != 0 {
		t.Fatalf("ночью напоминать нельзя: %v", kinds(got))
	}
	c.Set(t0.Add(22 * time.Hour)) // 08:00 следующего дня, всё ещё тихо
	if got := tick(t, rem); len(got) != 0 {
		t.Fatalf("до 9:00 напоминать нельзя: %v", kinds(got))
	}
	c.Set(t0.Add(23 * time.Hour)) // 09:00
	got := tick(t, rem)
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("утром отложенное напоминание уходит: %v", kinds(got))
	}
	// Тихие часы можно выключить.
	cfg.QuietOn = false
	rem = e.newRemindersCfg(c, cfg)
	e2 := e.assignOne(t, sc.ID, 2)
	_ = e2
	c.Set(t0.Add(23*time.Hour + 13*time.Hour)) // 22:00 того же дня + 13 ч от новой отправки
	if got := tick(t, rem); len(got) == 0 {
		t.Fatal("без тихих часов напоминания идут в любое время")
	}
}

// Принятое задание: напоминания об отчёте, затем автоматическая просрочка и уведомление.
func TestTickReportAndExpiry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	rem := e.newReminders(c)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1) // срок 3 дня после принятия
	if _, _, err := e.tasks.Accept(ctx, 1, id); err != nil {
		t.Fatal(err)
	}

	c.Set(t0.Add(24 * time.Hour))
	if got := tick(t, rem); len(got) != 1 || got[0].Phase != PhaseReport || got[0].Seq != 1 {
		t.Fatalf("напоминание об отчёте: %+v", got)
	}
	c.Set(t0.Add(48 * time.Hour))
	if got := tick(t, rem); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("второе напоминание: %+v", got)
	}
	// В момент срока задание становится просроченным; вместо эскалации приходит уведомление о просрочке.
	c.Set(t0.Add(72 * time.Hour))
	got := tick(t, rem)
	if len(got) != 1 || got[0].Kind != NoticeExpired || got[0].Card.Task.Status != domain.TaskExpired {
		t.Fatalf("просрочка: %+v", got)
	}
	if got := tick(t, rem); len(got) != 0 {
		t.Fatalf("после просрочки тишина: %v", kinds(got))
	}
	c.Set(t0.Add(300 * time.Hour))
	if got := tick(t, rem); len(got) != 0 {
		t.Fatalf("просроченным больше не напоминаем: %v", kinds(got))
	}
	// Опоздавший отчёт всё равно принимается.
	if _, err := e.reportsAt(c).Submit(ctx, 1, id, SubmitInput{Answers: []domain.Answer{{Key: "q_one", Value: "ок"}}}); err != nil {
		t.Fatal(err)
	}
}

// После возврата на доработку отсчёт идёт от момента возврата; повторный возврат начинает заново.
func TestTickRework(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	rep := e.reportsAt(c)
	rem := e.newReminders(c)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)
	if _, _, err := e.tasks.Accept(ctx, 1, id); err != nil {
		t.Fatal(err)
	}
	answers := []domain.Answer{{Key: "q_one", Value: "ок"}}
	if _, err := rep.Submit(ctx, 1, id, SubmitInput{Answers: answers}); err != nil {
		t.Fatal(err)
	}

	c.Set(t0.Add(5 * time.Hour)) // 15:00
	if _, changed, err := rep.ReturnForRework(ctx, firstAdmin, id, "исправьте"); err != nil || !changed {
		t.Fatal(err)
	}
	base1 := c.Now()
	c.Set(base1.Add(24 * time.Hour))
	got := tick(t, rem)
	if len(got) != 1 || got[0].Phase != PhaseRework || got[0].Seq != 1 {
		t.Fatalf("напоминание об исправлении: %+v", got)
	}

	// Покупатель исправил, админ вернул снова: напоминания начинаются заново.
	if _, err := rep.Submit(ctx, 1, id, SubmitInput{Answers: answers}); err != nil {
		t.Fatal(err)
	}
	c.Add(time.Hour)
	if _, changed, _ := rep.ReturnForRework(ctx, firstAdmin, id, "ещё раз"); !changed {
		t.Fatal("второй возврат")
	}
	base2 := c.Now()
	c.Set(base2.Add(23 * time.Hour))
	if got := tick(t, rem); len(got) != 0 {
		t.Fatalf("рано: %v", kinds(got))
	}
	c.Set(base2.Add(24 * time.Hour))
	if got := tick(t, rem); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("после повторного возврата отсчёт заново: %+v", got)
	}
}

// Интервалы задаются конфигом в минутах; пустой список отключает напоминания, но не просрочку.
func TestTickConfigMinutes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	cfg := defaultRemindCfg()
	cfg.QuietOn = false
	cfg.Accept = MinutesToDurations([]int{5, 10})
	rem := e.newRemindersCfg(c, cfg)
	sc := e.importScenario(t, scenarioYAML).Scenario
	e.assignOne(t, sc.ID, 1)

	for _, tc := range []struct {
		after time.Duration
		want  int
	}{{4 * time.Minute, 0}, {5 * time.Minute, 1}, {9 * time.Minute, 0}, {10 * time.Minute, 1}, {time.Hour, 0}} {
		c.Set(t0.Add(tc.after))
		if got := tick(t, rem); len(got) != tc.want {
			t.Fatalf("через %v: %v, ожидали %d напоминаний", tc.after, kinds(got), tc.want)
		}
	}

	// Напоминания выключены (пустые списки); просрочка при этом продолжает работать.
	cfg.Accept, cfg.Report = nil, nil
	rem = e.newRemindersCfg(c, cfg)
	id2 := e.assignOne(t, sc.ID, 2)
	if _, _, err := e.tasks.Accept(ctx, 2, id2); err != nil {
		t.Fatal(err)
	}
	c.Set(t0.Add(100 * time.Hour))
	got := tick(t, rem)
	if len(got) != 1 || got[0].Kind != NoticeExpired {
		t.Fatalf("при выключенных напоминаниях просрочка должна работать: %v", kinds(got))
	}
	if rem.Stale() != 24*time.Hour {
		t.Fatalf("порог «давно без ответа»: %v", rem.Stale())
	}
}

func TestSettingsValidation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	set := NewSettings(e.store)
	// Настройки напоминаний из бота убраны: они задаются конфигом.
	for _, b := range []struct{ key, val string }{
		{KeyBackupTime, "99:99"}, {"remind_accept_hours", "24"}, {"escalate_hours", "24"}, {"unknown", "1"},
	} {
		if _, err := set.Set(ctx, firstAdmin, b.key, b.val); !errors.Is(err, ErrForbidden) {
			t.Errorf("Set(%s, %q): ожидали ErrForbidden, получили %v", b.key, b.val, err)
		}
	}
	if v, err := set.Set(ctx, firstAdmin, KeyBackupTime, " 3:05 "); err != nil || v != "03:05" {
		t.Fatalf("нормализация времени: %q %v", v, err)
	}
	if b, _ := set.Backup(ctx); !b.Enabled || b.Hour != 3 || b.Minute != 5 {
		t.Fatalf("расписание бэкапа: %+v", b)
	}
}

func TestPing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	rem := e.newReminders(c)
	sc := e.importScenario(t, scenarioYAML).Scenario
	sent := e.assignOne(t, sc.ID, 1)
	declined := e.assignOne(t, sc.ID, 2)
	if _, _, err := e.tasks.Decline(ctx, 2, declined); err != nil {
		t.Fatal(err)
	}

	if card, err := rem.Ping(ctx, firstAdmin, sent); err != nil || card.Task.ID != sent {
		t.Fatalf("пинг: %v", err)
	}
	if _, err := rem.Ping(ctx, firstAdmin, sent); !errors.Is(err, ErrForbidden) {
		t.Fatalf("повторный пинг сразу: %v", err)
	}
	c.Add(PingCooldown + time.Second)
	if _, err := rem.Ping(ctx, firstAdmin, sent); err != nil {
		t.Fatalf("после паузы пинг разрешён: %v", err)
	}
	if _, err := rem.Ping(ctx, firstAdmin, declined); !errors.Is(err, ErrForbidden) {
		t.Fatalf("по отказавшемуся напоминать не нужно: %v", err)
	}
	if _, err := rem.Ping(ctx, firstAdmin, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("несуществующее: %v", err)
	}

	// Пинг покупателя целиком: два задания, по одному недавно уже напоминали.
	second := e.assignOne(t, sc.ID, 1)
	cards, throttled, err := rem.PingUser(ctx, firstAdmin, 1)
	if err != nil || len(cards) != 1 || cards[0].Task.ID != second || throttled != 1 {
		t.Fatalf("PingUser: cards=%d throttled=%d err=%v", len(cards), throttled, err)
	}
}

// Удаление не начатого задания не мешает журналу напоминаний (внешний ключ).
func TestDeleteTaskWithReminders(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	c := &clock{t: t0}
	e.useClock(c)
	rem := e.newReminders(c)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)
	c.Set(t0.Add(24 * time.Hour))
	if got := tick(t, rem); len(got) != 1 {
		t.Fatal("нужно напоминание для проверки")
	}
	if err := e.tasks.Delete(ctx, firstAdmin, id); err != nil {
		t.Fatalf("удаление задания с напоминаниями: %v", err)
	}
	if got := tick(t, rem); len(got) != 0 {
		t.Fatalf("после удаления напоминать некому: %v", kinds(got))
	}
}
