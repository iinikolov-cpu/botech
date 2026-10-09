package handlers

import (
	"context"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
	"botech/internal/i18n"
)

// Выбор типов заданий (покупатель, продавец, оба). Состояние выбора зашито в callback-данные
// в виде маски (1 покупатель, 2 продавец), поэтому в базе ничего не хранится до нажатия «Готово»:
//   kd:s:<маска>  показать экран с этой отметкой
//   kd:ok:<маска> сохранить

const (
	maskBuyer  = 1
	maskSeller = 2
)

func kindsMask(kinds []domain.TaskKind) int {
	m := 0
	for _, k := range kinds {
		switch k {
		case domain.KindBuyer:
			m |= maskBuyer
		case domain.KindSeller:
			m |= maskSeller
		}
	}
	return m
}

func maskKinds(m int) []domain.TaskKind {
	var out []domain.TaskKind
	if m&maskBuyer != 0 {
		out = append(out, domain.KindBuyer)
	}
	if m&maskSeller != 0 {
		out = append(out, domain.KindSeller)
	}
	return out
}

// screenKinds экран выбора типов заданий с текущей отметкой mask.
func screenKinds(l i18n.Lang, mask int) (string, *models.InlineKeyboardMarkup) {
	box := func(on bool, title string) string {
		if on {
			return "☑ " + title
		}
		return "☐ " + title
	}
	rows := [][]models.InlineKeyboardButton{
		row(btn(box(mask&maskBuyer != 0, i18n.T(l, "kind_buyer")), "kd:s:"+itoa(int64(mask^maskBuyer)))),
		row(btn(box(mask&maskSeller != 0, i18n.T(l, "kind_seller")), "kd:s:"+itoa(int64(mask^maskSeller)))),
	}
	if mask != 0 {
		rows = append(rows, row(btn(i18n.T(l, "btn_kinds_done"), "kd:ok:"+itoa(int64(mask)))))
	}
	return i18n.T(l, "kinds_ask"), kb(rows...)
}

// askKinds показывает пользователю экран выбора типов заданий.
func (a *App) askKinds(ctx context.Context, b *bot.Bot, u *domain.User) {
	text, markup := screenKinds(lang(u), kindsMask(u.Kinds))
	a.sendPanel(ctx, b, u.TgID, text, markup)
}

// onKindsCallback обрабатывает kd:s:<маска> и kd:ok:<маска>.
func (a *App) onKindsCallback(ctx context.Context, b *bot.Bot, upd *models.Update) {
	cb := upd.CallbackQuery
	u := userFrom(ctx)
	if u == nil {
		return
	}
	l := lang(u)
	parts := strings.Split(cb.Data, ":")
	if len(parts) < 3 {
		a.answerCB(ctx, b, cb.ID, "", false)
		return
	}
	mask, err := strconv.Atoi(parts[2])
	if err != nil {
		a.answerCB(ctx, b, cb.ID, "", false)
		return
	}
	mask &= maskBuyer | maskSeller
	if parts[1] != "ok" {
		a.answerCB(ctx, b, cb.ID, "", false)
		text, markup := screenKinds(l, mask)
		a.edit(ctx, b, cb, text, markup)
		return
	}
	if err := a.access.SetKinds(ctx, u.TgID, maskKinds(mask)); err != nil {
		a.answerCB(ctx, b, cb.ID, i18n.T(l, "kinds_need_one"), true)
		return
	}
	a.log.Info("типы заданий выбраны", "user", maskID(u.TgID), "kinds", domain.JoinKinds(maskKinds(mask)))
	a.answerCB(ctx, b, cb.ID, i18n.T(l, "kinds_saved"), false)
	u.Kinds = maskKinds(mask)
	text, markup := a.screenBuyerTasks(ctx, u)
	a.edit(ctx, b, cb, text, markup)
}
