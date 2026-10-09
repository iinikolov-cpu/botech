package service

import (
	"context"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

func parseCSV(t *testing.T, data []byte) [][]string {
	t.Helper()
	if !strings.HasPrefix(string(data), "\xef\xbb\xbf") {
		t.Fatal("в CSV нет BOM: Excel покажет кракозябры")
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\xef\xbb\xbf")))
	r.Comma = ';'
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("CSV не разбирается: %v", err)
	}
	return rows
}

func answersOf(rate, yn, note string) []domain.Answer {
	return []domain.Answer{
		{Key: "rate", Value: rate}, {Key: "yn", Value: yn},
		{Key: "photo", FileID: "F", FileUniqueID: "U"},
		{Key: "note", Value: note},
	}
}

// col возвращает индекс колонки по началу заголовка.
func col(t *testing.T, head []string, prefix string) int {
	t.Helper()
	for i, h := range head {
		if strings.HasPrefix(h, prefix) {
			return i
		}
	}
	t.Fatalf("нет колонки %q в %v", prefix, head)
	return -1
}

func TestAnalyticsStatsAndCSV(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	buyerSc := e.importScenario(t, reportScenario).Scenario
	sellerSc := e.importScenario(t, sellerYAML).Scenario
	an := NewAnalytics(e.store, uzt)

	// Покупатели: #1 и #2 выполнили (оценки 5 и 3), #3 отказ, #4 ждёт ответа.
	for i, tc := range []struct{ rate, yn string }{{"5", "yes"}, {"3", "no"}} {
		id := e.acceptedTask(t, buyerSc.ID, int64(i+1))
		note := "ок"
		if i == 1 {
			note = "=HYPERLINK(\"evil\")" // защита от формул
		}
		if _, err := e.reports().Submit(ctx, int64(i+1), id, SubmitInput{
			Answers: answersOf(tc.rate, tc.yn, note),
			Comp:    &CompInput{Amount: 100000, ReceiptFileID: "R" + tc.rate, ReceiptUniqueID: "RU" + tc.rate},
		}); err != nil {
			t.Fatal(err)
		}
	}
	declined := e.assignOne(t, buyerSc.ID, 3)
	if _, _, err := e.tasks.Decline(ctx, 3, declined); err != nil {
		t.Fatal(err)
	}
	e.assignOne(t, buyerSc.ID, 3) // ждёт ответа
	// Продавец: одно выполненное задание без компенсации.
	sid := e.assignOne(t, sellerSc.ID, 1)
	if _, _, err := e.tasks.Accept(ctx, 1, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reports().Submit(ctx, 1, sid, SubmitInput{Answers: []domain.Answer{{Key: "q_one", Value: "всё хорошо"}}}); err != nil {
		t.Fatal(err)
	}

	st, err := an.Stats(ctx, storage.ExportFilter{}, domain.KindBuyer)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4 || st.Issued != 4 || st.Accepted != 2 || st.Declined != 1 || st.Waiting != 1 || st.Completed != 2 {
		t.Fatalf("сводка покупателей: %+v", st)
	}
	if st.CompPending != 2 || st.SumPending != 200000 || st.ItemsChosen != 2 || st.ItemsBought != 2 {
		t.Fatalf("компенсации и айтемы покупателей: %+v", st)
	}
	if len(st.Operators) != 1 || st.Operators[0].Operator != "BTS" || st.Operators[0].Completed != 2 {
		t.Fatalf("операторы: %+v", st.Operators)
	}
	sl, _ := an.Stats(ctx, storage.ExportFilter{}, domain.KindSeller)
	if sl.Total != 1 || sl.Completed != 1 || sl.CompPending != 0 || sl.ItemsChosen != 0 || sl.Kind != domain.KindSeller {
		t.Fatalf("сводка продавцов не должна смешиваться с покупателями: %+v", sl)
	}
	// Фильтр по периоду: в будущем заданий нет.
	if st, _ = an.Stats(ctx, storage.ExportFilter{From: time.Now().Add(time.Hour)}, domain.KindBuyer); st.Total != 0 {
		t.Fatalf("период в будущем: %+v", st)
	}

	rows, _ := an.Rows(ctx, storage.ExportFilter{})
	// CSV по заданиям: заголовок + 5 строк, есть роль и Telegram ID.
	tasks := parseCSV(t, an.TasksCSV(rows))
	if len(tasks) != 6 || tasks[0][0] != "№ задания" || tasks[0][1] != "Роль" || tasks[0][2] != "Telegram ID" {
		t.Fatalf("CSV заданий: %v", tasks[0])
	}

	// Отчёты: сразу все сценарии, роль и Telegram ID, колонки вопросов каждого сценария.
	data, err := an.AnswersCSV(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	ans := parseCSV(t, data)
	if len(ans) != 4 { // заголовок и три отчёта (два покупателя, один продавец)
		t.Fatalf("строк в отчётах: %d", len(ans))
	}
	head := ans[0]
	role, tg := col(t, head, "Роль"), col(t, head, "Telegram ID")
	cRate, cOne := col(t, head, "report-test / rate: Оценка"), col(t, head, "seller-test / q_one: Вопрос")
	roles := map[string]int{}
	for _, r := range ans[1:] {
		roles[r[role]]++
		if r[tg] == "" {
			t.Fatalf("нет Telegram ID: %v", r)
		}
		switch r[role] {
		case "покупатель":
			if r[cRate] == "" || r[cOne] != "" {
				t.Fatalf("колонки покупателя: %v", r)
			}
		case "продавец":
			if r[cOne] != "всё хорошо" || r[cRate] != "" {
				t.Fatalf("колонки продавца: %v", r)
			}
		}
	}
	if roles["покупатель"] != 2 || roles["продавец"] != 1 {
		t.Fatalf("роли в выгрузке: %v", roles)
	}
	all := string(data)
	if strings.Contains(all, ";=HYPERLINK") || !strings.Contains(all, "'=HYPERLINK") {
		t.Fatalf("формула не экранирована: %s", all)
	}
	if !strings.Contains(all, "Товар ") || !strings.Contains(all, "https://shop.uz/") {
		t.Fatalf("в выгрузке нет айтема покупателя: %s", all)
	}
	// Без имени бота вместо ссылки отметка, с именем: ссылка на бота (с токеном ссылку делать нельзя).
	if !strings.Contains(all, "фото приложено") || strings.Contains(all, "t.me") {
		t.Fatalf("медиа без имени бота: %s", all)
	}
	an.SetBotUsername("@my_bot")
	data, _ = an.AnswersCSV(ctx, rows)
	repID := rows[len(rows)-1].ReportID // самое раннее задание (#1), отчёт есть
	want := fmt.Sprintf("https://t.me/my_bot?start=m%dxphoto", repID)
	if !strings.Contains(string(data), want) || strings.Contains(string(data), "фото приложено") {
		t.Fatalf("ссылка на медиа %q не найдена: %s", want, data)
	}

	// Компенсации и журнал.
	comps := parseCSV(t, an.CompsCSV(rows))
	if len(comps) != 3 || comps[0][1] != "Telegram ID" {
		t.Fatalf("CSV компенсаций: %v", comps)
	}
	if _, err := an.AuditCSV(ctx, 100); err != nil {
		t.Fatal(err)
	}
}

func TestMediaLink(t *testing.T) {
	if got := MediaLink("bot", 12, "photo_1"); got != "https://t.me/bot?start=m12xphoto_1" {
		t.Fatalf("ссылка: %q", got)
	}
	if MediaLink("", 12, "photo") != "" {
		t.Fatal("без имени бота ссылки нет")
	}
}
