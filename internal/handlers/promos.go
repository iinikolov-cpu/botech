package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

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

// promoCallback экраны раздела «Промокоды»: adm:pr, adm:pra, adm:prc, adm:prl:<стр>, adm:prd*.
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
				"• сообщением: один код или несколько, по одному в строке;\n" +
				"• файлом .csv или .txt (по одному коду в строке, если колонок несколько, берётся первая).\n\n" +
				"Повторяющиеся коды пропускаются автоматически.",
			kb(row(btn("Отмена", "adm:prc"))), ""
	case "prl":
		page, _ := strconv.Atoi(arg(2))
		t, m := a.screenPromoTable(ctx, page)
		return t, m, ""
	case "prd": // меню удаления
		st, _ := a.promos.Stats(ctx)
		return fmt.Sprintf("<b>Удаление промокодов</b>\n\nВсего кодов: %d, из них занято активными заданиями: %d. "+
				"Занятые коды удалить нельзя, пока задание не завершено или не отменено.", st.Total, st.Busy), kb(
				row(btn("✂ Удалить один или несколько", "adm:prdl")),
				row(btn("🗑 Удалить все незанятые", "adm:prda")),
				row(btn("« Назад", "adm:pr")),
			), ""
	case "prda": // подтверждение
		st, _ := a.promos.Stats(ctx)
		n := st.Total - st.Busy
		if n == 0 {
			return "Незанятых кодов нет, удалять нечего.", kb(row(btn("« Назад", "adm:pr"))), ""
		}
		return fmt.Sprintf("Удалить все незанятые промокоды (%d шт.)? Это нельзя отменить.", n),
			kb(row(btn("Да, удалить", "adm:prdy"), btn("Отмена", "adm:pr"))), ""
	case "prdy":
		n, err := a.promos.DeleteAllFree(ctx, admin.TgID)
		if err != nil {
			a.log.Error("удаление промокодов", "err", err)
			return "Не удалось удалить, подробности в логах.", kb(row(btn("« Назад", "adm:pr"))), ""
		}
		a.log.Info("незанятые промокоды удалены", "admin", maskID(admin.TgID), "count", n)
		t, m := a.screenPromos(ctx)
		return t, m, fmt.Sprintf("Удалено: %d", n)
	case "prdl": // удаление по списку
		if err := a.dialog.Set(ctx, admin.TgID, promoDelDialog, struct{}{}); err != nil {
			a.log.Error("состояние удаления промокодов", "err", err)
		}
		return "<b>Удаление по списку</b>\n\nОтправьте код (или несколько): сообщением или файлом .csv/.txt, по одному в строке. " +
			"Удалятся только коды, не занятые активными заданиями.", kb(row(btn("Отмена", "adm:prc"))), ""
	case "prc":
		_ = a.dialog.Clear(ctx, admin.TgID)
	}
	t, m := a.screenPromos(ctx)
	return t, m, ""
}

func (a *App) screenPromos(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	st, err := a.promos.Stats(ctx)
	if err != nil {
		a.log.Error("статистика промокодов", "err", err)
	}
	text := fmt.Sprintf("<b>Промокоды</b>\n\nИспользований на код: <b>%d</b>\nВсего кодов: %d\n"+
		"Доступно для выдачи: <b>%d</b>\nЗаняты активными заданиями: %d\nИсчерпаны: %d\n",
		a.promos.MaxUses(), st.Total, st.Available, st.Busy, st.Exhausted)
	switch {
	case st.WithUsesLeft == 0:
		text += "\n⚠ Нет ни одного кода со свободными использованиями. Загрузите новые коды."
	case st.Available == 0:
		text += "\n⚠ Все коды сейчас заняты активными заданиями."
	}
	return text, kb(
		row(btn("📋 Список кодов", "adm:prl:0")),
		row(btn("➕ Загрузить коды", "adm:pra"), btn("🗑 Удалить коды", "adm:prd")),
		row(btn("« Назад", "adm:home")),
	)
}

const promoPageSize = 20

// screenPromoTable таблица кодов: сколько использований осталось и есть ли активное задание.
func (a *App) screenPromoTable(ctx context.Context, page int) (string, *models.InlineKeyboardMarkup) {
	if page < 0 {
		page = 0
	}
	list, total, err := a.promos.List(ctx, promoPageSize, page*promoPageSize)
	if err != nil {
		a.log.Error("список промокодов", "err", err)
		return "Не удалось загрузить список.", kb(row(btn("« Назад", "adm:pr")))
	}
	max := a.promos.MaxUses()
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Список кодов</b> (всего: %d)\n\n", total)
	if total == 0 {
		sb.WriteString("Кодов пока нет.")
	} else {
		// Моноширинный блок: Telegram не умеет таблицы, так колонки выровнены пробелами.
		sb.WriteString("<pre>")
		sb.WriteString(padRight("Код", 16) + " " + padRight("Осталось", 8) + " Задание\n")
		for _, r := range list {
			task := "нет"
			if r.TaskID != 0 {
				task = "#" + itoa(r.TaskID)
			}
			fmt.Fprintf(&sb, "%s %s %s\n", padRight(esc(cut(r.Code, 16)), 16), padRight(fmt.Sprintf("%d из %d", r.Left, max), 8), task)
		}
		sb.WriteString("</pre>\nОсталось: сколько раз код ещё можно использовать. Задание: активное задание, за которым код закреплён сейчас.")
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("adm:prl:%d", page-1)))
	}
	if (page+1)*promoPageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("adm:prl:%d", page+1)))
	}
	var rows [][]models.InlineKeyboardButton
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn("« Назад", "adm:pr")))
	return sb.String(), kb(rows...)
}

// padRight дополняет строку пробелами до n символов (считая руны, а не байты).
func padRight(s string, n int) string {
	if l := utf8.RuneCountInString(s); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
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
		text += fmt.Sprintf("Не удалено (нет в пуле или занято активным заданием): %d\n", res.Skipped)
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
