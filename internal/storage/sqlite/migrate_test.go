package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// Обновление существующей базы: данные заданий и связанных таблиц переживают
// пересоздание таблицы tasks, а новый статус становится доступен.
func TestMigrationKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// Открываем «старую» базу: только миграции 0001-0003.
	old, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.applyMigrations(ctx, 3); err != nil {
		t.Fatal(err)
	}
	db := old.DB()
	stmts := []string{
		`INSERT INTO users (tg_id, role, status, created_at, updated_at) VALUES (1,'buyer','active',1,1)`,
		`INSERT INTO scenarios (id, key, title, operator, created_by, created_at, updated_at) VALUES (1,'k','T','O',1,1,1)`,
		`INSERT INTO scenario_versions (id, scenario_id, version, body, created_by, created_at) VALUES (1,1,1,'{}',1,1)`,
		`INSERT INTO tasks (id, scenario_id, scenario_version_id, user_id, status, due_days, created_by, created_at)
		 VALUES (7,1,1,1,'accepted',3,1,1)`,
		`INSERT INTO task_events (task_id, kind, at) VALUES (7,'created',1)`,
		`INSERT INTO reports (id, task_id, user_id, submitted_at) VALUES (1,7,1,1)`,
		`INSERT INTO compensations (task_id, user_id, amount, receipt_file_id, receipt_unique_id, created_at) VALUES (7,1,500,'f','u',1)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// В старой схеме статуса cancelled ещё нет.
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status='cancelled' WHERE id=7`); err == nil {
		t.Fatal("до миграции статус cancelled должен быть запрещён")
	}

	// Применяем остальные миграции (обновление).
	if err := old.applyMigrations(ctx, 0); err != nil {
		t.Fatal(err)
	}

	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = 7`).Scan(&status); err != nil || status != "accepted" {
		t.Fatalf("задание потеряно или изменено: %q %v", status, err)
	}
	for table, want := range map[string]int{"task_events": 1, "reports": 1, "compensations": 1} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil || n != want {
			t.Fatalf("%s: %d записей, ожидали %d (%v)", table, n, want, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status='cancelled' WHERE id=7`); err != nil {
		t.Fatalf("после миграции cancelled должен быть разрешён: %v", err)
	}
	// Внешние ключи снова включены на соединениях пула.
	if _, err := db.ExecContext(ctx, `INSERT INTO task_events (task_id, kind, at) VALUES (999,'x',1)`); err == nil {
		t.Fatal("внешние ключи должны быть включены после миграции")
	}
	// Повторное назначение того же сценария тому же покупателю разрешено (миграция 0006).
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status='sent' WHERE id=7`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (scenario_id, scenario_version_id, user_id, status, due_days, created_by, created_at) VALUES (1,1,1,'created',3,1,1)`); err != nil {
		t.Fatalf("повторное назначение должно быть разрешено: %v", err)
	}
	// Новый статус и версии отчёта: отчёт из старой схемы получил версию 1.
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status='rework' WHERE id=7`); err != nil {
		t.Fatalf("статус rework должен быть разрешён: %v", err)
	}
	var rev int
	if err := db.QueryRowContext(ctx, `SELECT revision FROM reports WHERE task_id = 7`).Scan(&rev); err != nil || rev != 1 {
		t.Fatalf("версия старого отчёта: %d %v", rev, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE compensations SET status='rejected' WHERE task_id = 7`); err != nil {
		t.Fatalf("статус rejected должен быть разрешён: %v", err)
	}
	// Номера заданий продолжаются после максимального (8 = следующий за вставленным в тесте).
	var next int64
	if err := db.QueryRowContext(ctx, `SELECT MAX(id) FROM tasks`).Scan(&next); err != nil || next < 8 {
		t.Fatalf("нумерация: %d %v", next, err)
	}
}

// Переход на многоразовые промокоды: свободные, выданные и использованные коды
// переносятся в новую схему с сохранением смысла.
func TestMigrationPromoReusable(t *testing.T) {
	ctx := context.Background()
	s, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.applyMigrations(ctx, 4); err != nil {
		t.Fatal(err)
	}
	db := s.DB()
	for _, q := range []string{
		`INSERT INTO promo_codes (code, status, added_by, created_at) VALUES ('FREE1','free',1,1)`,
		`INSERT INTO promo_codes (code, status, task_id, user_id, added_by, created_at, issued_at) VALUES ('ISSUED1','issued',5,9,1,1,10)`,
		`INSERT INTO promo_codes (code, status, task_id, user_id, added_by, created_at, issued_at, used_at) VALUES ('USED1','used',6,9,1,1,10,20)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := s.applyMigrations(ctx, 0); err != nil {
		t.Fatal(err)
	}

	type row struct {
		used   int
		active int64
	}
	got := map[string]row{}
	rows, err := db.QueryContext(ctx, `SELECT code, used_count, active_task_id FROM promo_codes`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		var r row
		if err := rows.Scan(&c, &r.used, &r.active); err != nil {
			t.Fatal(err)
		}
		got[c] = r
	}
	rows.Close()
	want := map[string]row{"FREE1": {0, 0}, "ISSUED1": {0, 5}, "USED1": {1, 0}}
	for c, w := range want {
		if got[c] != w {
			t.Errorf("%s: получили %+v, ожидали %+v", c, got[c], w)
		}
	}
	var outcome string
	if err := db.QueryRowContext(ctx, `SELECT outcome FROM promo_assignments WHERE task_id = 6`).Scan(&outcome); err != nil || outcome != "used" {
		t.Errorf("история выдачи использованного кода: %q %v", outcome, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT outcome FROM promo_assignments WHERE task_id = 5`).Scan(&outcome); err != nil || outcome != "active" {
		t.Errorf("история выдачи активного кода: %q %v", outcome, err)
	}
}

// Миграция 0008: старые сценарии остаются покупательскими, у пользователей типы не выбраны,
// версии без поля kind читаются как «покупатель», таблица айтемов работает.
func TestMigrationKindsAndItems(t *testing.T) {
	ctx := context.Background()
	s, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.applyMigrations(ctx, 7); err != nil {
		t.Fatal(err)
	}
	db := s.DB()
	for _, q := range []string{
		`INSERT INTO users (tg_id, role, status, created_at, updated_at) VALUES (1,'buyer','active',1,1)`,
		`INSERT INTO scenarios (id, key, title, operator, created_by, created_at, updated_at) VALUES (1,'k','T','O',1,1,1)`,
		`INSERT INTO scenario_versions (id, scenario_id, version, body, created_by, created_at) VALUES (1,1,1,'{"title":"T","operator":"O"}',1,1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := s.applyMigrations(ctx, 0); err != nil {
		t.Fatal(err)
	}

	r := s.Repos()
	u, err := r.Users.Get(ctx, 1)
	if err != nil || len(u.Kinds) != 0 {
		t.Fatalf("у старого пользователя типы не выбраны: %v %v", u, err)
	}
	sc, err := r.Scenarios.GetByID(ctx, 1)
	if err != nil || sc.Kind != "buyer" {
		t.Fatalf("старый сценарий покупательский: %v %v", sc, err)
	}
	v, err := r.Scenarios.LatestVersion(ctx, 1)
	if err != nil || v.Body.Kind != "buyer" {
		t.Fatalf("старая версия читается как покупатель: %v %v", v, err)
	}
	// Одно задание: не больше одного айтема; свободные айтемы (task_id = 0) не конфликтуют.
	for _, q := range []string{
		`INSERT INTO items (title, url, added_by, created_at) VALUES ('a','https://x/1',1,1)`,
		`INSERT INTO items (title, url, added_by, created_at) VALUES ('b','https://x/2',1,1)`,
		`UPDATE items SET task_id = 5, status = 'reserved' WHERE url = 'https://x/1'`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE items SET task_id = 5, status = 'reserved' WHERE url = 'https://x/2'`); err == nil {
		t.Fatal("два айтема на одно задание должны быть запрещены")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO items (title, url, added_by, created_at) VALUES ('dup','https://x/1',1,1)`); err == nil {
		t.Fatal("дубль ссылки должен быть запрещён")
	}
}
