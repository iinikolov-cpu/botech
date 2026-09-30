package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
)

const (
	promoAddDialog = "promo_add"
	promoDelDialog = "promo_del"
)

// maxPromoFile лимит размера файла с промокодами.
const maxPromoFile = 512 * 1024

// promoCallback экраны раздела «Промокоды»: adm:pr, adm:pra, adm:pri:<стр>, adm:pru:<id>:<l|t>:<арг>.
func (a *App) promoCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	arg := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	switch parts[1] {
	case "pra":
		if err := a.dialog.Set(ctx, admin.TgID, promoAddDialog, struct{}{}); err != nil {
			a.log.Error("состояние загрузки промокодов", "err", err)
		}
		return "<b>Загрузка промокодов</b>\n\nОтправьте коды одним из способов:\n" +
				"• сообщением: по одному коду в строке;\n" +
				"• файлом .csv или .txt (по одному коду в строке, если колонок несколько, берётся первая).\n\n" +
				"Повторяющиеся коды пропускаются автоматически.",
			kb(row(btn("Отмена", "adm:prc"))), ""
	case "prd": // меню удаления
		st, _ := a.promos.Stats(ctx)
		return fmt.Sprintf("<b>Удаление промокодов</b>\n\nМожно удалить только <b>свободные</b> коды (сейчас их %d). "+
				"Выданные и использованные коды не удаляются: по ним ведётся учёт.", st.Free), kb(
				row(btn("🗑 Удалить все свободные", "adm:prda")),
				row(btn("✂ Удалить по списку", "adm:prdl")),
				row(btn("« Назад", "adm:pr")),
			), ""
	case "prda": // подтверждение
		st, _ := a.promos.Stats(ctx)
		if st.Free == 0 {
			return "Свободных кодов нет, удалять нечего.", kb(row(btn("« Назад", "adm:pr"))), ""
		}
		return fmt.Sprintf("Удалить все свободные промокоды (%d шт.)? Это нельзя отменить.", st.Free),
			kb(row(btn("Да, удалить", "adm:prdy"), btn("Отмена", "adm:pr"))), ""
	case "prdy":
		n, err := a.promos.DeleteAllFree(ctx, admin.TgID)
		if err != nil {
			a.log.Error("удаление промокодов", "err", err)
			return "Не удалось удалить, подробности в логах.", kb(row(btn("« Назад", "adm:pr"))), ""
		}
		a.log.Info("свободные промокоды удалены", "admin", maskID(admin.TgID), "count", n)
		t, m := a.screenPromos(ctx)
		return t, m, fmt.Sprintf("Удалено: %d", n)
	case "prdl": // удаление по списку
		if err := a.dialog.Set(ctx, admin.TgID, promoDelDialog, struct{}{}); err != nil {
			a.log.Error("состояние удаления промокодов", "err", err)
		}
		return "<b>Удаление по списку</b>\n\nОтправьте коды, которые нужно удалить: сообщением или файлом .csv/.txt, по одному в строке. " +
			"Удалятся только свободные коды из списка.", kb(row(btn("Отмена", "adm:prc"))), ""
	case "prc":
		_ = a.dialog.Clear(ctx, admin.TgID)
		t, m := a.screenPromos(ctx)
		return t, m, ""
	case "pri":
		page, _ := strconv.Atoi(arg(2))
		t, m := a.screenPromoIssued(ctx, page)
		return t, m, ""
	case "pru":
		id, _ := strconv.ParseInt(arg(2), 10, 64)
		toast := "Отмечено"
		if ok, err := a.promos.MarkUsed(ctx, admin.TgID, id); err != nil {
			a.log.Error("отметка промокода", "err", err)
			toast = "Ошибка, подробности в логах"
		} else if !ok {
			toast = "Уже отмечено"
		}
		if arg(3) == "t" { // вернуться в карточку задания
			taskID, _ := strconv.ParseInt(arg(4), 10, 64)
			t, m := a.screenTask(ctx, taskID)
			return t, m, toast
		}
		page, _ := strconv.Atoi(arg(4))
		t, m := a.screenPromoIssued(ctx, page)
		return t, m, toast
	}
	t, m := a.screenPromos(ctx)
	return t, m, ""
}

func (a *App) screenPromos(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	st, err := a.promos.Stats(ctx)
	if err != nil {
		a.log.Error("статистика промокодов", "err", err)
	}
	text := fmt.Sprintf("<b>Промокоды</b>\n\nСвободно: <b>%d</b>\nВыдано (не использовано): %d\nИспользовано: %d\n",
		st.Free, st.Issued, st.Used)
	if st.Free == 0 {
		text += "\n⚠ Пул пуст: покупатели не получат промокоды при принятии заданий."
	} else if st.Free <= a.promoLow {
		text += fmt.Sprintf("\n⚠ Промокоды заканчиваются (порог предупреждения: %d).", a.promoLow)
	}
	return text, kb(
		row(btn("➕ Загрузить коды", "adm:pra")),
		row(btn("📤 Выданные коды", "adm:pri:0"), btn("🗑 Удалить коды", "adm:prd")),
		row(btn("« Назад", "adm:home")),
	)
}

func (a *App) screenPromoIssued(ctx context.Context, page int) (string, *models.InlineKeyboardMarkup) {
	if page < 0 {
		page = 0
	}
	list, total, err := a.promos.Issued(ctx, false, pageSize, page*pageSize)
	if err != nil {
		a.log.Error("выданные промокоды", "err", err)
		return "Не удалось загрузить список.", kb(row(btn("« Назад", "adm:pr")))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Выданные коды</b> (не использованы: %d)\nНажмите кнопку, когда покупатель использовал код.\n\n", total)
	var rows [][]models.InlineKeyboardButton
	for _, it := range list {
		name := itoa(it.Code.UserID)
		if it.User != nil && strings.TrimSpace(it.User.FirstName) != "" {
			name = strings.TrimSpace(it.User.FirstName)
		}
		fmt.Fprintf(&sb, "<code>%s</code> · %s · %s · задание #%d\n", esc(it.Code.Code), esc(name), a.fmtTime(it.Code.IssuedAt), it.Code.TaskID)
		rows = append(rows, row(btn(cut("✔ Использован: "+it.Code.Code, 60), fmt.Sprintf("adm:pru:%d:l:%d", it.Code.ID, page))))
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("adm:pri:%d", page-1)))
	}
	if (page+1)*pageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("adm:pri:%d", page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn("« Назад", "adm:pr")))
	return sb.String(), kb(rows...)
}

// importPromos загружает коды из текста и отвечает итогом.
func (a *App) importPromos(ctx context.Context, b *bot.Bot, admin *domain.User, raw string) {
	back := kb(row(btn("🎁 К промокодам", "adm:pr")))
	res, err := a.promos.Add(ctx, admin.TgID, raw)
	if err != nil {
		a.send(ctx, b, admin.TgID, "❌ "+esc(errText(err)), back)
		return
	}
	_ = a.dialog.Clear(ctx, admin.TgID)
	a.log.Info("промокоды загружены", "admin", maskID(admin.TgID), "added", res.Added)
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Добавлено кодов: <b>%d</b>\n", res.Added)
	if res.Duplicates > 0 {
		fmt.Fprintf(&sb, "Пропущено повторов: %d\n", res.Duplicates)
	}
	if len(res.Invalid) > 0 {
		shown := res.Invalid
		if len(shown) > 10 {
			shown = shown[:10]
		}
		fmt.Fprintf(&sb, "Не похоже на код (%d): %s\nКод: 3-64 символа, латиница, цифры, - и _.\n", len(res.Invalid), esc(strings.Join(shown, ", ")))
	}
	if res.Added == 0 && res.Duplicates == 0 && len(res.Invalid) == 0 {
		sb.WriteString("Кодов не найдено. Пришлите по одному коду в строке.\n")
	}
	a.send(ctx, b, admin.TgID, sb.String(), back)
}

// deletePromos удаляет свободные коды из присланного списка и отвечает итогом.
func (a *App) deletePromos(ctx context.Context, b *bot.Bot, admin *domain.User, raw string) {
	back := kb(row(btn("🎁 К промокодам", "adm:pr")))
	res, err := a.promos.DeleteFree(ctx, admin.TgID, raw)
	if err != nil {
		a.log.Error("удаление промокодов по списку", "err", err)
		a.send(ctx, b, admin.TgID, "Не удалось удалить, подробности в логах.", back)
		return
	}
	_ = a.dialog.Clear(ctx, admin.TgID)
	a.log.Info("промокоды удалены по списку", "admin", maskID(admin.TgID), "count", res.Deleted)
	text := fmt.Sprintf("🗑 Удалено кодов: <b>%d</b>\n", res.Deleted)
	if res.Skipped > 0 {
		text += fmt.Sprintf("Не удалено (нет в пуле или уже выданы): %d\n", res.Skipped)
	}
	if len(res.Invalid) > 0 {
		text += fmt.Sprintf("Не похоже на код: %d\n", len(res.Invalid))
	}
	a.send(ctx, b, admin.TgID, text, back)
}

// onPromoFile принимает файл .csv или .txt с промокодами.
func (a *App) onPromoFile(ctx context.Context, b *bot.Bot, admin *domain.User, doc *models.Document) {
	back := kb(row(btn("🎁 К промокодам", "adm:pr")))
	if doc.FileSize > maxPromoFile {
		a.send(ctx, b, admin.TgID, fmt.Sprintf("Файл слишком большой (максимум %d КБ).", maxPromoFile/1024), back)
		return
	}
	data, err := a.download(ctx, b, doc.FileID, maxPromoFile)
	if err != nil {
		a.log.Error("скачивание файла промокодов", "err", err)
		a.send(ctx, b, admin.TgID, "Не удалось скачать файл, попробуйте ещё раз.", back)
		return
	}
	if state, _ := a.dialog.Get(ctx, admin.TgID, nil); state == promoDelDialog {
		a.deletePromos(ctx, b, admin, string(data))
		return
	}
	a.importPromos(ctx, b, admin, string(data))
}

// notifyAdmins рассылает текст всем активным админам.
func (a *App) notifyAdmins(ctx context.Context, b *bot.Bot, text string, markup *models.InlineKeyboardMarkup) {
	admins, err := a.access.ActiveAdmins(ctx)
	if err != nil {
		a.log.Error("не удалось получить админов", "err", err)
		return
	}
	for _, ad := range admins {
		a.send(ctx, b, ad.TgID, text, markup)
	}
}
