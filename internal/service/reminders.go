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

// tickPage размер страницы при обходе заданий: ограничений на общее число нет, обход идёт страницами.
var tickPage = 500

// phaseSpec какие задания и от какого момента отсчитывать.
type phaseSpec struct {
	phase   string
	status  domain.TaskStatus
	offsets []time.Duration
	// base момент, от которого идут интервалы (нулевой, если отсчёта нет). Берётся из самого
	// задания, без сборки полной карточки: так проход каждую минуту остаётся дешёвым.
	base func(ctx context.Context, t *domain.Task) (time.Time, error)
}

// eachTask обходит все задания по фильтру страницами. fn не должна менять статус заданий фильтра.
func (r *Reminders) eachTask(ctx context.Context, f storage.TaskFilter, fn func(*domain.Task) error) error {
	for off := 0; ; off += tickPage {
		list, err := r.store.Repos().Tasks.List(ctx, f, tickPage, off)
		if err != nil {
			return err
		}
		for _, t := range list {
			if err := fn(t); err != nil {
				return err
			}
		}
		if len(list) < tickPage {
			return nil
		}
	}
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
	if cfg.QuietOn && InQuiet(now.In(r.loc), cfg.QuietFrom, cfg.QuietTo) {
		return out, nil // тихие часы: напоминания отправим после их окончания
	}

	taskBase := func(get func(*domain.Task) time.Time) func(context.Context, *domain.Task) (time.Time, error) {
		return func(_ context.Context, t *domain.Task) (time.Time, error) { return get(t), nil }
	}
	specs := []phaseSpec{
		{PhaseAccept, domain.TaskSent, cfg.Accept, taskBase(func(t *domain.Task) time.Time { return t.SentAt })},
		{PhaseReport, domain.TaskAccepted, cfg.Report, taskBase(func(t *domain.Task) time.Time { return t.AcceptedAt })},
		{PhaseRework, domain.TaskRework, cfg.Report, func(ctx context.Context, t *domain.Task) (time.Time, error) {
			rep, err := r.store.Repos().Reports.ByTask(ctx, t.ID)
			if errors.Is(err, storage.ErrNotFound) {
				return time.Time{}, nil
			}
			if err != nil {
				return time.Time{}, err
			}
			return rep.DecidedAt, nil
		}},
	}
	for _, sp := range specs {
		if len(sp.offsets) == 0 {
			continue
		}
		sp := sp
		err := r.eachTask(ctx, storage.TaskFilter{Statuses: []domain.TaskStatus{sp.status}}, func(t *domain.Task) error {
			base, err := sp.base(ctx, t)
			if err != nil {
				return err
			}
			// Первое напоминание ещё не наступило: ничего читать и писать не нужно.
			if base.IsZero() || now.Before(base.Add(sp.offsets[0])) {
				return nil
			}
			n, err := r.step(ctx, t, sp, base, now)
			out = append(out, n...)
			return err
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// expire переводит принятые задания с вышедшим сроком в «просрочено».
func (r *Reminders) expire(ctx context.Context, now time.Time) ([]Notice, error) {
	var out []Notice
	for {
		// Просроченные задания выходят из выборки, поэтому каждый раз читаем с начала.
		list, err := r.store.Repos().Tasks.List(ctx, storage.TaskFilter{DueBefore: now}, tickPage, 0)
		if err != nil {
			return out, err
		}
		changedAny := false
		for _, t := range list {
			changed, err := r.tasks.Expire(ctx, t.ID)
			if err != nil {
				return out, err
			}
			if !changed {
				continue
			}
			changedAny = true
			card, err := r.tasks.Card(ctx, t.ID)
			if err != nil {
				return out, err
			}
			out = append(out, Notice{Kind: NoticeExpired, Phase: PhaseReport, Card: card})
		}
		if len(list) < tickPage || !changedAny {
			return out, nil
		}
	}
}

// step применяет NextStep к одному заданию и записывает результат. Карточка для уведомления
// собирается только тогда, когда напоминание действительно нужно отправить.
func (r *Reminders) step(ctx context.Context, t *domain.Task, sp phaseSpec, base, now time.Time) ([]Notice, error) {
	var seq int
	err := r.store.WithTx(ctx, func(repos storage.Repos) error {
		done, err := repos.Reminders.Seqs(ctx, t.ID, sp.phase, base)
		if err != nil {
			return err
		}
		st := NextStep(base, now, sp.offsets, done)
		if st.Remind == 0 {
			return nil
		}
		for _, k := range st.Skipped {
			if _, err := repos.Reminders.Record(ctx, t.ID, sp.phase, base, k, true, now); err != nil {
				return err
			}
		}
		ok, err := repos.Reminders.Record(ctx, t.ID, sp.phase, base, st.Remind, false, now)
		if err != nil || !ok {
			return err
		}
		if err := repos.Tasks.AddEvent(ctx, &domain.TaskEvent{
			TaskID: t.ID, Kind: "reminder", At: now,
			Details: fmt.Sprintf("напоминание %d из %d (%s)", st.Remind, len(sp.offsets), phaseTitle(sp.phase)),
		}); err != nil {
			return err
		}
		seq = st.Remind
		return nil
	})
	if err != nil || seq == 0 {
		return nil, err
	}
	card, err := r.tasks.Card(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	return []Notice{{Kind: NoticeReminder, Phase: sp.phase, Card: card, Seq: seq, Total: len(sp.offsets), Elapsed: now.Sub(base)}}, nil
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
				return fmt.Errorf("%w: напоминание уже отправлено, повторить можно через %d мин", ErrForbidden, mins)
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
