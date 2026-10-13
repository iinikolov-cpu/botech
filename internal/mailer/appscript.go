package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AppsScript отправка писем через веб-приложение Google Apps Script (MailApp.sendEmail).
// Бот делает POST с JSON {secret, to, subject, body, name}; скрипт проверяет секрет и адрес
// получателя и отправляет письмо от имени аккаунта, под которым он развёрнут.
// Так не нужен ни SMTP-доступ, ни пароль приложения Google Workspace.
type AppsScript struct {
	url     string
	secret  string
	name    string
	client  *http.Client
	timeout time.Duration
}

// NewAppsScript создаёт отправитель. url адрес развёртывания (…/exec), secret общий секрет из свойств скрипта,
// name имя отправителя в письме.
func NewAppsScript(url, secret, name string) *AppsScript {
	return &AppsScript{url: url, secret: secret, name: name, timeout: 25 * time.Second, client: &http.Client{}}
}

// scriptReply ответ скрипта.
type scriptReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Send отправляет письмо. В тексте ошибок нет секрета и адреса развёртывания.
func (m *AppsScript) Send(ctx context.Context, to, subject, body string) error {
	payload, err := json.Marshal(map[string]string{
		"secret": m.secret, "to": to, "subject": cleanHeader(subject), "body": body, "name": m.name,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(payload))
	if err != nil {
		return errors.New("неверный адрес скрипта отправки")
	}
	// text/plain: так Google принимает запрос без предварительной проверки (preflight).
	req.Header.Set("Content-Type", "text/plain;charset=utf-8")
	resp, err := m.client.Do(req)
	if err != nil {
		// Не пробрасываем err как есть: в нём адрес развёртывания.
		if ctx.Err() != nil {
			return errors.New("скрипт отправки не ответил вовремя")
		}
		return errors.New("не удалось обратиться к скрипту отправки")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("скрипт отправки ответил статусом %d (проверьте, что развёртывание доступно «Всем» и адрес оканчивается на /exec)", resp.StatusCode)
	}
	var r scriptReply
	if err := json.Unmarshal(data, &r); err != nil {
		// Вместо JSON Google возвращает страницу входа или ошибки: развёртывание закрыто или устарело.
		return errors.New("скрипт отправки вернул не JSON (проверьте доступ «Всем» и что развёртывание обновлено после правки кода)")
	}
	if !r.OK {
		return fmt.Errorf("скрипт отправки отклонил письмо: %s", sanitize(r.Error))
	}
	return nil
}

// sanitize ограничивает текст ошибки от скрипта.
func sanitize(s string) string {
	s = cleanHeader(s)
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
