package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"botech/internal/domain"
)

const sellerScenario = `key: seller-flow
kind: seller
title: Продажа
operator: EMU
steps: [Шаг]
questions:
  - {key: rate, text: Оценка ПВЗ, type: rating}
  - {key: note, text: Комментарий, type: text, required: false}
`

// addUserKinds создаёт активного исполнителя с заданными типами заданий (пусто: ещё не выбирал).
func (e *testEnv) addUserKinds(t *testing.T, id int64, name string, kinds ...domain.TaskKind) {
	t.Helper()
	now := time.Now().UTC()
	if err := e.store.Repos().Users.Create(context.Background(), &domain.User{
		TgID: id, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru", FirstName: name, Kinds: kinds, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// Пока исполнитель не выбрал типы заданий, бот показывает только выбор; после выбора всё работает.
func TestKindsGateAndChoice(t *testing.T) {
	e := newTestEnv(t)
	e.addUserKinds(t, 2000, "Алия")

	e.say(2000, "/start")
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Какие задания вы готовы выполнять") || !strings.Contains(m.Markup, "kd:s:1") || !strings.Contains(m.Markup, "kd:s:2") {
		t.Fatalf("вместо меню должен быть выбор типов: %+v", m)
	}
	if strings.Contains(m.Markup, "kd:ok:") {
		t.Fatal("кнопки «Готово» без выбора быть не должно")
	}
	// Любые кнопки старых сообщений тоже ведут на выбор.
	e.click(2000, "tsk:l")
	if m, _ = e.tg.lastTo(2000); !strings.Contains(m.Text, "Какие задания вы готовы выполнять") {
		t.Fatalf("старые кнопки должны вести на выбор: %q", m.Text)
	}

	e.click(2000, "kd:s:2") // отметили «продавец»
	m, _ = e.tg.lastTo(2000)
	if !strings.Contains(m.Markup, "☑ Продавец") || !strings.Contains(m.Markup, "kd:ok:2") || !strings.Contains(m.Markup, "kd:s:3") {
		t.Fatalf("экран с отметкой продавца: %s", m.Markup)
	}
	e.click(2000, "kd:ok:0")
	if got := e.tg.lastAnswer(); !strings.Contains(got, "хотя бы один") {
		t.Fatalf("пустой выбор: %q", got)
	}
	e.click(2000, "kd:ok:3") // оба типа
	if got := e.tg.lastAnswer(); got != "Сохранено" {
		t.Fatalf("сохранение: %q", got)
	}
	u, _ := e.app.access.Lookup(context.Background(), 2000)
	if !u.CanDo(domain.KindBuyer) || !u.CanDo(domain.KindSeller) {
		t.Fatalf("сохранённые типы: %v", u.Kinds)
	}
	e.say(2000, "/start")
	if m, _ = e.tg.lastTo(2000); !strings.Contains(m.Text, "Здравствуйте") || !strings.Contains(m.Markup, "kd:s:3") {
		t.Fatalf("после выбора обычное меню с кнопкой смены типов: %+v", m)
	}
	// Изменить выбор можно в любой момент: оставили только покупателя.
	e.click(2000, "kd:ok:1")
	if u, _ = e.app.access.Lookup(context.Background(), 2000); u.CanDo(domain.KindSeller) || !u.CanDo(domain.KindBuyer) {
		t.Fatalf("смена типов: %v", u.Kinds)
	}
	// У админа выбора нет: панель открывается сразу.
	e.say(testAdmin, "/admin")
	if got := e.tg.last(); !strings.Contains(got, "Админ-панель") {
		t.Fatalf("админ: %q", got)
	}
}

// Мастер назначения предлагает только тех, кто выбрал тип сценария; админ видит тип у пользователя.
func TestAssignWizardFiltersByKind(t *testing.T) {
	e := newTestEnv(t)
	e.addUserKinds(t, 2000, "Селлер", domain.KindSeller)
	e.addUserKinds(t, 2001, "Бай", domain.KindBuyer)
	e.addUserKinds(t, 2002, "Оба", domain.AllKinds...)
	e.addUserKinds(t, 2003, "Никто")
	e.upload(testAdmin, "buyer.yaml", reportFlowScenario) // сценарий 1, покупатель
	e.upload(testAdmin, "seller.yaml", sellerScenario)    // сценарий 2, продавец

	e.click(testAdmin, "adm:sc")
	if m, _ := e.tg.lastTo(testAdmin); !strings.Contains(m.Markup, "продавец") || !strings.Contains(m.Markup, "покупатель") {
		t.Fatalf("в списке сценариев нет типов: %s", m.Markup)
	}

	e.click(testAdmin, "adm:as:0")
	e.click(testAdmin, "adm:as:s:2")
	e.click(testAdmin, "adm:as:d:3")
	m, _ := e.tg.lastTo(testAdmin)
	for _, want := range []string{"adm:as:t:2000:", "adm:as:t:2002:"} {
		if !strings.Contains(m.Markup, want) {
			t.Fatalf("в списке для продавца нет %s: %s", want, m.Markup)
		}
	}
	for _, bad := range []string{"adm:as:t:2001:", "adm:as:t:2003:"} {
		if strings.Contains(m.Markup, bad) {
			t.Fatalf("в списке для продавца не должно быть %s: %s", bad, m.Markup)
		}
	}
	// «Выбрать всех» тоже только подходящих.
	e.click(testAdmin, "adm:as:all")
	e.click(testAdmin, "adm:as:go")
	if !e.tg.anyTo(2000, "Вам новое задание") || !e.tg.anyTo(2002, "Вам новое задание") {
		t.Fatal("подходящие пользователи должны получить задание")
	}
	if e.tg.anyTo(2001, "Вам новое задание") || e.tg.anyTo(2003, "Вам новое задание") {
		t.Fatal("неподходящие пользователи не должны получать задание")
	}

	// В покупательском сценарии наоборот.
	e.click(testAdmin, "adm:as:0")
	e.click(testAdmin, "adm:as:s:1")
	e.click(testAdmin, "adm:as:d:3")
	m, _ = e.tg.lastTo(testAdmin)
	if !strings.Contains(m.Markup, "adm:as:t:2001:") || strings.Contains(m.Markup, "adm:as:t:2000:") {
		t.Fatalf("список для покупательского сценария: %s", m.Markup)
	}
	// Карточка пользователя показывает типы.
	e.click(testAdmin, "adm:uc:2003")
	if got := e.tg.last(); !strings.Contains(got, "Типы заданий: не выбраны") {
		t.Fatalf("карточка пользователя: %q", got)
	}
	e.click(testAdmin, "adm:uc:2002")
	if got := e.tg.last(); !strings.Contains(got, "Типы заданий: покупатель, продавец") {
		t.Fatalf("карточка пользователя: %q", got)
	}
}

// Сценарий продавца целиком: без промокода и предупреждений о пуле, отчёт без шагов компенсации.
func TestSellerFlowHasNoPromoOrCompensation(t *testing.T) {
	e := newTestEnv(t)
	e.addUserKinds(t, 2000, "Селлер", domain.KindSeller)
	e.upload(testAdmin, "seller.yaml", sellerScenario)
	e.assignTo(2000, 1)

	e.click(2000, "tsk:ac:1")
	card := e.tg.last()
	if strings.Contains(card, "ромокод") {
		t.Fatalf("у продавца нет промокода: %q", card)
	}
	if m, _ := e.tg.lastTo(2000); strings.Contains(m.Markup, "tsk:pc:") || !strings.Contains(m.Markup, "tsk:rp:1") {
		t.Fatalf("кнопки продавца: %s", m.Markup)
	}
	if e.tg.anyTo(testAdmin, "ромокод") {
		t.Fatal("админа не нужно предупреждать о промокодах для продавца")
	}

	e.click(2000, "tsk:rp:1")
	e.press(t, 2000, "rpt:r:", ":5")
	e.press(t, 2000, "rpt:s:", "") // необязательный комментарий пропускаем
	m, _ := e.tg.lastTo(2000)
	if !strings.Contains(m.Text, "Проверьте отчёт") || strings.Contains(m.Text, "омпенсац") {
		t.Fatalf("после вопросов сразу итог без компенсации: %q", m.Text)
	}
	e.press(t, 2000, "rpt:ok:", "")
	if !e.tg.anyTo(testAdmin, "Получен отчёт") || e.tg.anyTo(testAdmin, "омпенсац") {
		t.Fatal("админ получает отчёт без строки о компенсации")
	}
	e.click(testAdmin, "adm:tc:1")
	if got := e.tg.last(); !strings.Contains(got, "тип: продавец") || strings.Contains(got, "Промокод") || strings.Contains(got, "Компенсация") {
		t.Fatalf("карточка задания продавца: %q", got)
	}
	if m, _ := e.tg.lastTo(testAdmin); strings.Contains(m.Markup, "adm:cc:") {
		t.Fatalf("у продавца нет кнопки компенсации: %s", m.Markup)
	}
	e.click(testAdmin, "adm:cp:w:0")
	if m, _ := e.tg.lastTo(testAdmin); strings.Contains(m.Markup, "adm:cc:") {
		t.Fatalf("компенсаций у продавца быть не должно: %s", m.Markup)
	}
}
