package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Виды напоминаний (поле kind в журнале): что именно ждём от покупателя.
const (
	PhaseAccept = "accept" // принять задание или отказаться
	PhaseReport = "report" // прислать отчёт
	PhaseRework = "rework" // исправить отчёт после возврата
)

// NoticeKind что нужно отправить.
type NoticeKind string

const (
	NoticeReminder NoticeKind = "reminder" // напоминание покупателю
	NoticeExpired  NoticeKind = "expired"  // срок задания вышел (уведомляется покупатель)
)

// Notice одно уведомление, которое планировщик просит отправить. Сервис сам ничего не отправляет:
// отправкой в Telegram занимается слой handlers, поэтому логика проверяется без сети.
type Notice struct {
	Kind    NoticeKind
	Phase   string
	Card    *TaskCard
	Seq     int           // номер напоминания (для NoticeReminder)
	Total   int           // сколько напоминаний предусмотрено
	Elapsed time.Duration // сколько прошло с момента, от которого считаются интервалы
}

// RemindConfig настройки напоминаний (задаются в конфиге, а не в боте).
type RemindConfig struct {
	Accept    []time.Duration // от отправки задания до принятия
	Report    []time.Duration // от принятия (и от возврата на доработку) до отчёта
	QuietOn   bool
	QuietFrom int           // час начала тихих часов (0-23)
	QuietTo   int           // час окончания
	Stale     time.Duration // через сколько ожидания в списке заданий появляется отметка «давно без ответа»
}

// PingCooldown минимальный промежуток между ручными напоминаниями по одному заданию.
const PingCooldown = 10 * time.Minute

// Reminders автоматическая просрочка и напоминания покупателям.
type Reminders struct {
	store storage.Store
	tasks *Tasks
	cfg   RemindConfig
	loc   *time.Location
	now   func() time.Time
}

// NewReminders создаёт сервис. loc нужен для тихих часов.
func NewReminders(store storage.Store, tasks *Tasks, cfg RemindConfig, loc *time.Location) *Reminders {
	return &Reminders{store: store, tasks: tasks, cfg: cfg, loc: loc, now: time.Now}
}

// Stale порог «давно без ответа» для отметок в списке заданий.
func (r *Reminders) Stale() time.Duration { return r.cfg.Stale }

// phaseSpec какие задания и от какого момента отсчитывать.
type phaseSpec struct {
	phase   string
	status  domain.TaskStatus
	offsets []time.Duration
	base    func(c *TaskCard) time.Time
}

// Tick один проход планировщика: отмечает просроченные задания и вычисляет напоминания.
// Всё записывается в БД до отправки, поэтому после перезапуска ничего не повторяется
// (худший случай при сбое: одно напоминание не дойдёт, но не придёт дважды).
func (r *Reminders) Tick(ctx context.Context) ([]Notice, error) {
	now := r.now().UTC()
	var out []Notice

	expired, err := r.expire(ctx, now)
	if err != nil {
		return out, err
	}
	out = append(out, expired...)

	cfg := r.cfg
	quiet := cfg.QuietOn && InQuiet(now.In(r.loc), cfg.QuietFrom, cfg.QuietTo)

	specs := []phaseSpec{
		{PhaseAccept, domain.TaskSent, cfg.Accept, func(c *TaskCard) time.Time { return c.Task.SentAt }},
		{PhaseReport, domain.TaskAccepted, cfg.Report, func(c *TaskCard) time.Time { return c.Task.AcceptedAt }},
		{PhaseRework, domain.TaskRework, cfg.Report, func(c *TaskCard) time.Time {
			if c.Report == nil {
				return time.Time{}
			}
			return c.Report.DecidedAt
		}},
	}
	for _, sp := range specs {
		if len(sp.offsets) == 0 {
			continue
		}
		list, err := r.store.Repos().Tasks.List(ctx, storage.TaskFilter{Statuses: []domain.TaskStatus{sp.status}}, 1000, 0)
		if err != nil {
			return out, err
		}
		for _, t := range list {
			card, err := r.tasks.Card(ctx, t.ID)
			if err != nil {
				return out, err
			}
			n, err := r.step(ctx, card, sp, quiet, now)
			if err != nil {
				return out, err
			}
			out = append(out, n...)
		}
	}
	return out, nil
}

// expire переводит принятые задания с вышедшим сроком в «просрочено».
func (r *Reminders) expire(ctx context.Context, now time.Time) ([]Notice, error) {
	list, err := r.store.Repos().Tasks.List(ctx, storage.TaskFilter{DueBefore: now}, 1000, 0)
	if err != nil {
		return nil, err
	}
	var out []Notice
	for _, t := range list {
		changed, err := r.tasks.Expire(ctx, t.ID)
		if err != nil {
			return out, err
		}
		if !changed {
			continue
		}
		card, err := r.tasks.Card(ctx, t.ID)
		if err != nil {
			return out, err
		}
		out = append(out, Notice{Kind: NoticeExpired, Phase: PhaseReport, Card: card})
	}
	return out, nil
}

// step применяет NextStep к одному заданию и записывает результат.
func (r *Reminders) step(ctx context.Context, card *TaskCard, sp phaseSpec, quiet bool, now time.Time) ([]Notice, error) {
	base := sp.base(card)
	if base.IsZero() {
		return nil, nil
	}
	var out []Notice
	err := r.store.WithTx(ctx, func(repos storage.Repos) error {
		done, err := repos.Reminders.Seqs(ctx, card.Task.ID, sp.phase, base)
		if err != nil {
			return err
		}
		st := NextStep(base, now, sp.offsets, done)
		switch {
		case st.Remind > 0:
			if quiet {
				return nil // тихие часы: ничего не записываем, отправим после их окончания
			}
			for _, k := range st.Skipped {
				if _, err := repos.Reminders.Record(ctx, card.Task.ID, sp.phase, base, k, true, now); err != nil {
					return err
				}
			}
			ok, err := repos.Reminders.Record(ctx, card.Task.ID, sp.phase, base, st.Remind, false, now)
			if err != nil || !ok {
				return err
			}
			if err := repos.Tasks.AddEvent(ctx, &domain.TaskEvent{
				TaskID: card.Task.ID, Kind: "reminder", At: now,
				Details: fmt.Sprintf("напоминание %d из %d (%s)", st.Remind, len(sp.offsets), phaseTitle(sp.phase)),
			}); err != nil {
				return err
			}
			out = append(out, Notice{Kind: NoticeReminder, Phase: sp.phase, Card: card, Seq: st.Remind, Total: len(sp.offsets), Elapsed: now.Sub(base)})
		}
		return nil
	})
	return out, err
}

func phaseTitle(phase string) string {
	switch phase {
	case PhaseAccept:
		return "принятие задания"
	case PhaseRework:
		return "исправление отчёта"
	}
	return "отчёт"
}

// PhaseOf какое действие сейчас ждём от покупателя по заданию (пусто, если ничего).
func PhaseOf(status domain.TaskStatus) string {
	switch status {
	case domain.TaskCreated, domain.TaskSent:
		return PhaseAccept
	case domain.TaskAccepted, domain.TaskExpired:
		return PhaseReport
	case domain.TaskRework:
		return PhaseRework
	}
	return ""
}

// Ping ручное напоминание от админа по заданию. Не чаще одного раза в PingCooldown.
func (r *Reminders) Ping(ctx context.Context, admin, taskID int64) (*TaskCard, error) {
	card, err := r.tasks.Card(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if PhaseOf(card.Task.Status) == "" || card.Task.Status == domain.TaskCreated {
		return nil, fmt.Errorf("%w: по этому заданию напоминать не нужно (статус «%s»)", ErrForbidden, card.Task.Status.Title())
	}
	now := r.now().UTC()
	err = r.store.WithTx(ctx, func(repos storage.Repos) error {
		events, err := repos.Tasks.Events(ctx, taskID)
		if err != nil {
			return err
		}
		for _, e := range events {
			if e.Kind == "ping" && now.Sub(e.At) < PingCooldown {
				mins := int((PingCooldown - now.Sub(e.At)).Minutes()) + 1
				return fmt.Errorf("%w: напоминание уже отправлено, повторить можно через %d мин.", ErrForbidden, mins)
			}
		}
		if err := repos.Tasks.AddEvent(ctx, &domain.TaskEvent{
			TaskID: taskID, Kind: "ping", ActorID: admin, Details: "ручное напоминание от админа", At: now,
		}); err != nil {
			return err
		}
		return repos.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: admin, Action: "task.ping", Entity: "task", EntityID: fmt.Sprint(taskID), At: now,
		})
	})
	if err != nil {
		return nil, err
	}
	return card, nil
}

// PingUser напоминает покупателю обо всех его незавершённых заданиях. Возвращает отправленные
// (прошедшие ограничение частоты) карточки и число пропущенных из-за недавнего напоминания.
func (r *Reminders) PingUser(ctx context.Context, admin, userID int64) (sent []*TaskCard, throttled int, err error) {
	list, err := r.store.Repos().Tasks.List(ctx, storage.TaskFilter{
		UserID:   userID,
		Statuses: []domain.TaskStatus{domain.TaskSent, domain.TaskAccepted, domain.TaskExpired, domain.TaskRework},
	}, 50, 0)
	if err != nil {
		return nil, 0, err
	}
	for _, t := range list {
		card, err := r.Ping(ctx, admin, t.ID)
		switch {
		case err == nil:
			sent = append(sent, card)
		case errors.Is(err, ErrForbidden):
			throttled++
		default:
			return sent, throttled, err
		}
	}
	return sent, throttled, nil
}
