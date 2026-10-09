package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Ограничения срока выполнения (в днях после принятия).
const (
	MinDueDays = 1
	MaxDueDays = 60
)

// Tasks управляет заданиями и их статусами.
type Tasks struct {
	store        storage.Store
	now          func() time.Time
	promoMaxUses int // сколько раз можно использовать один промокод
}

// NewTasks создаёт сервис заданий. promoMaxUses: лимит использований одного промокода.
func NewTasks(store storage.Store, promoMaxUses int) *Tasks {
	return &Tasks{store: store, now: time.Now, promoMaxUses: promoMaxUses}
}

// PromoMaxUses лимит использований одного промокода.
func (s *Tasks) PromoMaxUses() int { return s.promoMaxUses }

// PromoReason почему заданию не удалось выдать промокод.
type PromoReason string

const (
	PromoReasonNone      PromoReason = ""          // код выдан (или не требовался)
	PromoReasonBusy      PromoReason = "busy"      // все коды с остатком заняты активными заданиями
	PromoReasonExhausted PromoReason = "exhausted" // нет ни одного кода со свободными использованиями
)

// TaskCard задание вместе со связанными данными для показа.
type TaskCard struct {
	Task     *domain.Task
	Scenario *domain.Scenario
	Version  *domain.ScenarioVersion // именно та версия, по которой выдано задание
	User     *domain.User
	Promo    *domain.PromoAssignment // выдача промокода (nil, если не выдавали)
	Comp     *domain.Compensation    // данные компенсации (nil, если нет)
	Report   *domain.Report          // последняя версия отчёта (nil, если отчёта нет)
	Item     *domain.Item            // выбранный покупателем айтем (nil, если не выбран или задание продавца)

	// PromoReason заполняется результатом Accept и IssuePromo, когда кода нет.
	PromoReason PromoReason
}

// AssignResult итог назначения нескольким покупателям.
type AssignResult struct {
	Created []*domain.Task
	Repeat  []int64 // у этих покупателей уже было незавершённое задание по этому сценарию (выдано ещё одно)
	Invalid []int64 // нет такого активного пользователя
	// WrongKind: пользователь не выбрал тип заданий этого сценария (покупатель/продавец), задание ему не выдано.
	WrongKind []int64
}

// errWrongKind пользователь не выбрал тип заданий, к которому относится сценарий (внутренняя).
var errWrongKind = errors.New("тип задания не выбран пользователем")

// activeStatuses незавершённые задания.
var activeStatuses = []domain.TaskStatus{domain.TaskCreated, domain.TaskSent, domain.TaskAccepted, domain.TaskExpired, domain.TaskRework}

// Assign создаёт задания (статус «создано»). Отправку в Telegram выполняет вызывающий,
// затем вызывает MarkSent. Так сбой доставки не теряет задание.
func (s *Tasks) Assign(ctx context.Context, actor, scenarioID int64, dueDays int, userIDs []int64) (*AssignResult, error) {
	if dueDays < MinDueDays || dueDays > MaxDueDays {
		return nil, fmt.Errorf("%w: срок должен быть от %d до %d дней", ErrForbidden, MinDueDays, MaxDueDays)
	}
	repos := s.store.Repos()
	sc, err := repos.Scenarios.GetByID(ctx, scenarioID)
	if err != nil {
		return nil, err
	}
	if sc.Archived {
		return nil, fmt.Errorf("%w: сценарий в архиве", ErrForbidden)
	}
	ver, err := repos.Scenarios.LatestVersion(ctx, scenarioID)
	if err != nil {
		return nil, err
	}

	res := &AssignResult{}
	seen := map[int64]bool{}
	for _, uid := range userIDs {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		// Каждому покупателю своя транзакция: сбой у одного не отменяет остальных.
		var (
			task   *domain.Task
			repeat bool
		)
		err := s.store.WithTx(ctx, func(r storage.Repos) error {
			u, err := r.Users.Get(ctx, uid)
			if errors.Is(err, storage.ErrNotFound) || (err == nil && (u.Role != domain.RoleBuyer || u.Status != domain.StatusActive)) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if !u.CanDo(sc.Kind) {
				return errWrongKind
			}
			// Повторное назначение того же сценария разрешено, но админу сообщаем о нём.
			n, err := r.Tasks.Count(ctx, storage.TaskFilter{UserID: uid, ScenarioID: scenarioID, Statuses: activeStatuses})
			if err != nil {
				return err
			}
			repeat = n > 0
			now := s.now().UTC()
			task = &domain.Task{
				ScenarioID: scenarioID, ScenarioVersionID: ver.ID, UserID: uid,
				Status: domain.TaskCreated, DueDays: dueDays, CreatedBy: actor, CreatedAt: now,
			}
			if err := r.Tasks.Create(ctx, task); err != nil {
				return err
			}
			if err := r.Tasks.AddEvent(ctx, &domain.TaskEvent{
				TaskID: task.ID, Kind: "created", ToStatus: domain.TaskCreated, ActorID: actor,
				Details: fmt.Sprintf("сценарий %s v%d, срок %d дн.", sc.Key, ver.Version, dueDays), At: now,
			}); err != nil {
				return err
			}
			return r.Audit.Add(ctx, &domain.AuditEntry{
				AdminID: actor, Action: "task.assign", Entity: "task", EntityID: fmt.Sprint(task.ID),
				Details: fmt.Sprintf("user %d, %s v%d", uid, sc.Key, ver.Version), At: now,
			})
		})
		switch {
		case err == nil:
			res.Created = append(res.Created, task)
			if repeat {
				res.Repeat = append(res.Repeat, uid)
			}
		case errors.Is(err, errWrongKind):
			res.WrongKind = append(res.WrongKind, uid)
		case errors.Is(err, ErrNotFound):
			res.Invalid = append(res.Invalid, uid)
		default:
			return res, err
		}
	}
	return res, nil
}

// MarkSent фиксирует успешную доставку задания покупателю (создано -> отправлено).
func (s *Tasks) MarkSent(ctx context.Context, taskID int64) error {
	_, err := s.transition(ctx, taskID, 0, domain.TaskSent, "", nil)
	return err
}

// MarkSendFailed записывает в историю неудачную доставку (статус остаётся «создано»).
func (s *Tasks) MarkSendFailed(ctx context.Context, taskID int64, reason string) error {
	return s.store.Repos().Tasks.AddEvent(ctx, &domain.TaskEvent{
		TaskID: taskID, Kind: "send_failed", ToStatus: domain.TaskCreated, Details: reason, At: s.now().UTC(),
	})
}

// Accept покупатель принимает задание. changed=false, если задание уже не в статусе
// «отправлено» (например, повторное нажатие): вызывающий просто показывает актуальное состояние.
func (s *Tasks) Accept(ctx context.Context, userID, taskID int64) (*TaskCard, bool, error) {
	return s.byBuyer(ctx, userID, taskID, domain.TaskAccepted)
}

// Decline покупатель отказывается от задания.
func (s *Tasks) Decline(ctx context.Context, userID, taskID int64) (*TaskCard, bool, error) {
	return s.byBuyer(ctx, userID, taskID, domain.TaskDeclined)
}

func (s *Tasks) byBuyer(ctx context.Context, userID, taskID int64, to domain.TaskStatus) (*TaskCard, bool, error) {
	// Покупатель нажал кнопку, значит сообщение он получил. Если бот ещё не успел
	// отметить отправку (доли секунды между отправкой и записью), делаем это здесь.
	if _, err := s.transition(ctx, taskID, userID, domain.TaskSent, "", nil); err != nil {
		return nil, false, err
	}
	var (
		reason PromoReason
		hook   func(storage.Repos, *domain.Task, time.Time) error
	)
	if to == domain.TaskAccepted {
		// Промокод выдаётся в той же транзакции, что и принятие: либо оба действия, либо ни одного.
		// Продавцу промокод на доставку не нужен.
		hook = func(r storage.Repos, t *domain.Task, now time.Time) error {
			if seller, err := isSellerTask(ctx, r, t); err != nil || seller {
				return err
			}
			var err error
			_, reason, err = issuePromo(ctx, r, t, s.promoMaxUses, now)
			return err
		}
	}
	changed, err := s.transition(ctx, taskID, userID, to, "", hook)
	if err != nil {
		return nil, false, err
	}
	card, err := s.Card(ctx, taskID)
	if card != nil && changed {
		card.PromoReason = reason
	}
	return card, changed, err
}

// isSellerTask true, если задание выдано по сценарию продавца (у него нет промокода, айтема и компенсации).
func isSellerTask(ctx context.Context, r storage.Repos, t *domain.Task) (bool, error) {
	v, err := r.Scenarios.GetVersion(ctx, t.ScenarioVersionID)
	if err != nil {
		return false, err
	}
	return v.Body.Kind == domain.KindSeller, nil
}

// issuePromo выдаёт заданию код из пула (если он ещё не выдан) и пишет событие в историю.
// Отсутствие подходящего кода не ошибка: возвращается причина, а админа предупредит вызывающий.
func issuePromo(ctx context.Context, r storage.Repos, t *domain.Task, maxUses int, now time.Time) (*domain.PromoAssignment, PromoReason, error) {
	if a, err := r.Promos.ByTask(ctx, t.ID); err == nil {
		return a, PromoReasonNone, nil // уже выдан ранее
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, PromoReasonNone, err
	}
	a, err := r.Promos.Issue(ctx, t.ID, t.UserID, maxUses, now)
	if errors.Is(err, storage.ErrNotFound) {
		st, err := r.Promos.Stats(ctx, maxUses)
		if err != nil {
			return nil, PromoReasonNone, err
		}
		reason, details := PromoReasonBusy, "все промокоды заняты активными заданиями"
		if st.WithUsesLeft == 0 {
			reason, details = PromoReasonExhausted, "нет промокодов со свободными использованиями"
		}
		return nil, reason, r.Tasks.AddEvent(ctx, &domain.TaskEvent{TaskID: t.ID, Kind: "promo_missing", Details: details, At: now})
	}
	if err != nil {
		return nil, PromoReasonNone, err
	}
	return a, PromoReasonNone, r.Tasks.AddEvent(ctx, &domain.TaskEvent{TaskID: t.ID, Kind: "promo_issued", Details: "выдан промокод", At: now})
}

// IssuePromo выдаёт промокод принятому заданию, если при принятии свободного кода не было.
// Повторный вызов ничего не выдаёт: код один на задание.
func (s *Tasks) IssuePromo(ctx context.Context, userID, taskID int64) (*TaskCard, error) {
	var reason PromoReason
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		t, err := r.Tasks.Get(ctx, taskID)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && t.UserID != userID) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.Status != domain.TaskAccepted && t.Status != domain.TaskExpired {
			return fmt.Errorf("%w: промокод выдаётся принятым заданиям", ErrForbidden)
		}
		if seller, err := isSellerTask(ctx, r, t); err != nil {
			return err
		} else if seller {
			return fmt.Errorf("%w: промокоды выдаются только заданиям покупателя", ErrForbidden)
		}
		_, reason, err = issuePromo(ctx, r, t, s.promoMaxUses, s.now().UTC())
		return err
	})
	if err != nil {
		return nil, err
	}
	card, err := s.Card(ctx, taskID)
	if card != nil {
		card.PromoReason = reason
	}
	return card, err
}

// Review админ помечает задание «проверено».
func (s *Tasks) Review(ctx context.Context, admin, taskID int64) (bool, error) {
	accept := func(r storage.Repos, t *domain.Task, now time.Time) error {
		return r.Reports.SetDecision(ctx, t.ID, domain.ReportAccepted, "", admin, now)
	}
	changed, err := s.transition(ctx, taskID, 0, domain.TaskReviewed, "", accept)
	if err != nil || !changed {
		return changed, err
	}
	return true, s.store.Repos().Audit.Add(ctx, &domain.AuditEntry{
		AdminID: admin, Action: "task.review", Entity: "task", EntityID: fmt.Sprint(taskID), At: s.now().UTC(),
	})
}

// transition общий путь смены статуса: проверка владельца (если owner != 0), правил переходов,
// атомарное обновление и запись в историю. Возвращает changed=false, если статус уже был другим.
func (s *Tasks) transition(ctx context.Context, taskID, owner int64, to domain.TaskStatus, details string,
	after func(r storage.Repos, t *domain.Task, now time.Time) error) (bool, error) {
	changed := false
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		t, err := r.Tasks.Get(ctx, taskID)
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if owner != 0 && t.UserID != owner {
			return ErrNotFound // чужое задание: не раскрываем, что оно существует
		}
		if !domain.CanTransition(t.Status, to) {
			return nil // уже обработано или переход невозможен: ничего не меняем
		}
		now := s.now().UTC()
		var dueAt time.Time
		if to == domain.TaskAccepted {
			dueAt = now.Add(time.Duration(t.DueDays) * 24 * time.Hour)
		}
		ok, err := r.Tasks.Transition(ctx, taskID, t.Status, to, now, dueAt)
		if err != nil || !ok {
			return err // !ok: параллельный запрос успел раньше
		}
		changed = true
		if err := r.Tasks.AddEvent(ctx, &domain.TaskEvent{
			TaskID: taskID, Kind: "status", FromStatus: t.Status, ToStatus: to, ActorID: owner, Details: details, At: now,
		}); err != nil {
			return err
		}
		if after != nil {
			return after(r, t, now)
		}
		return nil
	})
	return changed, err
}

// Delete полностью удаляет не начатое задание (создано, отправлено, отказ) с его историей.
// Задания в работе отменяются (Cancel), задания с отчётом не удаляются: там данные для учёта и выплат.
func (s *Tasks) Delete(ctx context.Context, admin, taskID int64) error {
	return s.store.WithTx(ctx, func(r storage.Repos) error {
		t, err := r.Tasks.Get(ctx, taskID)
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch t.Status {
		case domain.TaskCreated, domain.TaskSent, domain.TaskDeclined:
		case domain.TaskAccepted, domain.TaskExpired:
			return fmt.Errorf("%w: задание в работе нельзя удалить, его можно отменить", ErrForbidden)
		default:
			return fmt.Errorf("%w: задание с отчётом не удаляется, данные нужны для учёта", ErrForbidden)
		}
		ok, err := r.Tasks.DeleteUnstarted(ctx, taskID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: admin, Action: "task.delete", Entity: "task", EntityID: fmt.Sprint(taskID),
			Details: fmt.Sprintf("user %d, статус %s", t.UserID, t.Status), At: s.now().UTC(),
		})
	})
}

// Cancel отменяет задание в работе (принято или просрочено). Промокод возвращается в оборот.
func (s *Tasks) Cancel(ctx context.Context, admin, taskID int64) (bool, error) {
	// Код возвращается в оборот без списания использования: задание не выполнено.
	release := func(r storage.Repos, t *domain.Task, now time.Time) error {
		if _, err := r.Promos.Release(ctx, t.ID, false, now); err != nil {
			return err
		}
		_, err := r.Items.Release(ctx, t.ID, false, now) // выбранный айтем возвращается в список свободных
		return err
	}
	changed, err := s.transition(ctx, taskID, 0, domain.TaskCancelled, "отменено админом", release)
	if err != nil || !changed {
		return changed, err
	}
	return true, s.store.Repos().Audit.Add(ctx, &domain.AuditEntry{
		AdminID: admin, Action: "task.cancel", Entity: "task", EntityID: fmt.Sprint(taskID), At: s.now().UTC(),
	})
}

// Expire помечает принятое задание просроченным (срок вышел, отчёта нет). false, если статус уже другой.
// Код промокода остаётся за заданием: покупатель ещё может прислать отчёт, а админ может отменить задание.
func (s *Tasks) Expire(ctx context.Context, taskID int64) (bool, error) {
	return s.transition(ctx, taskID, 0, domain.TaskExpired, "срок вышел", nil)
}

// Card задание со сценарием, версией и покупателем.
func (s *Tasks) Card(ctx context.Context, id int64) (*TaskCard, error) {
	r := s.store.Repos()
	t, err := r.Tasks.Get(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.card(ctx, r, t)
}

func (s *Tasks) card(ctx context.Context, r storage.Repos, t *domain.Task) (*TaskCard, error) {
	sc, err := r.Scenarios.GetByID(ctx, t.ScenarioID)
	if err != nil {
		return nil, err
	}
	v, err := r.Scenarios.GetVersion(ctx, t.ScenarioVersionID)
	if err != nil {
		return nil, err
	}
	u, err := r.Users.Get(ctx, t.UserID)
	if err != nil {
		return nil, err
	}
	c := &TaskCard{Task: t, Scenario: sc, Version: v, User: u}
	if c.Promo, err = r.Promos.ByTask(ctx, t.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if c.Comp, err = r.Comps.ByTask(ctx, t.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if c.Report, err = r.Reports.ByTask(ctx, t.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if c.Item, err = r.Items.ByTask(ctx, t.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	return c, nil
}

// Events история задания.
func (s *Tasks) Events(ctx context.Context, id int64) ([]*domain.TaskEvent, error) {
	return s.store.Repos().Tasks.Events(ctx, id)
}

// BuyerStatuses статусы, которые покупатель видит в своём списке.
var BuyerStatuses = []domain.TaskStatus{domain.TaskSent, domain.TaskAccepted, domain.TaskExpired, domain.TaskReported, domain.TaskRework}

// ForUser задания покупателя (новые сверху).
func (s *Tasks) ForUser(ctx context.Context, userID int64) ([]*TaskCard, error) {
	r := s.store.Repos()
	// Проверенные задания показываем, пока компенсация не закрыта: по ней может понадобиться действие покупателя.
	list, err := r.Tasks.List(ctx, storage.TaskFilter{UserID: userID, Statuses: BuyerStatuses, ReviewedMode: storage.ReviewedOpenComp}, 20, 0)
	if err != nil {
		return nil, err
	}
	return s.cards(ctx, r, list)
}

// Page страница заданий для админа с общим числом.
func (s *Tasks) Page(ctx context.Context, f storage.TaskFilter, limit, offset int) ([]*TaskCard, int, error) {
	r := s.store.Repos()
	list, err := r.Tasks.List(ctx, f, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.Tasks.Count(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	cards, err := s.cards(ctx, r, list)
	return cards, total, err
}

func (s *Tasks) cards(ctx context.Context, r storage.Repos, list []*domain.Task) ([]*TaskCard, error) {
	out := make([]*TaskCard, 0, len(list))
	for _, t := range list {
		c, err := s.card(ctx, r, t)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
