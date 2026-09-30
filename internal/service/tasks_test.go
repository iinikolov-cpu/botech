package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"botech/internal/domain"
	"botech/internal/storage/sqlite"
)

const scenarioYAML = `key: test-scenario
title: Тестовый сценарий
operator: BTS
steps: [Шаг один]
questions:
  - {key: q_one, text: Вопрос, type: text}
`

type env struct {
	store     *sqlite.Store
	access    *Access
	scenarios *Scenarios
	tasks     *Tasks
}

// newEnv поднимает все сервисы на временной БД, каждый промокод можно использовать 1 раз.
func newEnv(t *testing.T) *env { return newEnvUses(t, 1) }

// newEnvUses то же, но с заданным лимитом использований промокода. Создаёт покупателей 1..3.
func newEnvUses(t *testing.T, promoUses int) *env {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e := &env{store: store, access: NewAccess(store, firstAdmin), scenarios: NewScenarios(store), tasks: NewTasks(store, promoUses)}
	ctx := context.Background()
	if err := e.access.EnsureFirstAdmin(ctx); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 3; id++ {
		now := time.Now().UTC()
		if err := store.Repos().Users.Create(ctx, &domain.User{
			TgID: id, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) importScenario(t *testing.T, yaml string) *ImportResult {
	t.Helper()
	res, errs, err := e.scenarios.Import(context.Background(), firstAdmin, []byte(yaml))
	if err != nil || len(errs) > 0 {
		t.Fatalf("импорт: err=%v, ошибки=%v", err, errs)
	}
	return res
}

// assignOne назначает задание и отправляет его; возвращает id задания.
func (e *env) assignOne(t *testing.T, scenarioID, user int64) int64 {
	t.Helper()
	res, err := e.tasks.Assign(context.Background(), firstAdmin, scenarioID, 3, []int64{user})
	if err != nil || len(res.Created) != 1 {
		t.Fatalf("назначение: err=%v res=%+v", err, res)
	}
	id := res.Created[0].ID
	if err := e.tasks.MarkSent(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestScenarioVersioning(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	r1 := e.importScenario(t, scenarioYAML)
	if !r1.Created || r1.Version.Version != 1 {
		t.Fatalf("первая загрузка: %+v", r1)
	}
	if r := e.importScenario(t, scenarioYAML); !r.Unchanged || r.Version.Version != 1 {
		t.Fatalf("повторная загрузка без изменений не должна создавать версию: %+v", r)
	}

	taskID := e.assignOne(t, r1.Scenario.ID, 1) // задание выдано по v1

	changed := strings.Replace(scenarioYAML, "Шаг один", "Совсем другой шаг", 1)
	r2 := e.importScenario(t, changed)
	if r2.Created || r2.Unchanged || r2.Version.Version != 2 {
		t.Fatalf("изменение должно дать v2: %+v", r2)
	}

	card, err := e.tasks.Card(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Version.Version != 1 || card.Version.Body.Steps[0] != "Шаг один" {
		t.Fatalf("старое задание должно остаться на v1, а показывает v%d %v", card.Version.Version, card.Version.Body.Steps)
	}

	// Новое задание уже на v2.
	id2 := e.assignOne(t, r1.Scenario.ID, 2)
	card2, _ := e.tasks.Card(ctx, id2)
	if card2.Version.Version != 2 {
		t.Fatalf("новое задание должно быть на v2, а на v%d", card2.Version.Version)
	}
}

func TestImportValidationErrors(t *testing.T) {
	e := newEnv(t)
	res, errs, err := e.scenarios.Import(context.Background(), firstAdmin, []byte("key: x"))
	if err != nil || res != nil || len(errs) == 0 {
		t.Fatalf("невалидный файл: res=%v errs=%v err=%v", res, errs, err)
	}
}

func TestArchivedScenarioRestoredOnImport(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.importScenario(t, scenarioYAML)
	if err := e.scenarios.SetArchived(ctx, firstAdmin, r.Scenario.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tasks.Assign(ctx, firstAdmin, r.Scenario.ID, 3, []int64{1}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("сценарий в архиве назначать нельзя, err=%v", err)
	}
	changed := strings.Replace(scenarioYAML, "Вопрос", "Другой вопрос", 1)
	if r2 := e.importScenario(t, changed); !r2.Restored {
		t.Fatalf("загрузка нового содержимого должна вернуть сценарий из архива: %+v", r2)
	}
}

func TestAssign(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario

	// Блокируем покупателя 3: он не должен получить задание.
	if _, err := e.access.SetStatus(ctx, firstAdmin, 3, domain.StatusBlocked); err != nil {
		t.Fatal(err)
	}

	res, err := e.tasks.Assign(ctx, firstAdmin, sc.ID, 5, []int64{1, 2, 2, 3, 99, firstAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 {
		t.Errorf("создано %d заданий, ожидали 2 (покупатели 1 и 2, дубль id=2 игнорируется)", len(res.Created))
	}
	if len(res.Invalid) != 3 { // заблокированный 3, несуществующий 99, админ
		t.Errorf("недопустимых %v, ожидали 3", res.Invalid)
	}

	// Повторная выдача того же сценария тому же покупателю пропускается.
	res2, err := e.tasks.Assign(ctx, firstAdmin, sc.ID, 5, []int64{1})
	if err != nil || len(res2.Created) != 0 || len(res2.Skipped) != 1 {
		t.Errorf("дубль: err=%v res=%+v", err, res2)
	}

	tests := []struct {
		name string
		days int
	}{{"ноль дней", 0}, {"слишком много", MaxDueDays + 1}}
	for _, tc := range tests {
		if _, err := e.tasks.Assign(ctx, firstAdmin, sc.ID, tc.days, []int64{2}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: ожидали ErrForbidden, получили %v", tc.name, err)
		}
	}
}

// Дубль при одновременном назначении: уникальный индекс в БД не даст создать два активных задания.
func TestAssignConcurrentNoDuplicates(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario

	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.tasks.Assign(ctx, firstAdmin, sc.ID, 3, []int64{1})
			if err == nil {
				created.Add(int32(len(res.Created)))
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("создано %d заданий, ожидали ровно 1", created.Load())
	}
}

func TestAcceptFlow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)

	// Чужой покупатель не может принять задание.
	if _, _, err := e.tasks.Accept(ctx, 2, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("чужое задание: ожидали ErrNotFound, получили %v", err)
	}

	before := time.Now()
	card, changed, err := e.tasks.Accept(ctx, 1, id)
	if err != nil || !changed || card.Task.Status != domain.TaskAccepted {
		t.Fatalf("принятие: err=%v changed=%v card=%+v", err, changed, card)
	}
	// Срок отсчитывается от принятия: 3 дня.
	wantDue := before.Add(3 * 24 * time.Hour)
	if d := card.Task.DueAt.Sub(wantDue); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("due_at=%v, ожидали около %v", card.Task.DueAt, wantDue)
	}

	// Повторное принятие и отказ после принятия ничего не меняют.
	if _, changed, _ := e.tasks.Accept(ctx, 1, id); changed {
		t.Error("повторное принятие не должно менять состояние")
	}
	if card, changed, _ := e.tasks.Decline(ctx, 1, id); changed || card.Task.Status != domain.TaskAccepted {
		t.Error("отказ после принятия должен игнорироваться")
	}

	// История: создано, отправлено, принято.
	events, _ := e.tasks.Events(ctx, id)
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind+":"+string(ev.ToStatus))
	}
	if got := strings.Join(kinds, ","); got != "created:created,status:sent,status:accepted,promo_missing:" {
		t.Errorf("история %q", got)
	}
}

func TestDecline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)

	card, changed, err := e.tasks.Decline(ctx, 1, id)
	if err != nil || !changed || card.Task.Status != domain.TaskDeclined || card.Task.DeclinedAt.IsZero() {
		t.Fatalf("отказ: err=%v changed=%v card=%+v", err, changed, card)
	}
	// После отказа задание не активно, тот же сценарий можно выдать снова.
	if res, _ := e.tasks.Assign(ctx, firstAdmin, sc.ID, 3, []int64{1}); len(res.Created) != 1 {
		t.Error("после отказа сценарий можно назначить повторно")
	}
}

// Одновременные нажатия «Принять» и «Отказаться»: побеждает ровно одно действие.
func TestConcurrentAcceptDecline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var changed bool
			if i%2 == 0 {
				_, changed, _ = e.tasks.Accept(ctx, 1, id)
			} else {
				_, changed, _ = e.tasks.Decline(ctx, 1, id)
			}
			if changed {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("сменили статус %d раз, ожидали ровно 1", wins.Load())
	}
	events, _ := e.tasks.Events(ctx, id)
	statusEvents := 0
	for _, ev := range events {
		if ev.Kind == "status" && ev.FromStatus == domain.TaskSent {
			statusEvents++
		}
	}
	if statusEvents != 1 {
		t.Fatalf("событий перехода из «отправлено»: %d, ожидали 1", statusEvents)
	}
}

func TestReviewOnlyAfterReport(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)

	if changed, err := e.tasks.Review(ctx, firstAdmin, id); err != nil || changed {
		t.Fatalf("проверить неотправленный отчёт нельзя: changed=%v err=%v", changed, err)
	}
	// Имитируем получение отчёта (сам приём отчётов появится на этапе 3).
	if _, err := e.store.Repos().Tasks.Transition(ctx, id, domain.TaskSent, domain.TaskAccepted, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Repos().Tasks.Transition(ctx, id, domain.TaskAccepted, domain.TaskReported, time.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if changed, err := e.tasks.Review(ctx, firstAdmin, id); err != nil || !changed {
		t.Fatalf("проверка отчёта: changed=%v err=%v", changed, err)
	}
}

func TestDialogState(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	d := NewDialog(e.store)
	type data struct{ N []int64 }

	if st, err := d.Get(ctx, 1, &data{}); err != nil || st != "" {
		t.Fatalf("пустое состояние: %q %v", st, err)
	}
	if err := d.Set(ctx, 1, "assign", data{N: []int64{5, 6}}); err != nil {
		t.Fatal(err)
	}
	var got data
	if st, err := d.Get(ctx, 1, &got); err != nil || st != "assign" || len(got.N) != 2 {
		t.Fatalf("состояние: %q %+v %v", st, got, err)
	}
	if err := d.Clear(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Get(ctx, 1, nil); st != "" {
		t.Fatal("после Clear состояния быть не должно")
	}
}

// Кнопка нажата раньше, чем бот записал «отправлено»: принятие всё равно должно сработать.
func TestAcceptBeforeMarkSent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	res, err := e.tasks.Assign(ctx, firstAdmin, sc.ID, 3, []int64{1})
	if err != nil || len(res.Created) != 1 {
		t.Fatal(err)
	}
	id := res.Created[0].ID // статус «создано», MarkSent не вызывали

	card, changed, err := e.tasks.Accept(ctx, 1, id)
	if err != nil || !changed || card.Task.Status != domain.TaskAccepted {
		t.Fatalf("принятие из «создано»: err=%v changed=%v status=%v", err, changed, card.Task.Status)
	}
	if err := e.tasks.MarkSent(ctx, id); err != nil { // запоздалая отметка не должна ничего ломать
		t.Fatal(err)
	}
	card, _ = e.tasks.Card(ctx, id)
	if card.Task.Status != domain.TaskAccepted {
		t.Fatalf("статус после запоздалого MarkSent: %s", card.Task.Status)
	}
}

func TestDeleteTask(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario

	// Отправленное, но не принятое задание удаляется полностью, вместе с историей.
	sent := e.assignOne(t, sc.ID, 1)
	if err := e.tasks.Delete(ctx, firstAdmin, sent); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tasks.Card(ctx, sent); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после удаления карточки быть не должно: %v", err)
	}
	if events, _ := e.tasks.Events(ctx, sent); len(events) != 0 {
		t.Fatalf("история должна быть удалена, осталось %d событий", len(events))
	}
	// Слот освободился: сценарий можно назначить заново.
	res, _ := e.tasks.Assign(ctx, firstAdmin, sc.ID, 3, []int64{1})
	if len(res.Created) != 1 {
		t.Fatal("после удаления задание можно назначить повторно")
	}
	if res.Created[0].ID <= sent {
		t.Fatalf("номер удалённого задания %d переиспользован (новое #%d): старые кнопки в чатах могли бы сработать на чужое задание", sent, res.Created[0].ID)
	}
	// Удаление записано в журнал.
	log, _ := e.access.AuditLog(ctx, 20)
	found := false
	for _, en := range log {
		found = found || en.Action == "task.delete"
	}
	if !found {
		t.Fatal("удаление должно попасть в журнал админов")
	}

	// Отказ тоже можно удалить.
	declined := e.assignOne(t, sc.ID, 2)
	if _, _, err := e.tasks.Decline(ctx, 2, declined); err != nil {
		t.Fatal(err)
	}
	if err := e.tasks.Delete(ctx, firstAdmin, declined); err != nil {
		t.Fatalf("удаление отказа: %v", err)
	}

	// Принятое и с отчётом удалять нельзя.
	accepted := e.acceptedTask(t, sc.ID, 3)
	if err := e.tasks.Delete(ctx, firstAdmin, accepted); !errors.Is(err, ErrForbidden) {
		t.Fatalf("принятое задание: %v", err)
	}
	if _, err := e.reports().Submit(ctx, 3, accepted, SubmitInput{Answers: nil}); err == nil {
		// в сценарии один обязательный вопрос, пустой отчёт должен быть отклонён
		t.Fatal("пустой отчёт принят")
	}
	if err := e.tasks.Delete(ctx, firstAdmin, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("несуществующее: %v", err)
	}
}

func TestCancelTask(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addCodes(t, 1)
	sc := e.importScenario(t, scenarioYAML).Scenario

	// Не принятое задание отменить нельзя (его удаляют).
	sent := e.assignOne(t, sc.ID, 1)
	if ok, err := e.tasks.Cancel(ctx, firstAdmin, sent); err != nil || ok {
		t.Fatalf("отмена неприятого: ok=%v err=%v", ok, err)
	}

	card, _, _ := e.tasks.Accept(ctx, 1, sent)
	promo := card.Promo.Code
	ok, err := e.tasks.Cancel(ctx, firstAdmin, sent)
	if err != nil || !ok {
		t.Fatalf("отмена принятого: ok=%v err=%v", ok, err)
	}
	card, _ = e.tasks.Card(ctx, sent)
	if card.Task.Status != domain.TaskCancelled || card.Promo == nil || card.Promo.Code != promo {
		t.Fatalf("после отмены: статус=%s, промокод должен остаться за заданием: %+v", card.Task.Status, card.Promo)
	}
	if ok, _ := e.tasks.Cancel(ctx, firstAdmin, sent); ok {
		t.Fatal("повторная отмена не должна срабатывать")
	}
	// Отчёт по отменённому заданию отправить нельзя, а слот освободился.
	if _, err := e.reports().Submit(ctx, 1, sent, SubmitInput{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("отчёт по отменённому: %v", err)
	}
	if _, err := e.reports().CanReport(ctx, 1, sent); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CanReport отменённого: %v", err)
	}
	if res, _ := e.tasks.Assign(ctx, firstAdmin, sc.ID, 3, []int64{1}); len(res.Created) != 1 {
		t.Fatal("после отмены сценарий можно назначить снова")
	}
	// Удалять отменённое нельзя: в истории остаются данные учёта.
	if err := e.tasks.Delete(ctx, firstAdmin, sent); !errors.Is(err, ErrForbidden) {
		t.Fatalf("удаление отменённого: %v", err)
	}
	// Задание с отчётом не отменяется.
	e2 := e.acceptedTask(t, sc.ID, 2)
	e.reportOK(t, 2, e2)
	if ok, _ := e.tasks.Cancel(ctx, firstAdmin, e2); ok {
		t.Fatal("задание с отчётом отменять нельзя")
	}
}
