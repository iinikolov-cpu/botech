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
	// Антидубль активных заданий пересоздан.
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET status='sent' WHERE id=7`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (scenario_id, scenario_version_id, user_id, status, due_days, created_by, created_at) VALUES (1,1,1,'created',3,1,1)`); err == nil {
		t.Fatal("уникальный индекс активных заданий должен работать после миграции")
	}
}
