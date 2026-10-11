package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"botech/internal/domain"
	"botech/internal/storage"
)

const onePhotoScenario = `key: one-photo
title: Одно фото
operator: BTS
steps: [Шаг]
questions:
  - {key: pvz, text: Фото ПВЗ, type: photo}
  - {key: note, text: Комментарий, type: text, required: false}
`

// startPhotoReport принимает задание и открывает отчёт: ждём фото на первый вопрос.
func (e *testEnv) startPhotoReport(t *testing.T) {
	t.Helper()
	e.addBuyer(t, 2000, "Алия")
	e.upload(testAdmin, "one.yaml", onePhotoScenario)
	e.assignTo(2000, 1)
	e.click(2000, "tsk:ac:1")
	e.ensureItem(t, 1)
	e.click(2000, "tsk:rp:1")
}

// Несколько фото на один вопрос: все сохраняются в одном ответе и видны админу.
func TestMultiplePhotosOneAnswer(t *testing.T) {
	e := newTestEnv(t)
	e.startPhotoReport(t)

	e.sendPhoto(2000, "A1")
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Принято файлов: <b>1</b>") || !strings.Contains(m.Markup, "rpt:n:") || strings.Contains(m.Markup, "rpt:s:") {
		t.Fatalf("после первого фото: %+v", m)
	}
	e.sendPhoto(2000, "A2")
	e.sendPhoto(2000, "A3")
	e.sendPhoto(2000, "A2") // повтор того же файла не дублируется
	if m, _ = e.tg.lastTo(2000); !strings.Contains(m.Text, "Принято файлов: <b>3</b>") {
		t.Fatalf("счётчик: %q", m.Text)
	}
	e.press(t, 2000, "rpt:n:", "")
	if m, _ = e.tg.lastTo(2000); !strings.Contains(m.Text, "Вопрос 2 из 2") {
		t.Fatalf("после «Дальше» следующий вопрос: %q", m.Text)
	}
	e.press(t, 2000, "rpt:s:", "") // комментарий пропускаем
	e.press(t, 2000, "rpt:ca:", ":0")
	if m, _ = e.tg.lastTo(2000); !strings.Contains(m.Text, "файлов: 3") {
		t.Fatalf("итог должен показывать три файла: %q", m.Text)
	}
	e.press(t, 2000, "rpt:ok:", "")

	card, err := e.app.tasks.Card(context.Background(), 1)
	if err != nil || card.Report == nil {
		t.Fatalf("отчёт не сохранён: %v", err)
	}
	var an domain.Answer
	for _, a := range card.Report.Answers {
		if a.Key == "pvz" {
			an = a
		}
	}
	files := an.AllFiles()
	if len(files) != 3 || files[0].ID != "A1" || files[2].ID != "A3" || an.FileID != "A1" {
		t.Fatalf("сохранённые файлы: %+v", an)
	}

	// Админ видит все три фото одним альбомом, подписи с номерами, кнопки последним сообщением.
	before := len(e.tg.methodsTo(testAdmin))
	e.click(testAdmin, "adm:rv:1")
	ms := e.tg.methodsTo(testAdmin)[before:]
	if len(ms) != 2 || ms[0] != "sendMediaGroup" || ms[1] != "sendMessage" {
		t.Fatalf("альбом, затем текст: %v", ms)
	}
	e.tg.mu.Lock()
	var group string
	for _, s := range e.tg.sent {
		if s.Method == "sendMediaGroup" && s.Chat == testAdmin {
			group = s.Text
		}
	}
	e.tg.mu.Unlock()
	for _, want := range []string{"A1", "A2", "A3", "(1/3)", "(3/3)"} {
		if !strings.Contains(group, want) {
			t.Fatalf("в альбоме нет %q: %s", want, group)
		}
	}

	// Ссылка из выгрузки открывает все файлы ответа.
	rows, _ := e.app.analytics.Rows(context.Background(), storage.ExportFilter{})
	link := fmt.Sprintf("m%dxpvz", rows[0].ReportID)
	before = len(e.tg.methodsTo(testAdmin))
	e.say(testAdmin, "/start "+link)
	if ms = e.tg.methodsTo(testAdmin)[before:]; len(ms) != 1 || ms[0] != "sendMediaGroup" {
		t.Fatalf("по ссылке должен прийти альбом из трёх файлов: %v", ms)
	}
}

// Альбом приходит несколькими сообщениями одновременно: ни одно фото не теряется.
func TestAlbumPhotosConcurrent(t *testing.T) {
	e := newTestEnv(t)
	e.startPhotoReport(t)
	var wg sync.WaitGroup
	for i := 1; i <= 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e.sendPhoto(2000, fmt.Sprintf("C%d", i))
		}(i)
	}
	wg.Wait()
	if m, _ := e.tg.lastTo(2000); !strings.Contains(m.Text, "Принято файлов: <b>6</b>") {
		t.Fatalf("при одновременной отправке потеряны фото: %q", m.Text)
	}
}

// «Назад» при приёме файлов сбрасывает их; максимум файлов на ответ; пропуск скрыт, пока есть файлы.
func TestPhotoCollectingBackAndLimit(t *testing.T) {
	e := newTestEnv(t)
	e.startPhotoReport(t)
	e.sendPhoto(2000, "B1")
	e.sendPhoto(2000, "B2")
	e.press(t, 2000, "rpt:b:", "")
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Вопрос 1 из 2") || strings.Contains(m.Text, "Принято файлов") {
		t.Fatalf("после «Назад» вопрос задаётся заново: %q", m.Text)
	}
	for i := 1; i <= domain.MaxFilesPerAnswer; i++ {
		e.sendPhoto(2000, fmt.Sprintf("L%d", i))
	}
	if m, _ = e.tg.lastTo(2000); !strings.Contains(m.Text, fmt.Sprintf("Принято файлов: <b>%d</b>", domain.MaxFilesPerAnswer)) {
		t.Fatalf("предел: %q", m.Text)
	}
	e.sendPhoto(2000, "LAST")
	if got := e.tg.last(); !strings.Contains(got, "Больше 10 файлов") {
		t.Fatalf("сверх предела: %q", got)
	}
	// «Дальше» работает один раз: старая кнопка после перехода устарела.
	next := ""
	for _, d := range dataRe.FindAllStringSubmatch(m.Markup, -1) {
		if strings.HasPrefix(d[1], "rpt:n:") {
			next = d[1]
		}
	}
	if next == "" {
		t.Fatalf("нет кнопки «Дальше»: %s", m.Markup)
	}
	e.click(2000, next)
	if m, _ = e.tg.lastTo(2000); !strings.Contains(m.Text, "Вопрос 2 из 2") {
		t.Fatalf("после «Дальше»: %q", m.Text)
	}
	e.click(2000, next)
	if got := e.tg.lastAnswer(); !strings.Contains(got, "устарела") {
		t.Fatalf("устаревшая кнопка: %q", got)
	}
}
