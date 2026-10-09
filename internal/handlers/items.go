package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/service"
)

const (
	itemAddDialog = "item_add"
	itemDelDialog = "item_del"
)

// itemCallback экраны раздела «Айтемы»: adm:it, adm:ita, adm:itc, adm:itl:<стр>, adm:itd, adm:itdl, adm:itda, adm:itdy.
func (a *App) itemCallback(ctx context.Context, b *bot.Bot, admin *domain.User, parts []string) (string, *models.InlineKeyboardMarkup, string) {
	switch parts[1] {
	case "ita":
		if err := a.dialog.Set(ctx, admin.TgID, itemAddDialog, struct{}{}); err != nil {
			a.log.Error("состояние загрузки айтемов", "err", err)
		}
		return "<b>Загрузка айтемов</b>\n\nАйтем это товар (объявление), который покупатель должен купить. " +
				"Отправьте список одним из способов:\n" +
				"• сообщением, по одному айтему в строке;\n" +
				"• файлом .csv или .txt (Excel: «Сохранить как CSV»).\n\n" +
				"Формат строки: <code>Название; ссылка; цена; заметка</code>\n" +
				"Цена и заметка необязательны. Колонки можно разделять «;», запятой, табуляцией или «|». Пример:\n" +
				"<code>Куртка зимняя; https://olx.uz/d/obyavlenie/123; 350000; размер L</code>\n\n" +
				"Айтем с уже загруженной ссылкой пропускается.",
			kb(row(btn("Отмена", "adm:itc"))), ""
	case "itl":
		page := 0
		if len(parts) > 2 {
			fmt.Sscan(parts[2], &page)
		}
		t, m := a.screenItemTable(ctx, page)
		return t, m, ""
	case "itd":
		st, _ := a.items.Stats(ctx)
		return fmt.Sprintf("<b>Удаление айтемов</b>\n\nВсего: %d, свободных: %d. Удалять можно только свободные: "+
				"выбранные покупателями и купленные остаются.", st.Total, st.Free), kb(
				row(btn("✂ Удалить по номерам", "adm:itdl")),
				row(btn("🗑 Удалить все свободные", "adm:itda")),
				row(btn("« Назад", "adm:it")),
			), ""
	case "itdl":
		if err := a.dialog.Set(ctx, admin.TgID, itemDelDialog, struct{}{}); err != nil {
			a.log.Error("состояние удаления айтемов", "err", err)
		}
		return "<b>Удаление по номерам</b>\n\nОтправьте номера айтемов из списка (например <code>3, 5, 8</code>). " +
			"Удалятся только свободные.", kb(row(btn("Отмена", "adm:itc"))), ""
	case "itda":
		st, _ := a.items.Stats(ctx)
		if st.Free == 0 {
			return "Свободных айтемов нет, удалять нечего.", kb(row(btn("« Назад", "adm:it"))), ""
		}
		return fmt.Sprintf("Удалить все свободные айтемы (%d шт.)? Это нельзя отменить.", st.Free),
			kb(row(btn("Да, удалить", "adm:itdy"), btn("Отмена", "adm:it"))), ""
	case "itdy":
		n, err := a.items.DeleteAllFree(ctx, admin.TgID)
		if err != nil {
			a.log.Error("удаление айтемов", "err", err)
			return "Не удалось удалить, подробности в логах.", kb(row(btn("« Назад", "adm:it"))), ""
		}
		a.log.Info("свободные айтемы удалены", "admin", maskID(admin.TgID), "count", n)
		t, m := a.screenItems(ctx)
		return t, m, fmt.Sprintf("Удалено: %d", n)
	case "itc":
		_ = a.dialog.Clear(ctx, admin.TgID)
	}
	t, m := a.screenItems(ctx)
	return t, m, ""
}

func (a *App) screenItems(ctx context.Context) (string, *models.InlineKeyboardMarkup) {
	st, err := a.items.Stats(ctx)
	if err != nil {
		a.log.Error("статистика айтемов", "err", err)
	}
	text := fmt.Sprintf("<b>Айтемы</b> (товары для покупки)\n\nВсего: %d\nСвободно: <b>%d</b>\nВыбрано покупателями: %d\nКуплено: %d\n",
		st.Total, st.Free, st.Reserved, st.Used)
	if st.Free == 0 {
		text += "\n⚠ Свободных айтемов нет. Загрузите новые, иначе покупатели не смогут выбрать товар."
	}
	return text, kb(
		row(btn("📋 Список", "adm:itl:0")),
		row(btn("➕ Загрузить", "adm:ita"), btn("🗑 Удалить", "adm:itd")),
		row(btn("« Назад", "adm:home")),
	)
}

const itemPageSize = 15

// screenItemTable таблица айтемов: номер, название, цена, состояние.
func (a *App) screenItemTable(ctx context.Context, page int) (string, *models.InlineKeyboardMarkup) {
	if page < 0 {
		page = 0
	}
	list, total, err := a.items.List(ctx, itemPageSize, page*itemPageSize)
	if err != nil {
		a.log.Error("список айтемов", "err", err)
		return "Не удалось загрузить список.", kb(row(btn("« Назад", "adm:it")))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Список айтемов</b> (всего: %d)\n\n", total)
	if total == 0 {
		sb.WriteString("Айтемов пока нет.")
	} else {
		sb.WriteString("<pre>")
		sb.WriteString(padRight("№", 4) + " " + padRight("Название", 20) + " " + padRight("Цена", 9) + " Состояние\n")
		for _, it := range list {
			price := "-"
			if it.Price > 0 {
				price = fmtMoney(it.Price)
			}
			state := "свободен"
			switch it.Status {
			case domain.ItemReserved:
				state = "выбран, #" + itoa(it.TaskID)
			case domain.ItemUsed:
				state = "куплен, #" + itoa(it.TaskID)
			}
			fmt.Fprintf(&sb, "%s %s %s %s\n", padRight(itoa(it.ID), 4), padRight(esc(cut(it.Title, 20)), 20), padRight(price, 9), state)
		}
		sb.WriteString("</pre>\n№ нужен для удаления. После «#» номер задания.")
	}
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, btn("‹", fmt.Sprintf("adm:itl:%d", page-1)))
	}
	if (page+1)*itemPageSize < total {
		nav = append(nav, btn("›", fmt.Sprintf("adm:itl:%d", page+1)))
	}
	var rows [][]models.InlineKeyboardButton
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, row(btn("« Назад", "adm:it")))
	return cut(sb.String(), 4000), kb(rows...)
}

// importItems загружает айтемы из текста и отвечает итогом.
func (a *App) importItems(ctx context.Context, b *bot.Bot, admin *domain.User, raw string) {
	back := kb(row(btn("🛒 К айтемам", "adm:it")))
	res, err := a.items.Add(ctx, admin.TgID, raw)
	if err != nil {
		a.sendKeyed(ctx, b, admin.TgID, "err", "❌ "+esc(errText(err)), nil)
		return
	}
	_ = a.dialog.Clear(ctx, admin.TgID)
	a.log.Info("айтемы загружены", "admin", maskID(admin.TgID), "added", res.Added)
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Добавлено айтемов: <b>%d</b>\n", res.Added)
	if res.Duplicates > 0 {
		fmt.Fprintf(&sb, "Пропущено (такая ссылка уже есть): %d\n", res.Duplicates)
	}
	if len(res.Invalid) > 0 {
		shown := res.Invalid
		if len(shown) > 8 {
			shown = shown[:8]
		}
		fmt.Fprintf(&sb, "\nНе принято (%d):\n", len(res.Invalid))
		for _, s := range shown {
			sb.WriteString("• " + esc(s) + "\n")
		}
		sb.WriteString("Нужен формат: название; ссылка (http или https); цена; заметка.\n")
	}
	if res.Added == 0 && res.Duplicates == 0 && len(res.Invalid) == 0 {
		sb.WriteString("Айтемов не найдено. Пришлите по одному в строке: название; ссылка.\n")
	}
	a.sendPanel(ctx, b, admin.TgID, sb.String(), back)
}

// deleteItems удаляет свободные айтемы по списку номеров и отвечает итогом.
func (a *App) deleteItems(ctx context.Context, b *bot.Bot, admin *domain.User, raw string) {
	back := kb(row(btn("🛒 К айтемам", "adm:it")))
	res, err := a.items.DeleteIDs(ctx, admin.TgID, raw)
	if err != nil {
		a.log.Error("удаление айтемов по номерам", "err", err)
		a.sendKeyed(ctx, b, admin.TgID, "err", "Не удалось удалить, подробности в логах.", nil)
		return
	}
	_ = a.dialog.Clear(ctx, admin.TgID)
	text := fmt.Sprintf("🗑 Удалено айтемов: <b>%d</b>\n", res.Deleted)
	if res.Skipped > 0 {
		text += fmt.Sprintf("Не удалено (нет такого номера или айтем уже выбран/куплен): %d\n", res.Skipped)
	}
	if len(res.Invalid) > 0 {
		text += fmt.Sprintf("Не похоже на номер: %d\n", len(res.Invalid))
	}
	a.sendPanel(ctx, b, admin.TgID, text, back)
}

// warnNoItems предупреждает админов, что свободных айтемов нет, а покупатель ждёт выбора.
func (a *App) warnNoItems(ctx context.Context, b *bot.Bot, c *service.TaskCard) {
	text := fmt.Sprintf("⚠ Нет свободных айтемов, а покупатель (%s) ждёт, чтобы выбрать товар для задания #%d. Загрузите новые айтемы.",
		userLabel(c.User), c.Task.ID)
	a.notifyAdminsKeyed(ctx, b, "item_warn", text, kb(row(btn("🛒 Айтемы", "adm:it"))))
}
