// Пакет mailer отправляет письма по SMTP (коды подтверждения почты).
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Mailer отправляет одно текстовое письмо. Интерфейс позволяет подменять отправку в тестах.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Config настройки SMTP.
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	Security string // starttls, tls или none
	Timeout  time.Duration
}

// SMTP отправка через SMTP-сервер.
type SMTP struct{ cfg Config }

// NewSMTP создаёт отправитель.
func NewSMTP(cfg Config) *SMTP {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	return &SMTP{cfg: cfg}
}

// cleanHeader убирает переводы строк: иначе в заголовок можно подмешать лишние заголовки.
func cleanHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// encodeSubject кодирует тему по RFC 2047, чтобы русский текст дошёл без искажений.
func encodeSubject(s string) string {
	return mime.QEncoding.Encode("UTF-8", cleanHeader(s))
}

// Message собирает текст письма (заголовки и тело в UTF-8). Экспортируется для тестов.
func Message(from, to, subject, body string, now time.Time) []byte {
	var sb strings.Builder
	sb.WriteString("From: " + cleanHeader(from) + "\r\n")
	sb.WriteString("To: " + cleanHeader(to) + "\r\n")
	sb.WriteString("Subject: " + encodeSubject(subject) + "\r\n")
	sb.WriteString("Date: " + now.UTC().Format(time.RFC1123Z) + "\r\n")
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	sb.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	sb.WriteString("\r\n")
	return []byte(sb.String())
}

// Send отправляет письмо. Адрес получателя проверяется, в тексте ошибок нет пароля.
func (m *SMTP) Send(ctx context.Context, to, subject, body string) error {
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("неверный адрес получателя: %w", err)
	}
	from, err := mail.ParseAddress(m.cfg.From)
	if err != nil {
		return fmt.Errorf("неверный адрес отправителя: %w", err)
	}
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))

	dctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("подключение к почтовому серверу: %w", err)
	}
	if dl, ok := dctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}
	if m.cfg.Security == "tls" {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("приветствие почтового сервера: %w", err)
	}
	defer c.Close()

	if m.cfg.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("почтовый сервер не поддерживает STARTTLS (проверьте SMTP_SECURITY и SMTP_PORT)")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if m.cfg.User != "" {
		// PlainAuth отказывается отправлять пароль по незашифрованному соединению (кроме localhost).
		if err := c.Auth(smtp.PlainAuth("", m.cfg.User, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("вход на почтовый сервер не удался (проверьте SMTP_USER и SMTP_PASSWORD): %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("адрес отправителя отклонён: %w", err)
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("адрес получателя отклонён: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(Message(m.cfg.From, rcpt.Address, subject, body, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
