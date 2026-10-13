package mailer

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMessage(t *testing.T) {
	msg := string(Message("Бот <bot@dila-team.com>", "a@dila-team.com", "Код: 123456", "Привет\nкод 123456", time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)))
	for _, want := range []string{"To: a@dila-team.com\r\n", "Content-Type: text/plain; charset=UTF-8\r\n", "MIME-Version: 1.0\r\n", "\r\n\r\nПривет\r\nкод 123456\r\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в письме нет %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "Subject: ") || strings.Contains(msg, "Subject: Код") {
		t.Errorf("тема с русским текстом должна быть закодирована (RFC 2047):\n%s", msg)
	}
}

// Переводы строк в заголовках не позволяют подмешать лишние заголовки (например, скрытую копию).
func TestMessageHeaderInjection(t *testing.T) {
	msg := string(Message("bot@dila-team.com", "a@dila-team.com\r\nBcc: evil@evil.com", "Тема\r\nBcc: evil@evil.com", "текст", time.Now()))
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("внедрён заголовок: %q\n%s", line, head)
		}
	}
}

// fakeSMTP минимальный SMTP-сервер без шифрования: запоминает принятое письмо.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	from string
	rcpt []string
	data string
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := func(s string) { conn.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		inData := false
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					f.mu.Lock()
					f.data = data.String()
					f.mu.Unlock()
					w("250 queued")
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			up := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
				w("250 fake")
			case strings.HasPrefix(up, "MAIL FROM:"):
				f.mu.Lock()
				f.from = line
				f.mu.Unlock()
				w("250 ok")
			case strings.HasPrefix(up, "RCPT TO:"):
				f.mu.Lock()
				f.rcpt = append(f.rcpt, line)
				f.mu.Unlock()
				w("250 ok")
			case up == "DATA":
				inData = true
				w("354 go")
			case up == "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return f
}

func TestSMTPSend(t *testing.T) {
	srv := startFakeSMTP(t)
	host, port, _ := net.SplitHostPort(srv.ln.Addr().String())
	var p int
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	m := NewSMTP(Config{Host: host, Port: p, From: "Бот <bot@dila-team.com>", Security: "none", Timeout: 5 * time.Second})
	if err := m.Send(context.Background(), "emp@dila-team.com", "Код: 123456", "Ваш код 123456"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !strings.Contains(srv.from, "bot@dila-team.com") || len(srv.rcpt) != 1 || !strings.Contains(srv.rcpt[0], "emp@dila-team.com") {
		t.Fatalf("конверт: from=%q rcpt=%v", srv.from, srv.rcpt)
	}
	if !strings.Contains(srv.data, "Ваш код 123456") || !strings.Contains(srv.data, "To: emp@dila-team.com") {
		t.Fatalf("письмо: %q", srv.data)
	}
}

func TestSMTPErrors(t *testing.T) {
	m := NewSMTP(Config{Host: "127.0.0.1", Port: 1, From: "bot@dila-team.com", Security: "none", Timeout: time.Second})
	if err := m.Send(context.Background(), "не адрес", "т", "т"); err == nil {
		t.Fatal("неверный адрес получателя должен давать ошибку")
	}
	// Порт закрыт: понятная ошибка подключения, без пароля.
	m = NewSMTP(Config{Host: "127.0.0.1", Port: 1, User: "u", Password: "СЕКРЕТ", From: "bot@dila-team.com", Security: "starttls", Timeout: time.Second})
	err := m.Send(context.Background(), "a@dila-team.com", "т", "т")
	if err == nil || strings.Contains(err.Error(), "СЕКРЕТ") {
		t.Fatalf("ошибка подключения: %v", err)
	}
	if err := NewSMTP(Config{Host: "127.0.0.1", Port: 1, From: "мусор"}).Send(context.Background(), "a@dila-team.com", "т", "т"); err == nil {
		t.Fatal("неверный адрес отправителя должен давать ошибку")
	}
}
