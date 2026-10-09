package handlers

import (
	"strings"
	"testing"
)

// Админ загружает айтемы сообщением и файлом, смотрит таблицу и удаляет.
func TestItemsAdminFlow(t *testing.T) {
	e := newTestEnv(t)
	e.click(testAdmin, "adm:home")
	if m, _ := e.tg.lastTo(testAdmin); !strings.Contains(m.Markup, "adm:it") {
		t.Fatalf("нет кнопки «Айтемы» на главной: %s", m.Markup)
	}
	e.click(testAdmin, "adm:it")
	if got := e.tg.last(); !strings.Contains(got, "Свободно: <b>0</b>") || !strings.Contains(got, "⚠") {
		t.Fatalf("пустой пул: %q", got)
	}

	e.click(testAdmin, "adm:ita")
	if got := e.tg.last(); !strings.Contains(got, "Название; ссылка; цена; заметка") {
		t.Fatalf("подсказка по формату: %q", got)
	}
	e.say(testAdmin, "Куртка; https://olx.uz/a1; 350000; размер L\nШапка;https://olx.uz/a2\nмусор")
	if got := e.tg.last(); !strings.Contains(got, "Добавлено айтемов: <b>2</b>") || !strings.Contains(got, "Не принято (1)") || !strings.Contains(got, "мусор") {
		t.Fatalf("итог загрузки: %q", got)
	}
	// Файл идёт в айтемы только когда открыт раздел загрузки айтемов.
	e.click(testAdmin, "adm:ita")
	e.upload(testAdmin, "items.csv", "Название;Ссылка;Цена\nЛампа;https://olx.uz/a3;1 500\nШапка;https://olx.uz/a2")
	if got := e.tg.last(); !strings.Contains(got, "Добавлено айтемов: <b>1</b>") || !strings.Contains(got, "Пропущено (такая ссылка уже есть): 1") {
		t.Fatalf("загрузка файлом: %q", got)
	}

	e.click(testAdmin, "adm:itl:0")
	got := e.tg.last()
	for _, want := range []string{"<pre>", "Куртка", "350 000", "Лампа", "свободен"} {
		if !strings.Contains(got, want) {
			t.Fatalf("в таблице нет %q: %q", want, got)
		}
	}
	e.click(testAdmin, "adm:itd")
	e.click(testAdmin, "adm:itdl")
	e.say(testAdmin, "2, 99")
	if got := e.tg.last(); !strings.Contains(got, "Удалено айтемов: <b>1</b>") || !strings.Contains(got, "Не удалено") {
		t.Fatalf("удаление по номерам: %q", got)
	}
	e.click(testAdmin, "adm:itda")
	if got := e.tg.last(); !strings.Contains(got, "(2 шт.)") {
		t.Fatalf("подтверждение удаления всех: %q", got)
	}
	e.click(testAdmin, "adm:itdy")
	if got := e.tg.last(); !strings.Contains(got, "Всего: 0") {
		t.Fatalf("после удаления: %q", got)
	}
	// Покупателю раздел недоступен.
	e.addBuyer(t, 2000, "Алия")
	e.click(2000, "adm:it")
	if got := e.tg.lastAnswer(); got != "Нет доступа." {
		t.Fatalf("раздел айтемов у покупателя: %q", got)
	}
}

// Покупатель сам выбирает товар; без выбора отчёт недоступен; выбранный пропадает из списка других.
func TestBuyerChoosesItem(t *testing.T) {
	e := newTestEnv(t)
	e.addBuyer(t, 2000, "Алия")
	e.addBuyer(t, 2001, "Борис")
	e.upload(testAdmin, "s.yaml", reportFlowScenario)
	e.assignTo(2000, 1)
	e.assignTo(2001, 1)

	// Принятие при пустом пуле: нужен выбор товара, отчёта нет, админу предупреждение.
	e.click(2000, "tsk:ac:1")
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Выберите товар") || !strings.Contains(m.Markup, "tsk:it:1") || strings.Contains(m.Markup, "tsk:rp:1") {
		t.Fatalf("карточка без выбранного товара: %+v", m)
	}
	if !e.tg.anyTo(testAdmin, "Нет свободных айтемов") {
		t.Fatal("админ не получил предупреждение о пустом пуле айтемов")
	}
	e.click(2000, "tsk:rp:1")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "выберите товар") {
		t.Fatalf("отчёт без товара: %q", got)
	}
	e.click(2000, "tsk:it:1")
	if got := e.tg.last(); !strings.Contains(got, "Свободных товаров пока нет") {
		t.Fatalf("пустой список для покупателя: %q", got)
	}

	// Админ загружает товары.
	e.click(testAdmin, "adm:ita")
	e.say(testAdmin, "Куртка;https://olx.uz/a1;350000;размер L\nШапка;https://olx.uz/a2")

	e.click(2000, "tsk:it:1")
	m, _ = e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Свободных: 2") || !strings.Contains(m.Markup, "tsk:ii:1:1") || !strings.Contains(m.Markup, "tsk:ii:1:2") || !strings.Contains(m.Markup, "Куртка · 350 000") {
		t.Fatalf("список товаров: %+v", m)
	}
	e.click(2000, "tsk:ii:1:1")
	m, _ = e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Куртка") || !strings.Contains(m.Text, "https://olx.uz/a1") || !strings.Contains(m.Text, "размер L") || !strings.Contains(m.Markup, "tsk:ip:1:1") {
		t.Fatalf("карточка товара: %+v", m)
	}
	e.click(2000, "tsk:ip:1:1")
	if got := e.tg.lastAnswer(); got != "Товар выбран." {
		t.Fatalf("выбор: %q", got)
	}
	m, _ = e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Товар для покупки: <b>Куртка</b>") || !strings.Contains(m.Text, "https://olx.uz/a1") ||
		!strings.Contains(m.Markup, "tsk:rp:1") || strings.Contains(m.Markup, "tsk:it:1") {
		t.Fatalf("карточка после выбора: %+v", m)
	}

	// Второй покупатель видит только оставшийся товар; выбранный нельзя взять ни по списку, ни по старой кнопке.
	e.click(2001, "tsk:ac:2")
	e.click(2001, "tsk:it:2")
	m, _ = e.tg.lastTo(2001)
	if !strings.Contains(m.Text, "Свободных: 1") || strings.Contains(m.Markup, "tsk:ii:2:1") || !strings.Contains(m.Markup, "tsk:ii:2:2") {
		t.Fatalf("список второго покупателя: %+v", m)
	}
	e.click(2001, "tsk:ip:2:1")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "уже выбрал") {
		t.Fatalf("занятый товар: %q", got)
	}

	// Отчёт: товар становится купленным; в таблице админа видно, кем и под какое задание.
	e.completeReportWithComp(t, 2000, 1, "350000")
	e.click(testAdmin, "adm:itl:0")
	if got := e.tg.last(); !strings.Contains(got, "куплен, #1") {
		t.Fatalf("таблица после покупки: %q", got)
	}
	// Админ отменяет второе задание: выбранный там товар (если был) вернулся бы в оборот.
	e.click(2001, "tsk:ip:2:2")
	e.click(testAdmin, "adm:tcy:2")
	e.click(testAdmin, "adm:itl:0")
	if got := e.tg.last(); !strings.Contains(got, "свободен") || strings.Contains(got, "выбран, #2") {
		t.Fatalf("после отмены товар должен вернуться: %q", got)
	}
}
