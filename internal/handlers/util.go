package handlers

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"botech/internal/domain"
)

// Callback-данные ограничены 64 байтами, поэтому формат короткий: "adm:<раздел>:<аргументы>".

type ctxKey struct{}

// withUser кладёт пользователя в контекст (его проверил middleware доступа).
func withUser(ctx context.Context, u *domain.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func userFrom(ctx context.Context) *domain.User {
	u, _ := ctx.Value(ctxKey{}).(*domain.User)
	return u
}

// esc экранирует текст для parse_mode=HTML.
func esc(s string) string { return html.EscapeString(s) }

// maskID скрывает Telegram ID для логов: остаются только последние 4 цифры.
func maskID(id int64) string {
	s := fmt.Sprint(id)
	if len(s) <= 4 {
		return "***"
	}
	return "***" + s[len(s)-4:]
}

// userLabel человекочитаемая подпись пользователя со ссылкой на профиль.
func userLabel(u *domain.User) string {
	name := strings.TrimSpace(u.FirstName)
	if name == "" {
		name = "без имени"
	}
	s := fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, u.TgID, esc(name))
	if u.Username != "" {
		s += " @" + esc(u.Username)
	}
	return s
}

// lastPart последний элемент разобранных callback-данных ("" для пустого списка).
func lastPart(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// itoa короткая запись числа для callback-данных.
func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// mark выделяет активную вкладку.
func mark(s string, on bool) string {
	if on {
		return "• " + s
	}
	return s
}

// cut обрезает строку по числу символов (для подписей кнопок).
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fmtMoney форматирует сумму с пробелами между тысячами: 150 000.
func fmtMoney(v int64) string {
	s := strconv.FormatInt(v, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out)
}

// orDash подпись для пустой даты в настройках.
func orDash(s string) string {
	if s == "" {
		return "ещё не было"
	}
	return s
}

func btn(text, data string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: data}
}

func kb(rows ...[]models.InlineKeyboardButton) *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func row(b ...models.InlineKeyboardButton) []models.InlineKeyboardButton { return b }

// send отправляет HTML-сообщение; ошибки только логируются.
func (a *App) send(ctx context.Context, b *bot.Bot, chatID int64, text string, markup *models.InlineKeyboardMarkup) {
	if err := a.trySend(ctx, b, chatID, text, markup); err != nil {
		a.log.Warn("не удалось отправить сообщение", "user", maskID(chatID), "err", err)
	}
}

// trySend отправляет HTML-сообщение и возвращает ошибку (нужно там, где важен факт доставки).
func (a *App) trySend(ctx context.Context, b *bot.Bot, chatID int64, text string, markup *models.InlineKeyboardMarkup) error {
	_, err := a.sendMsg(ctx, b, chatID, text, markup)
	return err
}

// sendMsg отправляет HTML-сообщение и возвращает его (нужен message_id для панелей).
func (a *App) sendMsg(ctx context.Context, b *bot.Bot, chatID int64, text string, markup *models.InlineKeyboardMarkup) (*models.Message, error) {
	p := &bot.SendMessageParams{
		ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}
	if markup != nil {
		p.ReplyMarkup = markup
	}
	return b.SendMessage(ctx, p)
}

// edit заменяет текст сообщения с кнопкой (для навигации по меню).
func (a *App) edit(ctx context.Context, b *bot.Bot, cb *models.CallbackQuery, text string, markup *models.InlineKeyboardMarkup) {
	msg := cb.Message.Message
	if msg == nil {
		a.sendPanel(ctx, b, cb.From.ID, text, markup)
		return
	}
	a.adopt(ctx, b, msg.Chat.ID, msg.ID)
	var err error
	if msg.Photo != nil || msg.Video != nil || msg.Document != nil {
		// У сообщений с медиа меняется подпись, а не текст.
		p := &bot.EditMessageCaptionParams{ChatID: msg.Chat.ID, MessageID: msg.ID, Caption: cut(text, 1000), ParseMode: models.ParseModeHTML}
		if markup != nil {
			p.ReplyMarkup = markup
		}
		_, err = b.EditMessageCaption(ctx, p)
	} else {
		p := &bot.EditMessageTextParams{
			ChatID: msg.Chat.ID, MessageID: msg.ID, Text: text, ParseMode: models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		}
		if markup != nil {
			p.ReplyMarkup = markup
		}
		_, err = b.EditMessageText(ctx, p)
	}
	if err != nil && !strings.Contains(err.Error(), "not modified") {
		a.log.Warn("не удалось изменить сообщение", "user", maskID(cb.From.ID), "err", err)
	}
}

// answerCB закрывает «часики» на кнопке; alert показывает всплывающее окно.
func (a *App) answerCB(ctx context.Context, b *bot.Bot, id, text string, alert bool) {
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text, ShowAlert: alert})
}

// limiter простой ограничитель «не больше n событий за окно» на ключ.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	events map[int64][]time.Time
	now    func() time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, events: map[int64][]time.Time{}, now: time.Now}
}

// Allow фиксирует событие и возвращает false, если лимит превышен.
func (l *limiter) Allow(key int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	kept := l.events[key][:0]
	for _, t := range l.events[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.events[key] = kept
		return false
	}
	l.events[key] = append(kept, now)
	if len(l.events) > limiterSweepAt {
		l.sweep(cut)
	}
	return true
}

// limiterSweepAt при каком числе ключей удалять устаревшие, чтобы карта не росла без конца
// (например, при потоке сообщений от тысяч посторонних аккаунтов).
const limiterSweepAt = 1024

// sweep удаляет ключи, у которых не осталось событий внутри окна (вызывать под мьютексом).
func (l *limiter) sweep(cut time.Time) {
	for k, evs := range l.events {
		if len(evs) == 0 || !evs[len(evs)-1].After(cut) {
			delete(l.events, k)
		}
	}
}

// Blocked true, если лимит уже исчерпан (без записи нового события).
func (l *limiter) Blocked(key int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := l.now().Add(-l.window)
	c := 0
	for _, t := range l.events[key] {
		if t.After(cut) {
			c++
		}
	}
	return c >= l.n
}

// mediaItem фото или видео из отчёта для показа админу.
type mediaItem struct {
	Video   bool
	FileID  string
	Caption string
}

// sendMedia отправляет фото и видео компактно: альбомами по 10 штук (одиночный файл обычным
// сообщением). Вызывать нужно ДО сообщения с кнопками, чтобы кнопки оказались в самом низу.
// Возвращает id отправленных сообщений: их можно убрать вместе с панелью (см. sendPanel).
func (a *App) sendMedia(ctx context.Context, b *bot.Bot, chatID int64, items []mediaItem) []int {
	var ids []int
	single := func(m mediaItem) {
		var (
			msg *models.Message
			err error
		)
		if m.Video {
			msg, err = b.SendVideo(ctx, &bot.SendVideoParams{ChatID: chatID, Video: &models.InputFileString{Data: m.FileID}, Caption: m.Caption})
		} else {
			msg, err = b.SendPhoto(ctx, &bot.SendPhotoParams{ChatID: chatID, Photo: &models.InputFileString{Data: m.FileID}, Caption: m.Caption})
		}
		if err != nil {
			a.log.Warn("не удалось отправить медиа", "err", err)
		} else if msg != nil {
			ids = append(ids, msg.ID)
		}
	}
	for len(items) > 0 {
		n := len(items)
		if n > 10 {
			n = 10
		}
		chunk := items[:n]
		items = items[n:]
		if len(chunk) == 1 {
			single(chunk[0])
			continue
		}
		group := make([]models.InputMedia, 0, len(chunk))
		for _, m := range chunk {
			if m.Video {
				group = append(group, &models.InputMediaVideo{Media: m.FileID, Caption: m.Caption})
			} else {
				group = append(group, &models.InputMediaPhoto{Media: m.FileID, Caption: m.Caption})
			}
		}
		msgs, err := b.SendMediaGroup(ctx, &bot.SendMediaGroupParams{ChatID: chatID, Media: group})
		if err != nil {
			a.log.Warn("альбом не отправился, отправляю по одному", "err", err)
			for _, m := range chunk {
				single(m)
			}
			continue
		}
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
	}
	return ids
}
