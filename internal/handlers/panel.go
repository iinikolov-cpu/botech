package handlers

import (
	"context"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Чтобы в чате не копились старые меню, у каждого пользователя есть одна «панель»: сообщение,
// с которым он сейчас работает. Новый экран заменяет старый: бот отправляет новое сообщение
// и удаляет прежнее (вместе с альбомом медиа, который к нему относился). Состояние хранится
// в памяти: после перезапуска бота старые сообщения просто остаются в чате.
//
// Уведомления (напоминания, «принял задание», доставленные задания) панелью не считаются и
// не удаляются, пока пользователь не нажмёт в них кнопку: тогда сообщение превращается в панель.

type chatPanel struct {
	id    int            // сообщение-панель (0, если нет)
	extra []int          // связанные сообщения (альбом медиа), удаляются вместе с панелью
	keyed map[string]int // «одно на ключ» сообщения, например предупреждение о промокодах
}

type panelStore struct {
	mu    sync.Mutex
	chats map[int64]*chatPanel
}

func newPanelStore() *panelStore { return &panelStore{chats: map[int64]*chatPanel{}} }

func (p *panelStore) get(chat int64) *chatPanel {
	c := p.chats[chat]
	if c == nil {
		c = &chatPanel{keyed: map[string]int{}}
		p.chats[chat] = c
	}
	return c
}

// swap делает msg новой панелью и возвращает сообщения прежней панели, которые нужно удалить.
func (p *panelStore) swap(chat int64, msg int, extra []int) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.get(chat)
	if c.id == msg && extra == nil { // то же сообщение: связанный альбом остаётся
		return nil
	}
	var old []int
	if c.id != 0 && c.id != msg {
		old = append(old, c.id)
	}
	if c.id != msg {
		old = append(old, c.extra...)
	}
	for k, id := range c.keyed {
		switch {
		case id == msg: // сообщение стало панелью: оно больше не «предупреждение»
			delete(c.keyed, k)
		case k == "err": // сообщение об ошибке ввода устарело, когда показан новый экран
			old = append(old, id)
			delete(c.keyed, k)
		}
	}
	c.id, c.extra = msg, extra
	return old
}

// replaceKeyed запоминает msg под ключом и возвращает прежнее сообщение с этим ключом.
func (p *panelStore) replaceKeyed(chat int64, key string, msg int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.get(chat)
	old := c.keyed[key]
	c.keyed[key] = msg
	if old == msg {
		return 0
	}
	return old
}

// deleteMsgs удаляет сообщения; ошибки не важны (сообщение могло быть удалено или устареть).
func (a *App) deleteMsgs(ctx context.Context, b *bot.Bot, chat int64, ids []int) {
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, err := b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chat, MessageID: id}); err != nil {
			a.log.Debug("сообщение не удалено", "err", err)
		}
	}
}

// sendPanel показывает экран новым сообщением и убирает прежнюю панель. extra: id сообщений
// (альбом медиа), которые нужно удалить вместе с этой панелью.
func (a *App) sendPanel(ctx context.Context, b *bot.Bot, chat int64, text string, markup *models.InlineKeyboardMarkup, extra ...int) {
	msg, err := a.sendMsg(ctx, b, chat, text, markup)
	if err != nil {
		a.log.Warn("не удалось отправить сообщение", "user", maskID(chat), "err", err)
		return
	}
	a.deleteMsgs(ctx, b, chat, a.panels.swap(chat, msg.ID, extra))
}

// sendKeyed отправляет уведомление, которое может быть только одно на ключ: повтор заменяет прежнее.
func (a *App) sendKeyed(ctx context.Context, b *bot.Bot, chat int64, key, text string, markup *models.InlineKeyboardMarkup) {
	msg, err := a.sendMsg(ctx, b, chat, text, markup)
	if err != nil {
		a.log.Warn("не удалось отправить сообщение", "user", maskID(chat), "err", err)
		return
	}
	if old := a.panels.replaceKeyed(chat, key, msg.ID); old != 0 {
		a.deleteMsgs(ctx, b, chat, []int{old})
	}
}

// adopt вызывается, когда пользователь нажал кнопку в сообщении: оно становится панелью,
// а прежняя панель (если это было другое сообщение) удаляется.
func (a *App) adopt(ctx context.Context, b *bot.Bot, chat int64, msg int) {
	a.deleteMsgs(ctx, b, chat, a.panels.swap(chat, msg, nil))
}

// eat удаляет сообщение пользователя (команду или ответ в диалоге), чтобы не засорять чат.
func (a *App) eat(ctx context.Context, b *bot.Bot, m *models.Message) {
	if m == nil {
		return
	}
	a.deleteMsgs(ctx, b, m.Chat.ID, []int{m.ID})
}
