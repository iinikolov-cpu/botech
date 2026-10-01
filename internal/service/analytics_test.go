package service

import (
	"context"
	"encoding/csv"
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

func TestAnalyticsStatsAndCSV(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, reportScenario).Scenario
	an := NewAnalytics(e.store, uzt)

	// #1 и #2 выполнены (оценки 5 и 3, «да» и «нет»), #3 отказ, #4 ждёт ответа.
	for i, tc := range []struct{ rate, yn string }{{"5", "yes"}, {"3", "no"}} {
		id := e.acceptedTask(t, sc.ID, int64(i+1))
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
	declined := e.assignOne(t, sc.ID, 3)
	if _, _, err := e.tasks.Decline(ctx, 3, declined); err != nil {
		t.Fatal(err)
	}
	e.assignOne(t, sc.ID, 3) // ждёт ответа

	st, err := an.Stats(ctx, storage.ExportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4 || st.Issued != 4 || st.Accepted != 2 || st.Declined != 1 || st.Waiting != 1 || st.Completed != 2 {
		t.Fatalf("сводка: %+v", st)
	}
	if st.CompPending != 2 || st.SumPending != 200000 {
		t.Fatalf("компенсации: %+v", st)
	}
	if len(st.Operators) != 1 || st.Operators[0].Operator != "BTS" || st.Operators[0].Completed != 2 {
		t.Fatalf("операторы: %+v", st.Operators)
	}
	var rate, yn *QuestionStat
	for i := range st.Operators[0].Questions {
		q := &st.Operators[0].Questions[i]
		switch q.Key {
		case "rate":
			rate = q
		case "yn":
			yn = q
		}
	}
	if rate == nil || rate.Avg != 4 || rate.N != 2 || rate.Text != "Оценка" {
		t.Fatalf("средняя оценка: %+v", rate)
	}
	if yn == nil || yn.YesPc != 50 {
		t.Fatalf("доля «да»: %+v", yn)
	}

	// Фильтр по периоду: в будущем заданий нет.
	st, _ = an.Stats(ctx, storage.ExportFilter{From: time.Now().Add(time.Hour)})
	if st.Total != 0 {
		t.Fatalf("период в будущем: %+v", st)
	}

	rows, _ := an.Rows(ctx, storage.ExportFilter{})
	// CSV по заданиям: заголовок + 4 строки.
	tasks := parseCSV(t, an.TasksCSV(rows))
	if len(tasks) != 5 || tasks[0][0] != "№ задания" {
		t.Fatalf("CSV заданий: %v", tasks)
	}
	// Ответы: колонки по вопросам, медиа без файла, формулы экранированы.
	data, err := an.AnswersCSV(ctx, sc.ID, rows)
	if err != nil {
		t.Fatal(err)
	}
	ans := parseCSV(t, data)
	if len(ans) != 3 { // заголовок и два отчёта
		t.Fatalf("строк в ответах: %d", len(ans))
	}
	head := strings.Join(ans[0], "|")
	if !strings.Contains(head, "rate: Оценка") || !strings.Contains(head, "photo: Фото") {
		t.Fatalf("заголовок: %s", head)
	}
	all := string(data)
	if !strings.Contains(all, "фото приложено") || strings.Contains(all, "F;") {
		t.Fatalf("медиа в CSV: %s", all)
	}
	if strings.Contains(all, ";=HYPERLINK") || !strings.Contains(all, "'=HYPERLINK") {
		t.Fatalf("формула не экранирована: %s", all)
	}
	// Компенсации и журнал.
	if comps := parseCSV(t, an.CompsCSV(rows)); len(comps) != 3 {
		t.Fatalf("CSV компенсаций: %v", comps)
	}
	if _, err := an.AuditCSV(ctx, 100); err != nil {
		t.Fatal(err)
	}
}
