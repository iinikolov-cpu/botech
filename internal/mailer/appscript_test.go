package mailer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppsScriptSend(t *testing.T) {
	const secret = "supersecretvalue-123456"
	var got map[string]string
	// Как у Google: POST на /exec выполняет скрипт и отвечает редиректом, результат отдаётся по новому адресу.
	result := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer result.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			http.Redirect(w, r, result.URL+"/echo", http.StatusFound)
		}
	}))
	defer redir.Close()

	m := NewAppsScript(redir.URL+"/exec", secret, "Бот")
	if err := m.Send(context.Background(), "a@dila-team.com", "Код\r\nBcc: x@y.z", "Ваш код 123456"); err != nil {
		t.Fatal(err)
	}
	if got["secret"] != secret || got["to"] != "a@dila-team.com" || got["body"] != "Ваш код 123456" || got["name"] != "Бот" {
		t.Fatalf("тело запроса: %v", got)
	}
	if strings.ContainsAny(got["subject"], "\r\n") {
		t.Fatalf("перевод строки в теме: %q", got["subject"])
	}
}

func TestAppsScriptErrors(t *testing.T) {
	const secret = "supersecretvalue-123456"
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"отказ":     {200, `{"ok":false,"error":"домен не разрешён"}`, "домен не разрешён"},
		"не JSON":   {200, `<html>Войдите</html>`, "не JSON"},
		"статус":    {403, `forbidden`, "403"},
		"длинный":   {200, `{"ok":false,"error":"` + strings.Repeat("я", 500) + `"}`, "…"},
		"без ok":    {200, `{}`, "отклонил"},
		"пустой":    {200, ``, "не JSON"},
		"ошибка js": {200, `{"ok":false,"error":"a\r\nb"}`, "отклонил"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			err := NewAppsScript(srv.URL+"/exec", secret, "Бот").Send(context.Background(), "a@dila-team.com", "s", "b")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ожидалась ошибка с %q: %v", c.want, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), srv.URL) {
				t.Fatalf("в ошибке утечка секрета или адреса: %v", err)
			}
		})
	}
}

func TestAppsScriptUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/exec"
	srv.Close()
	err := NewAppsScript(url, "supersecretvalue-123456", "Бот").Send(context.Background(), "a@dila-team.com", "s", "b")
	if err == nil || strings.Contains(err.Error(), url) {
		t.Fatalf("ошибка без адреса: %v", err)
	}
}
