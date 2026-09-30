package service

import (
	"context"
	"errors"
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
	return id
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
