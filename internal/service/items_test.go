package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"botech/internal/domain"
)

func TestParseItems(t *testing.T) {
	type want struct {
		title, url string
		price      int64
		note       string
	}
	tests := []struct {
		name    string
		in      string
		items   []want
		invalid int
	}{
		{"точка с запятой", "Куртка;https://olx.uz/a1;150000;синяя", []want{{"Куртка", "https://olx.uz/a1", 150000, "синяя"}}, 0},
		{"запятые как в Google Таблицах", "Куртка,https://olx.uz/a1,150000", []want{{"Куртка", "https://olx.uz/a1", 150000, ""}}, 0},
		{"табуляция и разделитель |", "Шапка\thttps://x.uz/1\t90 000\nСапоги|https://x.uz/2|0|размер 42", []want{
			{"Шапка", "https://x.uz/1", 90000, ""}, {"Сапоги", "https://x.uz/2", 0, "размер 42"}}, 0},
		{"только название и ссылка", "Чайник https://x.uz/3", []want{{"Чайник", "https://x.uz/3", 0, ""}}, 0},
		{"кавычки и BOM", "\xef\xbb\xbf\"Лампа\";\"https://x.uz/4\"", []want{{"Лампа", "https://x.uz/4", 0, ""}}, 0},
		{"заголовок пропускается", "Название;Ссылка;Цена\nЛампа;https://x.uz/5;100", []want{{"Лампа", "https://x.uz/5", 100, ""}}, 0},
		{"пустые строки", "\n\nЛампа;https://x.uz/6\n\n", []want{{"Лампа", "https://x.uz/6", 0, ""}}, 0},
		{"нет ссылки", "Просто текст", nil, 1},
		{"нет названия", ";https://x.uz/7;100", nil, 1},
		{"ссылка не http", "Лампа;ftp://x.uz/8", nil, 1},
		{"слишком длинное название", strings.Repeat("я", 101) + ";https://x.uz/9", nil, 1},
		{"хорошие и плохие вперемешку", "Ок;https://x.uz/10\nмусор\nОк2;https://x.uz/11", []want{{"Ок", "https://x.uz/10", 0, ""}, {"Ок2", "https://x.uz/11", 0, ""}}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			items, invalid := ParseItems(tc.in)
			var got []want
			for _, it := range items {
				got = append(got, want{it.Title, it.URL, it.Price, it.Note})
			}
			if !reflect.DeepEqual(got, tc.items) || len(invalid) != tc.invalid {
				t.Fatalf("получили %+v (некорректных: %v), ожидали %+v (некорректных: %d)", got, invalid, tc.items, tc.invalid)
			}
		})
	}
}

func TestParseIDs(t *testing.T) {
	ids, invalid := ParseIDs("3, 5 #7;3\nабв 0 -2")
	if !reflect.DeepEqual(ids, []int64{3, 5, 7}) || len(invalid) != 3 {
		t.Fatalf("номера: %v, некорректные: %v", ids, invalid)
	}
}

func TestItemsAddListDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	items := NewItems(e.store)

	res, err := items.Add(ctx, firstAdmin, "A;https://x.uz/1;100\nB;https://x.uz/2\nA again;https://x.uz/1\nмусор")
	if err != nil || res.Added != 2 || res.Duplicates != 1 || len(res.Invalid) != 1 {
		t.Fatalf("загрузка: %+v %v", res, err)
	}
	// Повторная загрузка тех же ссылок ничего не добавляет.
	if res, _ = items.Add(ctx, firstAdmin, "B;https://x.uz/2"); res.Added != 0 || res.Duplicates != 1 {
		t.Fatalf("повтор: %+v", res)
	}
	if st, _ := items.Stats(ctx); st.Total != 2 || st.Free != 2 {
		t.Fatalf("сводка: %+v", st)
	}
	list, total, _ := items.List(ctx, 10, 0)
	if total != 2 || len(list) != 2 || list[0].Title != "A" || list[0].Price != 100 {
		t.Fatalf("список: %+v", list)
	}

	// Занятый (выбранный) айтем не удаляется, свободный удаляется.
	sc := e.importScenario(t, scenarioYAML).Scenario
	task := e.assignOne(t, sc.ID, 1)
	if _, _, err := e.tasks.Accept(ctx, 1, task); err != nil {
		t.Fatal(err)
	}
	if _, err := items.Choose(ctx, 1, task, list[0].ID); err != nil {
		t.Fatal(err)
	}
	del, err := items.DeleteIDs(ctx, firstAdmin, "1, 2, 99, абв")
	if err != nil || del.Deleted != 1 || del.Skipped != 2 || len(del.Invalid) != 1 {
		t.Fatalf("удаление по номерам: %+v %v", del, err)
	}
	if n, err := items.DeleteAllFree(ctx, firstAdmin); err != nil || n != 0 {
		t.Fatalf("удалить все свободные: %d %v", n, err)
	}
	if st, _ := items.Stats(ctx); st.Total != 1 || st.Reserved != 1 {
		t.Fatalf("после удаления: %+v", st)
	}
}

// Жизненный цикл: выбор -> айтем пропадает из списка; отчёт -> куплен; отмена -> снова свободен.
func TestItemLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	items := NewItems(e.store)
	if _, err := items.Add(ctx, firstAdmin, "A;https://x.uz/1\nB;https://x.uz/2\nC;https://x.uz/3"); err != nil {
		t.Fatal(err)
	}
	sc := e.importScenario(t, scenarioYAML).Scenario
	t1 := e.assignOne(t, sc.ID, 1)
	t2 := e.assignOne(t, sc.ID, 2)

	// До принятия выбирать нельзя.
	if _, err := items.Choose(ctx, 1, t1, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("выбор до принятия: %v", err)
	}
	for _, p := range [][2]int64{{1, t1}, {2, t2}} {
		if _, _, err := e.tasks.Accept(ctx, p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	// Отчёт без выбранного айтема не принимается.
	ans := SubmitInput{Answers: []domain.Answer{{Key: "q_one", Value: "ок"}}}
	if _, err := e.reports().Submit(ctx, 1, t1, ans); !errors.Is(err, ErrForbidden) {
		t.Fatalf("отчёт без айтема: %v", err)
	}
	// Чужое задание и несуществующий айтем.
	if _, err := items.Choose(ctx, 2, t1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("чужое задание: %v", err)
	}
	if _, err := items.Choose(ctx, 1, t1, 999); !errors.Is(err, ErrForbidden) {
		t.Fatalf("нет такого айтема: %v", err)
	}

	it, err := items.Choose(ctx, 1, t1, 1)
	if err != nil || it.Status != domain.ItemReserved || it.TaskID != t1 {
		t.Fatalf("выбор: %+v %v", it, err)
	}
	if _, err := items.Choose(ctx, 1, t1, 2); !errors.Is(err, ErrForbidden) {
		t.Fatalf("второй айтем на то же задание: %v", err)
	}
	if _, err := items.Choose(ctx, 2, t2, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("уже выбранный айтем другим покупателем: %v", err)
	}
	free, total, _ := items.ListFree(ctx, 10, 0)
	if total != 2 || len(free) != 2 || free[0].ID == 1 {
		t.Fatalf("выбранный айтем должен пропасть из списка: %+v", free)
	}
	card, _ := e.tasks.Card(ctx, t1)
	if card.Item == nil || card.Item.ID != 1 {
		t.Fatalf("карточка задания: %+v", card.Item)
	}

	// Отчёт: айтем становится купленным (и остаётся таким при повторной отправке после доработки).
	if _, err := e.reports().Submit(ctx, 1, t1, ans); err != nil {
		t.Fatal(err)
	}
	if st, _ := items.Stats(ctx); st.Used != 1 || st.Reserved != 0 || st.Free != 2 {
		t.Fatalf("после отчёта: %+v", st)
	}
	if _, _, err := e.reports().ReturnForRework(ctx, firstAdmin, t1, "исправьте"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reports().Submit(ctx, 1, t1, ans); err != nil {
		t.Fatalf("повторная отправка после доработки: %v", err)
	}
	if st, _ := items.Stats(ctx); st.Used != 1 {
		t.Fatalf("повторная отправка не меняет купленное: %+v", st)
	}

	// Отмена задания возвращает выбранный айтем в список свободных.
	if _, err := items.Choose(ctx, 2, t2, 2); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.tasks.Cancel(ctx, firstAdmin, t2); err != nil || !ok {
		t.Fatalf("отмена: %v %v", ok, err)
	}
	if st, _ := items.Stats(ctx); st.Free != 2 || st.Reserved != 0 || st.Used != 1 {
		t.Fatalf("после отмены: %+v", st)
	}
	if card, _ := e.tasks.Card(ctx, t2); card.Item != nil {
		t.Fatalf("у отменённого задания айтема быть не должно: %+v", card.Item)
	}
}

// Продавцу айтем не выбирается.
func TestSellerCannotChooseItem(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	items := NewItems(e.store)
	if _, err := items.Add(ctx, firstAdmin, "A;https://x.uz/1"); err != nil {
		t.Fatal(err)
	}
	sc := e.importScenario(t, sellerYAML).Scenario
	id := e.assignOne(t, sc.ID, 1)
	if _, _, err := e.tasks.Accept(ctx, 1, id); err != nil {
		t.Fatal(err)
	}
	if _, err := items.Choose(ctx, 1, id, 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("продавец выбирает айтем: %v", err)
	}
}
