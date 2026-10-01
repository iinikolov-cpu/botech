package handlers

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Команда /admin дважды: старое меню и сама команда удаляются, в чате остаётся одно меню.
func TestPanelReplacesPreviousMenu(t *testing.T) {
	e := newTestEnv(t)
	e.say(testAdmin, "/admin")
	if got := e.tg.deletedCount(); got != 1 { // удалена только сама команда
		t.Fatalf("после первой команды удалений: %d", got)
	}
	e.say(testAdmin, "/admin")
	if got := e.tg.deletedCount(); got != 3 { // команда, предыдущая панель и снова команда
		t.Fatalf("после второй команды удалений: %d", got)
	}
}

// Ответ в диалоге (загрузка промокодов) не оставляет в чате ни ввод, ни старый экран:
// результат заменяет запрос.
func TestPanelDialogResultReplacesPrompt(t *testing.T) {
	e := newTestEnv(t)
	e.click(testAdmin, "adm:pr")
	e.click(testAdmin, "adm:pra")
	before := e.tg.deletedCount()
	e.say(testAdmin, "CODE001\nCODE002")
	if got := e.tg.last(); !strings.Contains(got, "Добавлено кодов: <b>2</b>") {
		t.Fatalf("итог загрузки: %q", got)
	}
	if got := e.tg.deletedCount() - before; got != 2 { // сообщение с кодами и экран-запрос
		t.Fatalf("удалено сообщений: %d, ожидали 2", got)
	}
}

// Предупреждение о промокодах всегда одно: новое заменяет прежнее, а не копится.
func TestPromoWarningNotDuplicated(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.addBuyer(t, 2001, "Боря")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	e.assignTo(2001, 1)

	e.click(2000, "tsk:ac:1")
	if e.tg.count(testAdmin, "Не осталось ни одного промокода") != 1 {
		t.Fatal("нет предупреждения о пустом пуле")
	}
	before := e.tg.deletedCount()
	e.click(2001, "tsk:ac:2")
	if e.tg.count(testAdmin, "Не осталось ни одного промокода") != 2 {
		t.Fatal("второе предупреждение не отправлено")
	}
	if e.tg.deletedCount()-before != 1 {
		t.Fatalf("прежнее предупреждение должно быть удалено, удалений: %d", e.tg.deletedCount()-before)
	}
}

// Отметки в списке заданий: нет ответа, просрочено, давно на доработке.
func TestTaskListMarks(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.addBuyer(t, 2001, "Боря")
	e.addBuyer(t, 2002, "Вика")
	e.quietOff(t)
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1) // #1: останется без ответа
	e.assignTo(2001, 1) // #2: просрочим
	e.assignTo(2002, 1) // #3: вернём на доработку

	listMarkup := func() string {
		e.click(testAdmin, "adm:tk:a:0")
		m, _ := e.tg.lastTo(testAdmin)
		return m.Markup
	}
	if got := listMarkup(); strings.Contains(got, "⚠") {
		t.Fatalf("свежие задания без отметок: %s", got)
	}

	// #1: сутки без ответа.
	e.shift(t, 1, 25*time.Hour)
	if got := listMarkup(); !strings.Contains(got, "⚠ #1") || !strings.Contains(got, "нет ответа 1 дн. 1 ч") {
		t.Fatalf("нет отметки «нет ответа»: %s", got)
	}

	// #2: принято и просрочено.
	e.click(2001, "tsk:ac:2")
	e.shift(t, 2, 75*time.Hour)
	e.runReminders(t)
	if got := listMarkup(); !strings.Contains(got, "⚠ #2") || !strings.Contains(got, "просрочено на") {
		t.Fatalf("нет отметки «просрочено»: %s", got)
	}

	// #3: на доработке меньше суток отметки нет, больше суток есть.
	e.click(2002, "tsk:ac:3")
	e.completeReportWithComp(t, 2002, 3, "10000")
	e.click(testAdmin, "adm:rw:3")
	e.say(testAdmin, "переснимите")
	if got := listMarkup(); strings.Contains(got, "⚠ #3") {
		t.Fatalf("свежий возврат не должен отмечаться: %s", got)
	}
	if _, err := e.store.DB().ExecContext(context.Background(),
		`UPDATE reports SET decided_at = decided_at - ?1 WHERE task_id = 3`, int64((26 * time.Hour).Seconds())); err != nil {
		t.Fatal(err)
	}
	if got := listMarkup(); !strings.Contains(got, "⚠ #3") || !strings.Contains(got, "на доработке 1 дн. 2 ч") {
		t.Fatalf("нет отметки «на доработке»: %s", got)
	}
	// Карточка задания тоже показывает причину.
	e.click(testAdmin, "adm:tc:3")
	if got := e.tg.last(); !strings.Contains(got, "⚠ на доработке") {
		t.Fatalf("карточка: %q", got)
	}
}
