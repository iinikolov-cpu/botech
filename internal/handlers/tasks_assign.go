package handlers

import (
	"botech/internal/domain"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// assignState состояние мастера назначения (хранится в БД между нажатиями).
type assignState struct {
	ScenarioID int64   `json:"scenario_id"`
	Days       int     `json:"days"`
	Selected   []int64 `json:"selected"`
}

const assignDialog = "assign"

var dueChoices = []int{1, 2, 3, 5, 7, 10, 14}

// assignCallback мастер назначения: adm:as:0 | s:<id> | d:<дни> | p:<стр> | t:<uid>:<стр> | all | none | go | x.
func (a *App) assignCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	arg := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	var st assignState
	if name, err := a.dialog.Get(ctx, admin.TgID, &st); err != nil || name != assignDialog {
		st = assignState{}
	}
	save := func() {
		if err := a.dialog.Set(ctx, admin.TgID, assignDialog, st); err != nil {
			a.log.Error("сохранение состояния мастера", "err", err)
		}
	}
	page, _ := strconv.Atoi(arg(3))

	switch arg(2) {
	case "x":
		_ = a.dialog.Clear(ctx, admin.TgID)
		t, m := a.screenHome(ctx)
		return t, m, "Отменено"
	case "s":
		id, _ := strconv.ParseInt(arg(3), 10, 64)
		st = assignState{ScenarioID: id}
		save()
		t, m := a.screenAssignDays(ctx, st)
		return t, m, ""
	case "d":
		days, _ := strconv.Atoi(arg(3))
		if st.ScenarioID == 0 {
			t, m := a.screenAssignScenarios(ctx)
			return t, m, ""
		}
		st.Days = days
		save()
		t, m := a.screenAssignBuyers(ctx, st, 0)
		return t, m, ""
	case "p":
		t, m := a.screenAssignBuyers(ctx, st, page)
		return t, m, ""
	case "t":
		uid, _ := strconv.ParseInt(arg(3), 10, 64)
		p, _ := strconv.Atoi(arg(4))
		st.Selected = toggle(st.Selected, uid)
		save()
		t, m := a.screenAssignBuyers(ctx, st, p)
		return t, m, ""
	case "all":
		list, _, err := a.access.UsersForKind(ctx, a.scenarioKind(ctx, st.ScenarioID), 500, 0)
		if err != nil {
			a.log.Error("выбор всех покупателей", "err", err)
		}
		st.Selected = st.Selected[:0]
		for _, u := range list {
			st.Selected = append(st.Selected, u.TgID)
		}
		save()
		t, m := a.screenAssignBuyers(ctx, st, 0)
		return t, m, "Выбраны все активные"
	case "none":
		st.Selected = nil
		save()
		t, m := a.screenAssignBuyers(ctx, st, 0)
		return t, m, ""
	case "go":
		t, m, toast := a.assignExecute(ctx, b, admin, st)
		return t, m, toast
	}
	// adm:as:0 и любое неизвестное: начало
	_ = a.dialog.Clear(ctx, admin.TgID)
	t, m := a.screenAssignScenarios(ctx)
	return t, m, ""
}

func toggle(list []int64, id int64) []int64 {
	for i, v := range list {
		if v == id {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return append(list, id)
}

func (a *App) screenAssignScenarios(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	list, err := a.scenarios.List(ctx, false)
	if err != nil {
		a.log.Error("список сценариев для назначения", "err", err)
	}
	if len(list) == 0 {
		return "Нет сценариев. Сначала загрузите сценарий в разделе «Сценарии».", kb(row(btn("📋 Сценарии", "adm:sc"), btn("« Назад", "adm:home")))
	}
	var rows [][]models.InlineKeyboardButton
	for _, s := range list {
		rows = append(rows, row(btn(cut(fmt.Sprintf("%s · %s · %s", s.Title, s.Operator, s.Kind.Title()), 60), "adm:as:s:"+itoa(s.ID))))
	}
	rows = append(rows, row(btn("Отмена", "adm:as:x")))
	return "<b>Назначение задания</b>\nШаг 1 из 3: выберите сценарий.", kb(rows...)
}

func (a *App) screenAssignDays(ctx context.Context, st assignState) (string, *models.InlineKeyboardMarkup) {
	sc, _, err := a.scenarios.Get(ctx, st.ScenarioID)
	if err != nil {
		return a.screenAssignScenarios(ctx)
	}
	var btns []models.InlineKeyboardButton
	for _, d := range dueChoices {
		btns = append(btns, btn(fmt.Sprintf("%d дн.", d), fmt.Sprintf("adm:as:d:%d", d)))
	}
	rows := [][]models.InlineKeyboardButton{btns[:4], btns[4:], row(btn("« Другой сценарий", "adm:as:0"), btn("Отмена", "adm:as:x"))}
	return fmt.Sprintf("<b>Назначение задания</b>\nСценарий: %s\nШаг 2 из 3: сколько дней даётся на выполнение после принятия?", esc(sc.Title)), kb(rows...)
}

func (a *App) screenAssignBuyers(ctx context.Context, st assignState, page int) (string, *models.InlineKeyboardMarkup) {
	if page < 0 {
		page = 0
	}
	kind := a.scenarioKind(ctx, st.ScenarioID)
	list, total, err := a.access.UsersForKind(ctx, kind, pageSize, page*pageSize)
	if err != nil {
		a.log.Error("список покупателей для назначения", "err", err)
	}
	sel := map[int64]bool{}
	for _, id := range st.Selected {
		sel[id] = true
	}
	var rows [][]models.InlineKeyboardButton
	for _, u := range list {
		name := strings.TrimSpace(u.FirstName)
		if name == "" {
			name = itoa(u.TgID)
		}
		if u.Username != "" {
			name += " @" + u.Username
		}
		box := "☐ "
		if sel[u.TgID] {
			box = "☑ "
		}
		rows = append(rows, row(btn(cut(box+name, 60), fmt.Sprintf("adm:as:t:%d:%d", u.TgID, page))))
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("adm:as:p:%d", page-1)))
	}
	if (page+1)*pageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("adm:as:p:%d", page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn("Выбрать всех", "adm:as:all"), btn("Снять выбор", "adm:as:none")))
	if len(st.Selected) > 0 {
		rows = append(rows, row(btn(fmt.Sprintf("🚀 Отправить (%d)", len(st.Selected)), "adm:as:go")))
	}
	rows = append(rows, row(btn("Отмена", "adm:as:x")))

	title := "?"
	if sc, _, err := a.scenarios.Get(ctx, st.ScenarioID); err == nil {
		title = sc.Title
	}
	text := fmt.Sprintf("<b>Назначение задания</b>\nСценарий: %s\nСрок: %d дн. после принятия\n\nШаг 3 из 3: отметьте покупателей (выбрано: %d).",
		esc(title), st.Days, len(st.Selected))
	if total == 0 {
		text += fmt.Sprintf("\n\nПодходящих пользователей нет: никто из активных не выбрал тип заданий «%s».", kind.Title())
	} else {
		text += fmt.Sprintf("\nПоказаны только те, кто выбрал тип «%s».", kind.Title())
	}
	return text, kb(rows...)
}

// assignExecute создаёт задания и рассылает их. Задание сохраняется до отправки,
// поэтому недоставленное можно отправить повторно из карточки.
func (a *App) assignExecute(ctx context.Context, b *bot.Bot, admin *domain.User, st assignState) (string, *models.InlineKeyboardMarkup, string) {
	if st.ScenarioID == 0 || st.Days == 0 || len(st.Selected) == 0 {
		t, m := a.screenAssignScenarios(ctx)
		return t, m, "Сначала выберите сценарий, срок и покупателей"
	}
	res, err := a.tasks.Assign(ctx, admin.TgID, st.ScenarioID, st.Days, st.Selected)
	if err != nil {
		a.log.Error("назначение заданий", "err", err)
		return "Не удалось назначить: " + esc(errText(err)), kb(row(btn("« Назад", "adm:home"))), ""
	}
	_ = a.dialog.Clear(ctx, admin.TgID)

	var delivered int
	var failed []string
	for _, t := range res.Created {
		if a.deliverTask(ctx, b, t.ID) {
			delivered++
		} else {
			failed = append(failed, a.nameOf(ctx, t.UserID))
		}
	}
	a.log.Info("задания назначены", "admin", maskID(admin.TgID), "created", len(res.Created), "delivered", delivered)

	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Готово</b>\nОтправлено: %d\n", delivered)
	if len(failed) > 0 {
		fmt.Fprintf(&sb, "\n⚠ Не доставлено (%d): %s\nЗадание создано, отправить повторно можно из карточки задания.\n", len(failed), esc(strings.Join(failed, ", ")))
	}
	if len(res.Repeat) > 0 {
		fmt.Fprintf(&sb, "\nℹ У них уже было незавершённое задание по этому сценарию, выдано ещё одно (%d): %s\n", len(res.Repeat), esc(a.names(ctx, res.Repeat)))
	}
	if len(res.WrongKind) > 0 {
		fmt.Fprintf(&sb, "\n⛔ Не выдано: пользователь не выбрал этот тип заданий (%d): %s\n", len(res.WrongKind), esc(a.names(ctx, res.WrongKind)))
	}
	if len(res.Invalid) > 0 {
		fmt.Fprintf(&sb, "\nНедоступны (заблокированы или не исполнители) (%d): %s\n", len(res.Invalid), esc(a.names(ctx, res.Invalid)))
	}
	return sb.String(), kb(row(btn("📌 К заданиям", "adm:tk:a:0"), btn("« В меню", "adm:home"))), ""
}

// scenarioKind тип сценария (покупатель, если сценарий не найден).
func (a *App) scenarioKind(ctx context.Context, scenarioID int64) domain.TaskKind {
	if sc, _, err := a.scenarios.Get(ctx, scenarioID); err == nil && sc.Kind.Valid() {
		return sc.Kind
	}
	return domain.KindBuyer
}

func (a *App) nameOf(ctx context.Context, id int64) string {
	u, err := a.access.Lookup(ctx, id)
	if err != nil || u == nil || strings.TrimSpace(u.FirstName) == "" {
		return itoa(id)
	}
	return strings.TrimSpace(u.FirstName)
}

func (a *App) names(ctx context.Context, ids []int64) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, a.nameOf(ctx, id))
	}
	return strings.Join(out, ", ")
}
