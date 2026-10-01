package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/service"
	"botech/internal/storage"
)

// Фильтры списка заданий: буква в callback -> условия выборки.
// «Отчёты» включают и принятые, но ещё не закрытые по компенсации: пока её не выплатили
// (или она отклонена и ждёт исправления), задание остаётся в этом списке.
var taskFilters = map[string]storage.TaskFilter{
	"a": {},
	"w": {Statuses: []domain.TaskStatus{domain.TaskCreated, domain.TaskSent}},
	"p": {Statuses: []domain.TaskStatus{domain.TaskAccepted}},
	"r": {Statuses: []domain.TaskStatus{domain.TaskReported, domain.TaskRework}, ReviewedMode: storage.ReviewedOpenComp},
	"o": {Statuses: []domain.TaskStatus{domain.TaskExpired}},
	"f": {Statuses: []domain.TaskStatus{domain.TaskDeclined, domain.TaskCancelled}, ReviewedMode: storage.ReviewedSettled},
}

// taskAdminCallback экраны заданий: adm:tk:<фильтр>:<стр>, adm:tc:<id>, adm:trv:<id>, adm:trs:<id>.
func (a *App) taskAdminCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	arg := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	id, _ := strconv.ParseInt(arg(2), 10, 64)
	switch parts[1] {
	case "tc":
		t, m := a.screenTask(ctx, id)
		return t, m, ""
	case "trv":
		toast := "Готово"
		changed, err := a.tasks.Review(ctx, admin.TgID, id)
		if err != nil {
			a.log.Error("проверка задания", "err", err)
			toast = "Ошибка, подробности в логах"
		} else if !changed {
			toast = "Отметить проверенным можно только задание с полученным отчётом"
		}
		t, m := a.screenTask(ctx, id)
		return t, m, toast
	case "tdl": // подтверждение удаления
		c, err := a.tasks.Card(ctx, id)
		if err != nil {
			t, m := a.screenTasks(ctx, "a", 0)
			return t, m, "Задание не найдено"
		}
		text := fmt.Sprintf("Удалить задание #%d («%s», %s)?\nОно исчезнет из списков вместе с историей. Восстановить нельзя.",
			id, esc(c.Version.Body.Title), userLabel(c.User))
		return text, kb(row(btn("Да, удалить", "adm:tdy:"+itoa(id)), btn("Отмена", "adm:tc:"+itoa(id)))), ""
	case "tdy":
		if err := a.tasks.Delete(ctx, admin.TgID, id); err != nil {
			t, m := a.screenTask(ctx, id)
			return t, m, errText(err)
		}
		a.log.Info("задание удалено", "admin", maskID(admin.TgID), "task", id)
		t, m := a.screenTasks(ctx, "a", 0)
		return t, m, "Задание удалено"
	case "tcl": // подтверждение отмены
		c, err := a.tasks.Card(ctx, id)
		if err != nil {
			t, m := a.screenTasks(ctx, "a", 0)
			return t, m, "Задание не найдено"
		}
		text := fmt.Sprintf("Отменить задание #%d («%s», %s)?\nПокупатель получит уведомление и больше не сможет отправить отчёт. Задание останется в истории со статусом «отменено».",
			id, esc(c.Version.Body.Title), userLabel(c.User))
		return text, kb(row(btn("Да, отменить", "adm:tcy:"+itoa(id)), btn("Назад", "adm:tc:"+itoa(id)))), ""
	case "tcy":
		toast := "Задание отменено"
		changed, err := a.tasks.Cancel(ctx, admin.TgID, id)
		switch {
		case err != nil:
			a.log.Error("отмена задания", "err", err)
			toast = "Ошибка, подробности в логах"
		case !changed:
			toast = "Отменить можно только принятое или просроченное задание"
		default:
			if c, err := a.tasks.Card(ctx, id); err == nil {
				a.send(ctx, b, c.User.TgID, fmt.Sprintf("Задание «%s» отменено администратором. Отправлять отчёт по нему больше не нужно.", esc(c.Version.Body.Title)), nil)
			}
			a.log.Info("задание отменено", "admin", maskID(admin.TgID), "task", id)
		}
		t, m := a.screenTask(ctx, id)
		return t, m, toast
	case "trs": // повторная отправка недоставленного задания
		toast := "Отправлено"
		if !a.deliverTask(ctx, b, id) {
			toast = "Не доставлено: покупатель недоступен"
		}
		t, m := a.screenTask(ctx, id)
		return t, m, toast
	}
	page, _ := strconv.Atoi(arg(3))
	t, m := a.screenTasks(ctx, arg(2), page)
	return t, m, ""
}

// waitNote отметка для списка заданий: что покупатель слишком долго не реагирует. Пусто, если всё в порядке.
// Порог задаётся в конфиге (STALE_MINUTES, по умолчанию сутки).
func (a *App) waitNote(c *service.TaskCard) string {
	stale := a.reminders.Stale()
	since := func(t time.Time) (time.Duration, bool) {
		if t.IsZero() {
			return 0, false
		}
		d := time.Since(t)
		return d, d > stale
	}
	switch c.Task.Status {
	case domain.TaskSent:
		if d, ok := since(c.Task.SentAt); ok {
			return "нет ответа " + humanDuration(d)
		}
	case domain.TaskExpired:
		if !c.Task.DueAt.IsZero() {
			return "просрочено на " + humanDuration(time.Since(c.Task.DueAt))
		}
		return "просрочено"
	case domain.TaskRework:
		if c.Report != nil {
			if d, ok := since(c.Report.DecidedAt); ok {
				return "на доработке " + humanDuration(d)
			}
		}
	}
	return ""
}

func (a *App) screenTasks(ctx context.Context, filter string, page int) (string, *models.InlineKeyboardMarkup) {
	f, ok := taskFilters[filter]
	if !ok {
		filter = "a"
	}
	if page < 0 {
		page = 0
	}
	list, total, err := a.tasks.Page(ctx, f, pageSize, page*pageSize)
	if err != nil {
		a.log.Error("список заданий", "err", err)
		return "Не удалось загрузить список.", kb(row(btn("« Назад", "adm:home")))
	}
	tab := func(title, f string) models.InlineKeyboardButton {
		return btn(mark(title, f == filter), "adm:tk:"+f+":0")
	}
	rows := [][]models.InlineKeyboardButton{
		row(tab("Все", "a"), tab("Ждут", "w"), tab("В работе", "p")),
		row(tab("Отчёты", "r"), tab("Просрочено", "o"), tab("Закрыто", "f")),
	}
	for _, c := range list {
		name := strings.TrimSpace(c.User.FirstName)
		if name == "" {
			name = itoa(c.User.TgID)
		}
		state, flag := c.Task.Status.Title(), ""
		if w := a.waitNote(c); w != "" {
			state, flag = w, "⚠ "
		}
		head := fmt.Sprintf("%s#%d ", flag, c.Task.ID)
		tail := " · " + name + " · " + state
		room := 60 - len([]rune(head)) - len([]rune(tail))
		if room < 8 {
			room = 8
		}
		rows = append(rows, row(btn(head+cut(c.Version.Body.Title, room)+tail, "adm:tc:"+itoa(c.Task.ID))))
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("adm:tk:%s:%d", filter, page-1)))
	}
	if (page+1)*pageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("adm:tk:%s:%d", filter, page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn("➕ Назначить", "adm:as:0"), btn("« Назад", "adm:home")))
	text := fmt.Sprintf("<b>Задания</b> (найдено: %d)", total)
	if total == 0 {
		text += "\n\nПока пусто."
	}
	return text, kb(rows...)
}

func (a *App) screenTask(ctx context.Context, id int64) (string, *models.InlineKeyboardMarkup) {
	c, err := a.tasks.Card(ctx, id)
	if err != nil {
		return "Задание не найдено.", kb(row(btn("« К заданиям", "adm:tk:a:0")))
	}
	t, body := c.Task, c.Version.Body
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Задание #%d</b>\n", t.ID)
	fmt.Fprintf(&sb, "Сценарий: %s (%s), версия %d\n", esc(body.Title), esc(body.Operator), c.Version.Version)
	fmt.Fprintf(&sb, "Покупатель: %s\n", userLabel(c.User))
	fmt.Fprintf(&sb, "Статус: <b>%s</b>\n", t.Status.Title())
	if w := a.waitNote(c); w != "" {
		fmt.Fprintf(&sb, "⚠ %s\n", w)
	}
	fmt.Fprintf(&sb, "Срок: %d дн. после принятия", t.DueDays)
	if !t.DueAt.IsZero() {
		fmt.Fprintf(&sb, " (до %s)", a.fmtTime(t.DueAt))
	}
	sb.WriteString("\n")
	if c.Promo != nil {
		state := map[domain.PromoOutcome]string{
			domain.PromoActive:   "закреплён за заданием",
			domain.PromoUsed:     "использован",
			domain.PromoReleased: "возвращён в оборот, задание отменено",
		}[c.Promo.Outcome]
		fmt.Fprintf(&sb, "Промокод: <code>%s</code> (%s)\n", esc(c.Promo.Code), state)
	}
	if c.Report != nil {
		fmt.Fprintf(&sb, "Отчёт: версия %d от %s\n", c.Report.Revision, a.fmtTime(c.Report.SubmittedAt))
		if c.Report.Decision == domain.ReportRework && c.Report.AdminComment != "" {
			fmt.Fprintf(&sb, "Возвращён на доработку: <i>%s</i>\n", esc(cut(c.Report.AdminComment, 300)))
		}
	}
	if c.Comp != nil {
		fmt.Fprintf(&sb, "Компенсация: %s сум (%s)\n", fmtMoney(c.Comp.Amount), compStatusTitle[c.Comp.Status])
	}

	events, _ := a.tasks.Events(ctx, id)
	if len(events) > 0 {
		sb.WriteString("\n<b>История</b>\n")
	}
	for _, e := range events {
		fmt.Fprintf(&sb, "%s  %s\n", a.fmtTime(e.At), esc(describeEvent(e)))
	}

	var rows [][]models.InlineKeyboardButton
	switch t.Status {
	case domain.TaskCreated:
		rows = append(rows, row(btn("🔁 Отправить повторно", "adm:trs:"+itoa(id))))
	case domain.TaskReported:
		rows = append(rows, row(btn("✅ Проверено", "adm:trv:"+itoa(id))))
	}
	switch c.Task.Status {
	case domain.TaskCreated, domain.TaskSent, domain.TaskDeclined:
		rows = append(rows, row(btn("🗑 Удалить задание", "adm:tdl:"+itoa(id))))
	case domain.TaskAccepted, domain.TaskExpired, domain.TaskRework:
		rows = append(rows, row(btn("🚫 Отменить задание", "adm:tcl:"+itoa(id))))
	}
	switch c.Task.Status {
	case domain.TaskSent, domain.TaskAccepted, domain.TaskExpired, domain.TaskRework:
		rows = append(rows, row(btn("🔔 Напомнить покупателю", "adm:tp:"+itoa(id))))
	}
	if c.Report != nil {
		rows = append(rows, row(btn("📄 Открыть отчёт", "adm:rv:"+itoa(id))))
	}
	if c.Comp != nil {
		rows = append(rows, row(btn("💰 Компенсация", "adm:cc:"+itoa(c.Comp.ID))))
	}
	rows = append(rows, row(btn("« К заданиям", "adm:tk:a:0")))
	return sb.String(), kb(rows...)
}

func describeEvent(e *domain.TaskEvent) string {
	switch e.Kind {
	case "created":
		return "создано: " + e.Details
	case "send_failed":
		return "не доставлено: " + e.Details
	case "status":
		s := fmt.Sprintf("%s → %s", e.FromStatus.Title(), e.ToStatus.Title())
		if e.Details != "" {
			s += " (" + e.Details + ")"
		}
		return s
	case "compensation_rejected":
		return "компенсация отклонена, " + e.Details
	case "compensation_resubmitted":
		return "покупатель исправил данные компенсации"
	case "promo_used":
		return "использование промокода засчитано"
	case "reminder":
		return "напоминание покупателю: " + e.Details
	case "escalation":
		return "эскалация: " + e.Details
	case "ping":
		return "ручное напоминание от админа"
	case "promo_issued":
		return "выдан промокод"
	case "promo_missing":
		return "промокод не выдан: пул пуст"
	}
	return e.Kind
}

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
		list, _, err := a.access.Users(ctx, domain.RoleBuyer, domain.StatusActive, 500, 0)
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
		rows = append(rows, row(btn(cut(fmt.Sprintf("%s · %s", s.Title, s.Operator), 60), "adm:as:s:"+itoa(s.ID))))
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
	list, total, err := a.access.Users(ctx, domain.RoleBuyer, domain.StatusActive, pageSize, page*pageSize)
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
		text += "\n\nАктивных покупателей пока нет."
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
	if len(res.Invalid) > 0 {
		fmt.Fprintf(&sb, "\nНедоступны (заблокированы или не покупатели) (%d): %s\n", len(res.Invalid), esc(a.names(ctx, res.Invalid)))
	}
	return sb.String(), kb(row(btn("📌 К заданиям", "adm:tk:a:0"), btn("« В меню", "adm:home"))), ""
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
