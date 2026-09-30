package service

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"botech/internal/domain"
)

func TestParseCodes(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantValid   []string
		wantInvalid []string
	}{
		{"по одному в строке", "AAA111\nBBB222\r\nCCC333\n", []string{"AAA111", "BBB222", "CCC333"}, nil},
		{"через пробел в одной строке", "AAA111 BBB222  CCC333", []string{"AAA111", "BBB222", "CCC333"}, nil},
		{"csv: берётся первая колонка", "code,comment\nAAA111,первый\nBBB222;второй\n", []string{"AAA111", "BBB222"}, nil},
		{"заголовок пропускается", "Промокод\nAAA111", []string{"AAA111"}, nil},
		{"BOM и кавычки", "\xef\xbb\xbf\"AAA111\"\n'BBB222'", []string{"AAA111", "BBB222"}, nil},
		{"мусор отделяется", "AAA111\nа б\nx!\n", []string{"AAA111"}, []string{"а", "б", "x!"}},
		{"слишком короткий", "ab", nil, []string{"ab"}},
		{"пусто", " \n\n", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			valid, invalid := ParseCodes(tc.in)
			if !reflect.DeepEqual(valid, tc.wantValid) || !reflect.DeepEqual(invalid, tc.wantInvalid) {
				t.Fatalf("valid=%v invalid=%v, ожидали %v и %v", valid, invalid, tc.wantValid, tc.wantInvalid)
			}
		})
	}
}

// addBuyers создаёт активных покупателей с id от from до to включительно.
func (e *env) addBuyers(t *testing.T, from, to int64) {
	t.Helper()
	for id := from; id <= to; id++ {
		now := time.Now().UTC()
		if err := e.store.Repos().Users.Create(context.Background(), &domain.User{
			TgID: id, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) promos() *Promos { return NewPromos(e.store, e.tasks.PromoMaxUses()) }

func (e *env) addCodes(t *testing.T, n int) {
	t.Helper()
	var raw string
	for i := 0; i < n; i++ {
		raw += fmt.Sprintf("CODE%04d\n", i)
	}
	res, err := e.promos().Add(context.Background(), firstAdmin, raw)
	if err != nil || res.Added != n {
		t.Fatalf("загрузка кодов: %+v %v", res, err)
	}
}

func (e *env) stats(t *testing.T) domain.PromoStats {
	t.Helper()
	st, err := e.promos().Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPromoAddDuplicates(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	p := e.promos()
	res, err := p.Add(ctx, firstAdmin, "AAA111\nBBB222\nAAA111\n!!!\nаб")
	if err != nil || res.Added != 2 || res.Duplicates != 1 || len(res.Invalid) != 2 {
		t.Fatalf("первая загрузка: %+v %v", res, err)
	}
	res, _ = p.Add(ctx, firstAdmin, "AAA111\nCCC333")
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("повторная загрузка не должна дублировать коды: %+v", res)
	}
	if st := e.stats(t); st.Total != 3 || st.Available != 3 {
		t.Fatalf("сводка: %+v", st)
	}
}

func TestPromoIssuedOnAccept(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t) // один код можно использовать 1 раз
	e.addCodes(t, 2)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)

	card, changed, err := e.tasks.Accept(ctx, 1, id)
	if err != nil || !changed || card.Promo == nil || card.Promo.Outcome != domain.PromoActive || card.PromoReason != PromoReasonNone {
		t.Fatalf("принятие с промокодом: err=%v changed=%v card=%+v", err, changed, card)
	}
	// Повторное принятие и повторный запрос кода не выдают второй.
	e.tasks.Accept(ctx, 1, id)
	again, err := e.tasks.IssuePromo(ctx, 1, id)
	if err != nil || again.Promo.Code != card.Promo.Code {
		t.Fatalf("повторная выдача: err=%v %+v", err, again)
	}
	if st := e.stats(t); st.Available != 1 || st.Busy != 1 {
		t.Fatalf("сводка: %+v", st)
	}
	// Чужой покупатель не может запросить код.
	if _, err := e.tasks.IssuePromo(ctx, 2, id); err == nil {
		t.Fatal("чужой покупатель получил код")
	}
}

// Многоразовый код: пока задание активно, код занят; выполненное задание засчитывает
// использование и возвращает код в оборот, пока лимит не исчерпан.
func TestPromoReusableLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newEnvUses(t, 2)
	e.addCodes(t, 1)
	sc := e.importScenario(t, scenarioYAML).Scenario
	r := e.reports()

	t1 := e.assignOne(t, sc.ID, 1)
	c1, _, _ := e.tasks.Accept(ctx, 1, t1)
	if c1.Promo == nil {
		t.Fatal("первому заданию должен достаться код")
	}
	// Второе задание: код занят активным заданием, но использования ещё есть.
	t2 := e.assignOne(t, sc.ID, 2)
	c2, _, _ := e.tasks.Accept(ctx, 2, t2)
	if c2.Promo != nil || c2.PromoReason != PromoReasonBusy {
		t.Fatalf("код занят: promo=%v reason=%q", c2.Promo, c2.PromoReason)
	}
	if st := e.stats(t); st.Busy != 1 || st.Available != 0 || st.WithUsesLeft != 1 {
		t.Fatalf("сводка при занятом коде: %+v", st)
	}

	// Первое задание выполнено: использование засчитано, код свободен (осталось 1 из 2).
	res, err := r.Submit(ctx, 1, t1, SubmitInput{Answers: []domain.Answer{{Key: "q_one", Value: "ок"}}})
	if err != nil || res.PoolExhausted {
		t.Fatalf("отчёт: err=%v exhausted=%v", err, res.PoolExhausted)
	}
	if st := e.stats(t); st.Available != 1 || st.Busy != 0 {
		t.Fatalf("после выполнения код должен вернуться в оборот: %+v", st)
	}
	rows, _, _ := e.promos().List(ctx, 10, 0)
	if len(rows) != 1 || rows[0].UsedCount != 1 || rows[0].Left != 1 || rows[0].TaskID != 0 {
		t.Fatalf("таблица кодов: %+v", rows)
	}

	// Второе задание получает тот же код по кнопке.
	c2, err = e.tasks.IssuePromo(ctx, 2, t2)
	if err != nil || c2.Promo == nil || c2.Promo.Code != c1.Promo.Code {
		t.Fatalf("повторная выдача: err=%v %+v", err, c2)
	}
	// Второе выполнение исчерпывает код: админу нужно предупреждение.
	res, err = r.Submit(ctx, 2, t2, SubmitInput{Answers: []domain.Answer{{Key: "q_one", Value: "ок"}}})
	if err != nil || !res.PoolExhausted {
		t.Fatalf("исчерпание пула: err=%v exhausted=%v", err, res.PoolExhausted)
	}
	if st := e.stats(t); st.Exhausted != 1 || st.WithUsesLeft != 0 || st.Available != 0 {
		t.Fatalf("сводка после исчерпания: %+v", st)
	}
	// Третье задание: кодов с остатком нет вообще.
	t3 := e.assignOne(t, sc.ID, 3)
	c3, _, _ := e.tasks.Accept(ctx, 3, t3)
	if c3.Promo != nil || c3.PromoReason != PromoReasonExhausted {
		t.Fatalf("нет кодов с остатком: promo=%v reason=%q", c3.Promo, c3.PromoReason)
	}
	// Пополнение снимает проблему.
	if _, err := e.promos().Add(ctx, firstAdmin, "NEWCODE1"); err != nil {
		t.Fatal(err)
	}
	if c3, _ = e.tasks.IssuePromo(ctx, 3, t3); c3.Promo == nil || c3.Promo.Code != "NEWCODE1" {
		t.Fatalf("после пополнения: %+v", c3.Promo)
	}
}

// Отмена задания возвращает код в оборот, но использование не засчитывает.
func TestPromoCancelReleasesWithoutCounting(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addCodes(t, 1)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.acceptedTask(t, sc.ID, 1)
	if st := e.stats(t); st.Busy != 1 {
		t.Fatalf("код должен быть занят: %+v", st)
	}
	if ok, err := e.tasks.Cancel(ctx, firstAdmin, id); err != nil || !ok {
		t.Fatalf("отмена: ok=%v err=%v", ok, err)
	}
	if st := e.stats(t); st.Available != 1 || st.Busy != 0 {
		t.Fatalf("после отмены код свободен: %+v", st)
	}
	rows, _, _ := e.promos().List(ctx, 10, 0)
	if rows[0].UsedCount != 0 {
		t.Fatalf("отмена не должна засчитывать использование: %+v", rows[0])
	}
	card, _ := e.tasks.Card(ctx, id)
	if card.Promo == nil || card.Promo.Outcome != domain.PromoReleased {
		t.Fatalf("история выдачи: %+v", card.Promo)
	}
	// Код можно выдать новому заданию.
	id2 := e.acceptedTask(t, sc.ID, 2)
	if c, _ := e.tasks.Card(ctx, id2); c.Promo == nil {
		t.Fatal("освобождённый код должен выдаваться снова")
	}
}

// Главный тест: 30 покупателей одновременно принимают задания, кодов только 10.
// Каждый код достаётся ровно одному активному заданию, остальные получают причину «заняты».
func TestPromoConcurrentAcceptNoDuplicates(t *testing.T) {
	ctx := context.Background()
	e := newEnvUses(t, 3) // даже при трёх использованиях на код одновременно он держит одно задание
	const buyers, codes = 30, 10
	e.addBuyers(t, 100, 100+buyers-1)
	e.addCodes(t, codes)
	sc := e.importScenario(t, scenarioYAML).Scenario

	taskOf := map[int64]int64{}
	for u := int64(100); u < 100+buyers; u++ {
		taskOf[u] = e.assignOne(t, sc.ID, u)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  = map[string]int{}
		busy atomic.Int32
	)
	for u, id := range taskOf {
		wg.Add(1)
		go func(u, id int64) {
			defer wg.Done()
			card, _, err := e.tasks.Accept(ctx, u, id)
			if err != nil {
				t.Errorf("Accept: %v", err)
				return
			}
			if card.Promo == nil {
				if card.PromoReason == PromoReasonBusy {
					busy.Add(1)
				}
				return
			}
			mu.Lock()
			got[card.Promo.Code]++
			mu.Unlock()
		}(u, id)
	}
	wg.Wait()

	if len(got) != codes {
		t.Fatalf("выдано %d разных кодов, ожидали %d", len(got), codes)
	}
	for code, n := range got {
		if n != 1 {
			t.Errorf("код %s закреплён за %d заданиями одновременно", code, n)
		}
	}
	if busy.Load() != buyers-codes {
		t.Errorf("с причиной «заняты» осталось %d заданий, ожидали %d", busy.Load(), buyers-codes)
	}
	if st := e.stats(t); st.Busy != codes || st.Available != 0 {
		t.Errorf("сводка: %+v", st)
	}
}

// Двадцать одновременных запросов кода по одному заданию должны занять один код.
func TestPromoConcurrentSameTask(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)
	if _, _, err := e.tasks.Accept(ctx, 1, id); err != nil { // пул пуст, код ещё не выдан
		t.Fatal(err)
	}
	e.addCodes(t, 5)

	var wg sync.WaitGroup
	codes := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			card, err := e.tasks.IssuePromo(ctx, 1, id)
			if err != nil {
				t.Errorf("IssuePromo: %v", err)
				return
			}
			codes <- card.Promo.Code
		}()
	}
	wg.Wait()
	close(codes)
	first := ""
	for c := range codes {
		if first == "" {
			first = c
		}
		if c != first {
			t.Fatalf("получены разные коды %s и %s", first, c)
		}
	}
	if st := e.stats(t); st.Busy != 1 || st.Available != 4 {
		t.Fatalf("занят не один код: %+v", st)
	}
}

func TestPromoDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	p := e.promos()
	if _, err := p.Add(ctx, firstAdmin, "AAA111\nBBB222\nCCC333\nDDD444"); err != nil {
		t.Fatal(err)
	}
	// Один код занят активным заданием: его удалять нельзя.
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)
	card, _, _ := e.tasks.Accept(ctx, 1, id)
	busyCode := card.Promo.Code

	res, err := p.DeleteFree(ctx, firstAdmin, busyCode+"\nZZZ999\nBBB222\nBBB222\n!!")
	if err != nil {
		t.Fatal(err)
	}
	wantDeleted := 1
	if busyCode == "BBB222" {
		wantDeleted = 0
	}
	if res.Deleted != wantDeleted || len(res.Invalid) != 1 {
		t.Fatalf("удаление по списку: %+v (занят был %s)", res, busyCode)
	}
	// Одиночное удаление: список из одного кода.
	if res, _ := p.DeleteFree(ctx, firstAdmin, "CCC333"); busyCode != "CCC333" && res.Deleted != 1 {
		t.Fatalf("удаление одного кода: %+v", res)
	}

	st := e.stats(t)
	n, err := p.DeleteAllFree(ctx, firstAdmin)
	if err != nil || n != st.Total-st.Busy {
		t.Fatalf("удалить все незанятые: n=%d err=%v, ожидали %d", n, err, st.Total-st.Busy)
	}
	if st := e.stats(t); st.Total != 1 || st.Busy != 1 {
		t.Fatalf("занятый код должен остаться: %+v", st)
	}

	// После выполнения задания код (уже не занятый) удаляется, а история выдачи остаётся.
	if _, err := e.reports().Submit(ctx, 1, id, SubmitInput{Answers: []domain.Answer{{Key: "q_one", Value: "ок"}}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := p.DeleteAllFree(ctx, firstAdmin); n != 1 {
		t.Fatalf("удалено %d, ожидали 1", n)
	}
	after, _ := e.tasks.Card(ctx, id)
	if after.Promo == nil || after.Promo.Code != busyCode || after.Promo.Outcome != domain.PromoUsed {
		t.Fatalf("история выдачи должна пережить удаление кода: %+v", after.Promo)
	}
}
