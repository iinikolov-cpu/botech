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

func (e *env) promos() *Promos { return NewPromos(e.store) }

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
	if st, _ := p.Stats(ctx); st.Free != 3 {
		t.Fatalf("в пуле %d кодов, ожидали 3", st.Free)
	}
}

func TestPromoIssuedOnAccept(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addCodes(t, 2)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)

	card, changed, err := e.tasks.Accept(ctx, 1, id)
	if err != nil || !changed || card.Promo == nil || !card.NewPromo || card.PoolLeft != 1 {
		t.Fatalf("принятие с промокодом: err=%v changed=%v card=%+v", err, changed, card)
	}
	// Повторное принятие и повторный запрос кода не выдают второй.
	e.tasks.Accept(ctx, 1, id)
	again, err := e.tasks.IssuePromo(ctx, 1, id)
	if err != nil || again.NewPromo || again.Promo.Code != card.Promo.Code {
		t.Fatalf("повторная выдача: err=%v %+v", err, again)
	}
	if st, _ := e.promos().Stats(ctx); st.Free != 1 || st.Issued != 1 {
		t.Fatalf("статистика пула: %+v", st)
	}
	// Чужой покупатель не может запросить код.
	if _, err := e.tasks.IssuePromo(ctx, 2, id); err == nil {
		t.Fatal("чужой покупатель получил код")
	}
}

func TestPromoEmptyPoolThenLater(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)

	card, changed, err := e.tasks.Accept(ctx, 1, id)
	if err != nil || !changed || card.Promo != nil || card.Task.Status != domain.TaskAccepted {
		t.Fatalf("пустой пул не должен мешать принятию: err=%v %+v", err, card)
	}
	e.addCodes(t, 1)
	card, err = e.tasks.IssuePromo(ctx, 1, id)
	if err != nil || card.Promo == nil || !card.NewPromo {
		t.Fatalf("выдача после пополнения: err=%v %+v", err, card)
	}
}

// Главный тест: 30 покупателей одновременно принимают задания, кодов только 10.
// Каждый код должен уйти ровно одному заданию.
func TestPromoConcurrentAcceptNoDuplicates(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const buyers, codes = 30, 10
	e.addBuyers(t, 100, 100+buyers-1)
	e.addCodes(t, codes)
	sc := e.importScenario(t, scenarioYAML).Scenario

	taskOf := map[int64]int64{}
	for u := int64(100); u < 100+buyers; u++ {
		taskOf[u] = e.assignOne(t, sc.ID, u)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		got     = map[string]int{}
		without atomic.Int32
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
				without.Add(1)
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
			t.Errorf("код %s выдан %d раз", code, n)
		}
	}
	if without.Load() != buyers-codes {
		t.Errorf("без кода осталось %d заданий, ожидали %d", without.Load(), buyers-codes)
	}
	if st, _ := e.promos().Stats(ctx); st.Free != 0 || st.Issued != codes {
		t.Errorf("статистика пула: %+v", st)
	}
}

// Двадцать одновременных запросов кода по одному заданию должны израсходовать один код.
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
	if st, _ := e.promos().Stats(ctx); st.Issued != 1 || st.Free != 4 {
		t.Fatalf("израсходован не один код: %+v", st)
	}
}

func TestPromoMarkUsed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addCodes(t, 1)
	sc := e.importScenario(t, scenarioYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)
	card, _, _ := e.tasks.Accept(ctx, 1, id)

	p := e.promos()
	if ok, err := p.MarkUsed(ctx, firstAdmin, card.Promo.ID); err != nil || !ok {
		t.Fatalf("отметка: ok=%v err=%v", ok, err)
	}
	if ok, _ := p.MarkUsed(ctx, firstAdmin, card.Promo.ID); ok {
		t.Fatal("повторная отметка не должна менять состояние")
	}
	if st, _ := p.Stats(ctx); st.Used != 1 || st.Issued != 0 {
		t.Fatalf("статистика: %+v", st)
	}
}
