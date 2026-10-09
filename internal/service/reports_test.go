package service

import (
	"botech/internal/storage"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"botech/internal/domain"
)

var reportBody = domain.ScenarioBody{Questions: []domain.Question{
	{Key: "rate", Text: "Оценка", Type: domain.QRating, Required: true},
	{Key: "yn", Text: "Да/нет", Type: domain.QYesNo, Required: true},
	{Key: "photo", Text: "Фото", Type: domain.QPhoto, Required: true},
	{Key: "note", Text: "Комментарий", Type: domain.QText, Required: false},
}}

func goodAnswers() []domain.Answer {
	return []domain.Answer{
		{Key: "rate", Value: "5"},
		{Key: "yn", Value: "yes"},
		{Key: "photo", FileID: "F1", FileUniqueID: "U1"},
	}
}

func TestValidateAnswers(t *testing.T) {
	tests := []struct {
		name    string
		mod     func([]domain.Answer) []domain.Answer
		wantErr string
	}{
		{"всё верно, необязательный пропущен", func(a []domain.Answer) []domain.Answer { return a }, ""},
		{"нет обязательного", func(a []domain.Answer) []domain.Answer { return a[:2] }, "вопрос 3 обязателен"},
		{"обязательный пропущен", func(a []domain.Answer) []domain.Answer { a[0].Skipped = true; return a }, "вопрос 1 обязателен"},
		{"оценка 6", func(a []domain.Answer) []domain.Answer { a[0].Value = "6"; return a }, "оценка от 1 до 5"},
		{"оценка не число", func(a []domain.Answer) []domain.Answer { a[0].Value = "отлично"; return a }, "оценка от 1 до 5"},
		{"да/нет неверно", func(a []domain.Answer) []domain.Answer { a[1].Value = "maybe"; return a }, "да или нет"},
		{"фото без файла", func(a []domain.Answer) []domain.Answer { a[2].FileID = ""; return a }, "нужен файл"},
		{"лишний ответ", func(a []domain.Answer) []domain.Answer { return append(a, domain.Answer{Key: "zzz", Value: "x"}) }, "лишний ответ"},
		{"слишком длинный текст", func(a []domain.Answer) []domain.Answer {
			return append(a, domain.Answer{Key: "note", Value: strings.Repeat("я", MaxTextAnswer+1)})
		}, "текст от 1"},
		{"необязательный текст заполнен", func(a []domain.Answer) []domain.Answer {
			return append(a, domain.Answer{Key: "note", Value: " ок "})
		}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ValidateAnswers(reportBody, tc.mod(goodAnswers()))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("неожиданная ошибка: %v", err)
				}
				if len(out) != len(reportBody.Questions) {
					t.Fatalf("ответов %d, ожидали по числу вопросов (%d)", len(out), len(reportBody.Questions))
				}
				return
			}
			if !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ошибка %v, ожидали %q", err, tc.wantErr)
			}
		})
	}
}

const reportScenario = `key: report-test
title: Отчёт
operator: BTS
steps: [Шаг]
questions:
  - {key: rate, text: Оценка, type: rating}
  - {key: yn, text: Да или нет, type: yesno}
  - {key: photo, text: Фото, type: photo}
  - {key: note, text: Комментарий, type: text, required: false}
`

func (e *env) reports() *Reports { return NewReports(e.store, e.tasks) }

// acceptedTask выдаёт и принимает задание, возвращает его id.
func (e *env) acceptedTask(t *testing.T, scenarioID, user int64) int64 {
	t.Helper()
	id := e.assignOne(t, scenarioID, user)
	if _, _, err := e.tasks.Accept(context.Background(), user, id); err != nil {
		t.Fatal(err)
	}
	e.ensureItem(t, user, id)
	return id
}

// ensureItem выбирает покупателю айтем под задание (отчёт покупателя без айтема не принимается).
// Для заданий продавца и заданий с уже выбранным айтемом ничего не делает.
func (e *env) ensureItem(t *testing.T, user, taskID int64) {
	t.Helper()
	ctx := context.Background()
	card, err := e.tasks.Card(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Item != nil || card.Version.Body.Kind == domain.KindSeller {
		return
	}
	items := NewItems(e.store)
	line := fmt.Sprintf("Товар %d;https://shop.uz/%d-%d", taskID, taskID, time.Now().UnixNano())
	if _, err := items.Add(ctx, firstAdmin, line); err != nil {
		t.Fatal(err)
	}
	free, _, err := items.ListFree(ctx, 1, 0)
	if err != nil || len(free) == 0 {
		t.Fatalf("нет свободного айтема: %v", err)
	}
	if _, err := items.Choose(ctx, user, taskID, free[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitReport(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	id := e.acceptedTask(t, sc.ID, 1)
	r := e.reports()

	comp := &CompInput{Amount: 150000, ReceiptFileID: "R1", ReceiptUniqueID: "RU1"}
	res, err := r.Submit(ctx, 1, id, SubmitInput{Answers: goodAnswers(), Comp: comp})
	if err != nil {
		t.Fatal(err)
	}
	if res.Card.Task.Status != domain.TaskReported || res.Late || res.DupReceipt != 0 {
		t.Fatalf("итог: статус=%s late=%v dup=%d", res.Card.Task.Status, res.Late, res.DupReceipt)
	}
	rep, _ := r.Get(ctx, id)
	if rep == nil || len(rep.Answers) != 4 || !rep.Answers[3].Skipped {
		t.Fatalf("сохранённый отчёт: %+v", rep)
	}
	if res.Card.Comp == nil || res.Card.Comp.Amount != 150000 || res.Card.Comp.Status != domain.CompPending {
		t.Fatalf("компенсация: %+v", res.Card.Comp)
	}

	// Повторная отправка запрещена.
	if _, err := r.Submit(ctx, 1, id, SubmitInput{Answers: goodAnswers()}); !errors.Is(err, ErrAlreadyReported) {
		t.Fatalf("повторный отчёт: %v", err)
	}
	// Проверка админом теперь возможна.
	if ok, err := e.tasks.Review(ctx, firstAdmin, id); err != nil || !ok {
		t.Fatalf("проверка: ok=%v err=%v", ok, err)
	}
}

func TestSubmitReportRules(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	r := e.reports()

	// Отчёт по непринятому заданию нельзя.
	sent := e.assignOne(t, sc.ID, 1)
	if _, err := r.Submit(ctx, 1, sent, SubmitInput{Answers: goodAnswers()}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("отчёт до принятия: %v", err)
	}
	// Чужой покупатель.
	if _, _, err := e.tasks.Accept(ctx, 1, sent); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, 2, sent, SubmitInput{Answers: goodAnswers()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("чужой отчёт: %v", err)
	}
	// Некорректные данные не должны менять статус задания.
	bad := []struct {
		name string
		in   SubmitInput
	}{
		{"нет ответов", SubmitInput{}},
		{"сумма 0", SubmitInput{Answers: goodAnswers(), Comp: &CompInput{Amount: 0, ReceiptFileID: "R"}}},
		{"сумма слишком большая", SubmitInput{Answers: goodAnswers(), Comp: &CompInput{Amount: MaxCompAmount + 1, ReceiptFileID: "R"}}},
		{"нет чека", SubmitInput{Answers: goodAnswers(), Comp: &CompInput{Amount: 100}}},
	}
	e.ensureItem(t, 1, sent)
	for _, tc := range bad {
		if _, err := r.Submit(ctx, 1, sent, tc.in); !errors.Is(err, ErrInvalidReport) {
			t.Errorf("%s: ожидали ErrInvalidReport, получили %v", tc.name, err)
		}
	}
	card, _ := e.tasks.Card(ctx, sent)
	if card.Task.Status != domain.TaskAccepted || card.Comp != nil {
		t.Fatalf("после отказов статус=%s comp=%v, должно остаться как было", card.Task.Status, card.Comp)
	}
}

func TestSubmitLateAndDuplicateReceipt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	r := e.reports()

	first := e.acceptedTask(t, sc.ID, 1)
	if _, err := r.Submit(ctx, 1, first, SubmitInput{Answers: goodAnswers(),
		Comp: &CompInput{Amount: 1000, ReceiptFileID: "R1", ReceiptUniqueID: "SAME"}}); err != nil {
		t.Fatal(err)
	}

	// Второй покупатель просрочил и приложил тот же файл чека.
	second := e.acceptedTask(t, sc.ID, 2)
	if _, err := e.store.DB().ExecContext(ctx, `UPDATE tasks SET status = 'expired' WHERE id = ?`, second); err != nil {
		t.Fatal(err)
	}
	res, err := r.Submit(ctx, 2, second, SubmitInput{Answers: goodAnswers(),
		Comp: &CompInput{Amount: 2000, ReceiptFileID: "R2", ReceiptUniqueID: "SAME"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Late {
		t.Error("отчёт по просроченному заданию должен помечаться опоздавшим")
	}
	if res.DupReceipt != first {
		t.Errorf("дубль чека: получили задание %d, ожидали %d", res.DupReceipt, first)
	}
	events, _ := e.tasks.Events(ctx, second)
	last := events[len(events)-1]
	if last.ToStatus != domain.TaskReported || last.Details != "с опозданием" {
		t.Errorf("последнее событие: %+v", last)
	}
	// В карточке компенсации тоже видно предупреждение.
	cc, _ := r.CompCardByID(ctx, res.Card.Comp.ID)
	if cc.DupReceipt != first {
		t.Errorf("CompCard.DupReceipt=%d", cc.DupReceipt)
	}
}

// Двадцать одновременных отправок одного отчёта должны сохранить ровно один.
func TestSubmitConcurrent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	id := e.acceptedTask(t, sc.ID, 1)
	r := e.reports()

	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Submit(ctx, 1, id, SubmitInput{Answers: goodAnswers(),
				Comp: &CompInput{Amount: 500, ReceiptFileID: "R", ReceiptUniqueID: "U"}})
			if err == nil {
				ok.Add(1)
			} else if !errors.Is(err, ErrAlreadyReported) {
				t.Errorf("неожиданная ошибка: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("успешно отправлено %d отчётов, ожидали 1", ok.Load())
	}
	if _, total, sum, _ := r.CompPage(ctx, domain.CompPending, 10, 0); total != 1 || sum != 500 {
		t.Fatalf("компенсаций %d на сумму %d, ожидали 1 на 500", total, sum)
	}
}

func TestCompensationPaid(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	id := e.acceptedTask(t, sc.ID, 1)
	r := e.reports()
	res, err := r.Submit(ctx, 1, id, SubmitInput{Answers: goodAnswers(),
		Comp: &CompInput{Amount: 70000, ReceiptFileID: "R", ReceiptUniqueID: "U"}})
	if err != nil {
		t.Fatal(err)
	}
	cid := res.Card.Comp.ID

	if _, total, sum, _ := r.CompPage(ctx, domain.CompPending, 10, 0); total != 1 || sum != 70000 {
		t.Fatalf("к выплате: %d на %d", total, sum)
	}
	if ok, err := r.MarkPaid(ctx, firstAdmin, cid); err != nil || !ok {
		t.Fatalf("выплата: ok=%v err=%v", ok, err)
	}
	if ok, _ := r.MarkPaid(ctx, firstAdmin, cid); ok {
		t.Fatal("повторная отметка «выплачено» не должна срабатывать")
	}
	if _, total, _, _ := r.CompPage(ctx, domain.CompPending, 10, 0); total != 0 {
		t.Fatal("после выплаты в списке к выплате пусто")
	}
	cc, _ := r.CompCardByID(ctx, cid)
	if cc.Comp.Status != domain.CompPaid || cc.Comp.PaidBy != firstAdmin || cc.Comp.PaidAt.IsZero() {
		t.Fatalf("карточка: %+v", cc.Comp)
	}
	_ = time.Now
}

// reportOK отправляет валидный отчёт по заданию сценария scenarioYAML (один текстовый вопрос).
func (e *env) reportOK(t *testing.T, user, taskID int64) {
	t.Helper()
	e.ensureItem(t, user, taskID)
	if _, err := e.reports().Submit(context.Background(), user, taskID, SubmitInput{
		Answers: []domain.Answer{{Key: "q_one", Value: "ответ"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func goodInput(amount int64, receipt string) SubmitInput {
	return SubmitInput{
		Answers: goodAnswers(),
		Comp:    &CompInput{Amount: amount, ReceiptFileID: receipt, ReceiptUniqueID: receipt + "-u"},
	}
}

// Возврат отчёта на доработку: комментарий, новая версия, компенсация заменяется, использование промокода не дублируется.
func TestReworkCycle(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addCodes(t, 2)
	sc := e.importScenario(t, reportScenario).Scenario
	r := e.reports()
	id := e.acceptedTask(t, sc.ID, 1)

	// Нельзя вернуть на доработку то, что ещё не отправлено.
	if _, changed, err := r.ReturnForRework(ctx, firstAdmin, id, "что-то"); err != nil || changed {
		t.Fatalf("возврат до отчёта: changed=%v err=%v", changed, err)
	}
	if _, err := r.Submit(ctx, 1, id, goodInput(1000, "R1")); err != nil {
		t.Fatal(err)
	}
	usedAfterFirst := e.stats(t)

	// Пустой комментарий не принимается, статус не меняется.
	if _, _, err := r.ReturnForRework(ctx, firstAdmin, id, "   "); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("пустой комментарий: %v", err)
	}
	card, changed, err := r.ReturnForRework(ctx, firstAdmin, id, "  Фото чека не читается  ")
	if err != nil || !changed || card.Task.Status != domain.TaskRework {
		t.Fatalf("возврат: changed=%v err=%v card=%+v", changed, err, card)
	}
	if card.Report == nil || card.Report.Decision != domain.ReportRework || card.Report.AdminComment != "Фото чека не читается" {
		t.Fatalf("комментарий админа: %+v", card.Report)
	}
	if _, changed, _ := r.ReturnForRework(ctx, firstAdmin, id, "ещё раз"); changed {
		t.Fatal("повторный возврат не должен срабатывать")
	}
	if ok, _ := e.tasks.Review(ctx, firstAdmin, id); ok {
		t.Fatal("отчёт на доработке нельзя отметить проверенным")
	}

	// Покупатель исправляет. Срок давно прошёл, но исправление не считается опозданием.
	if _, err := e.store.DB().ExecContext(ctx, `UPDATE tasks SET due_at = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CanReport(ctx, 1, id); err != nil {
		t.Fatalf("CanReport на доработке: %v", err)
	}
	fixed := goodInput(2500, "R2")
	fixed.Answers[0].Value = "4"
	res, err := r.Submit(ctx, 1, id, fixed)
	if err != nil {
		t.Fatal(err)
	}
	if res.Late || res.Card.Task.Status != domain.TaskReported {
		t.Fatalf("после исправления: late=%v статус=%s", res.Late, res.Card.Task.Status)
	}
	if res.Report.Revision != 2 || res.Card.Report.Answers[0].Value != "4" || res.Card.Report.Decision != domain.ReportPending {
		t.Fatalf("версия отчёта: %+v", res.Report)
	}
	if res.Card.Comp.Amount != 2500 || res.Card.Comp.ReceiptFileID != "R2" || res.Card.Comp.Status != domain.CompPending {
		t.Fatalf("компенсация должна обновиться: %+v", res.Card.Comp)
	}
	if st := e.stats(t); st != usedAfterFirst {
		t.Fatalf("повторная отправка не должна менять учёт промокодов: было %+v, стало %+v", usedAfterFirst, st)
	}
	var versions int
	_ = e.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE task_id = ?`, id).Scan(&versions)
	if versions != 2 {
		t.Fatalf("версий отчёта в истории %d, ожидали 2", versions)
	}

	// Теперь отчёт принимается, решение фиксируется.
	if ok, err := e.tasks.Review(ctx, firstAdmin, id); err != nil || !ok {
		t.Fatalf("проверка: ok=%v err=%v", ok, err)
	}
	card, _ = e.tasks.Card(ctx, id)
	if card.Report.Decision != domain.ReportAccepted {
		t.Fatalf("решение: %q", card.Report.Decision)
	}
	// История содержит комментарий админа.
	events, _ := e.tasks.Events(ctx, id)
	found := false
	for _, ev := range events {
		found = found || (ev.ToStatus == domain.TaskRework && strings.Contains(ev.Details, "Фото чека не читается"))
	}
	if !found {
		t.Fatal("комментарий возврата должен быть в истории задания")
	}
}

// Отклонение компенсации: комментарий, исправление покупателем, выплата только после исправления.
func TestRejectAndResubmitCompensation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	r := e.reports()
	id := e.acceptedTask(t, sc.ID, 1)
	res, err := r.Submit(ctx, 1, id, goodInput(9000, "R1"))
	if err != nil {
		t.Fatal(err)
	}
	cid := res.Card.Comp.ID

	if _, _, err := r.RejectCompensation(ctx, firstAdmin, cid, ""); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("пустой комментарий: %v", err)
	}
	cc, changed, err := r.RejectCompensation(ctx, firstAdmin, cid, "Сумма не совпадает с чеком")
	if err != nil || !changed || cc.Comp.Status != domain.CompRejected || cc.Comp.AdminComment != "Сумма не совпадает с чеком" {
		t.Fatalf("отклонение: changed=%v err=%v %+v", changed, err, cc)
	}
	if ok, _ := r.MarkPaid(ctx, firstAdmin, cid); ok {
		t.Fatal("отклонённую компенсацию выплатить нельзя")
	}
	if _, changed, _ := r.RejectCompensation(ctx, firstAdmin, cid, "повтор"); changed {
		t.Fatal("повторное отклонение не должно срабатывать")
	}
	if _, total, _, _ := r.CompPage(ctx, domain.CompRejected, 10, 0); total != 1 {
		t.Fatalf("в списке отклонённых %d, ожидали 1", total)
	}

	// Исправление покупателем.
	if _, err := r.ResubmitCompensation(ctx, 2, id, CompInput{Amount: 100, ReceiptFileID: "X"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("чужой покупатель: %v", err)
	}
	if _, err := r.ResubmitCompensation(ctx, 1, id, CompInput{Amount: 0, ReceiptFileID: "X"}); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("неверная сумма: %v", err)
	}
	if _, err := r.ResubmitCompensation(ctx, 1, id, CompInput{Amount: 8500, ReceiptFileID: "R2", ReceiptUniqueID: "R2-u"}); err != nil {
		t.Fatal(err)
	}
	cc, _ = r.CompCardByID(ctx, cid)
	if cc.Comp.Status != domain.CompPending || cc.Comp.Amount != 8500 || cc.Comp.AdminComment != "" {
		t.Fatalf("после исправления: %+v", cc.Comp)
	}
	// Исправлять можно только отклонённую.
	if _, err := r.ResubmitCompensation(ctx, 1, id, CompInput{Amount: 1, ReceiptFileID: "X"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("исправление не отклонённой: %v", err)
	}
	if ok, err := r.MarkPaid(ctx, firstAdmin, cid); err != nil || !ok {
		t.Fatalf("выплата после исправления: ok=%v err=%v", ok, err)
	}
	if _, changed, _ := r.RejectCompensation(ctx, firstAdmin, cid, "поздно"); changed {
		t.Fatal("выплаченную компенсацию отклонить нельзя")
	}
}

// Раздел «Отчёты»: принятый, но не выплаченный отчёт остаётся в списке, пока компенсация не закрыта.
func TestReviewedStaysUntilCompensationClosed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	r := e.reports()
	id := e.acceptedTask(t, sc.ID, 1)
	res, err := r.Submit(ctx, 1, id, goodInput(700, "R1"))
	if err != nil {
		t.Fatal(err)
	}
	reports := storage.TaskFilter{Statuses: []domain.TaskStatus{domain.TaskReported, domain.TaskRework}, ReviewedMode: storage.ReviewedOpenComp}
	closed := storage.TaskFilter{Statuses: []domain.TaskStatus{domain.TaskDeclined, domain.TaskCancelled}, ReviewedMode: storage.ReviewedSettled}
	count := func(f storage.TaskFilter) int {
		_, n, err := e.tasks.Page(ctx, f, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if count(reports) != 1 || count(closed) != 0 {
		t.Fatalf("отчёт ждёт проверки: отчёты=%d закрыто=%d", count(reports), count(closed))
	}
	if ok, _ := e.tasks.Review(ctx, firstAdmin, id); !ok {
		t.Fatal("проверка")
	}
	if count(reports) != 1 || count(closed) != 0 {
		t.Fatalf("принят, но не выплачен, должен оставаться в «Отчётах»: отчёты=%d закрыто=%d", count(reports), count(closed))
	}
	if _, _, err := r.RejectCompensation(ctx, firstAdmin, res.Card.Comp.ID, "неверный чек"); err != nil {
		t.Fatal(err)
	}
	if count(reports) != 1 {
		t.Fatal("с отклонённой компенсацией задание остаётся в «Отчётах»")
	}
	if _, err := r.ResubmitCompensation(ctx, 1, id, CompInput{Amount: 700, ReceiptFileID: "R3", ReceiptUniqueID: "R3-u"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.MarkPaid(ctx, firstAdmin, res.Card.Comp.ID); !ok {
		t.Fatal("выплата")
	}
	if count(reports) != 0 || count(closed) != 1 {
		t.Fatalf("после выплаты задание закрыто: отчёты=%d закрыто=%d", count(reports), count(closed))
	}
	// Покупатель видит проверенное задание, пока компенсация открыта (и не видит после выплаты).
	if list, _ := e.tasks.ForUser(ctx, 1); len(list) != 0 {
		t.Fatalf("после выплаты в списке покупателя пусто, а там %d", len(list))
	}
}
