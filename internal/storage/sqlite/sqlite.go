// Пакет sqlite реализует storage поверх SQLite (чистый Go, без CGO).
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // драйвер регистрируется как "sqlite"

	"botech/internal/storage"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// dbtx общий интерфейс для *sql.DB и *sql.Tx, чтобы репозитории работали и там и там.
type dbtx interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// Store реализация storage.Store.
type Store struct {
	db *sql.DB
}

var _ storage.Store = (*Store)(nil)

// Open открывает базу, включает WAL и применяет миграции.
func Open(path string) (*Store, error) {
	s, err := openRaw(path)
	if err != nil {
		return nil, err
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("миграции: %w", err)
	}
	return s, nil
}

// openRaw открывает базу с нужными настройками, но без применения миграций.
func openRaw(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("создание каталога БД: %w", err)
		}
	}
	// _txlock=immediate: транзакция сразу берёт блокировку записи,
	// поэтому конкурентные записи выстраиваются в очередь, а не падают.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	return &Store{db: db}, nil
}

// DB отдаёт *sql.DB (нужно для бэкапа и тестов).
func (s *Store) DB() *sql.DB { return s.db }

// Backup делает снимок через VACUUM INTO: SQLite сама собирает согласованную копию, пока бот
// продолжает работать. Копировать файл базы напрямую нельзя: при записи он может оказаться битым.
func (s *Store) Backup(ctx context.Context, dest string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest)
	return err
}

// Close закрывает базу.
func (s *Store) Close() error { return s.db.Close() }

// Repos репозитории вне транзакции.
func (s *Store) Repos() storage.Repos { return reposFor(s.db) }

// WithTx выполняет fn в транзакции.
func (s *Store) WithTx(ctx context.Context, fn func(r storage.Repos) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(reposFor(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func reposFor(q dbtx) storage.Repos {
	return storage.Repos{
		Users:     &userRepo{q},
		Invites:   &inviteRepo{q},
		Audit:     &auditRepo{q},
		Scenarios: &scenarioRepo{q},
		Tasks:     &taskRepo{q},
		FSM:       &fsmRepo{q},
		Promos:    &promoRepo{q},
		Reports:   &reportRepo{q},
		Comps:     &compRepo{q},
		Settings:  &settingsRepo{q},
		Reminders: &reminderRepo{q},
		Stats:     &statsRepo{q},
		Items:     &itemRepo{q},
	}
}

// migrate применяет все встроенные миграции.
func (s *Store) migrate(ctx context.Context) error { return s.applyMigrations(ctx, 0) }

// applyMigrations применяет встроенные SQL-файлы по порядку имён, каждый один раз.
// limit > 0 останавливается после файла с таким порядковым номером (нужно тестам обновления).
//
// Все миграции идут на одном соединении с отключёнными внешними ключами: так SQLite
// позволяет безопасно пересоздать таблицу (например, чтобы изменить CHECK). Перед
// возвратом ключи включаются обратно и проверяются.
func (s *Store) applyMigrations(ctx context.Context, limit int) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`) }()

	if _, err := conn.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if limit > 0 && limit < len(names) {
		names = names[:limit]
	}

	for _, name := range names {
		var n int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		// После миграции связи между таблицами должны остаться целыми.
		rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		broken := rows.Next()
		_ = rows.Close()
		if broken {
			_ = tx.Rollback()
			return fmt.Errorf("%s: нарушены внешние ключи после миграции", name)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%s','now'))`, name); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
