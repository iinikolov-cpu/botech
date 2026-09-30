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
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("миграции: %w", err)
	}
	return s, nil
}

// DB отдаёт *sql.DB (нужно для бэкапа и тестов).
func (s *Store) DB() *sql.DB { return s.db }

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
	}
}

// migrate применяет встроенные SQL-файлы по порядку имён, каждый один раз.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
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

	for _, name := range names {
		var n int
		if err := s.db.QueryRowContext(ctx,
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
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
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
