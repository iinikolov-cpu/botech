package service

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
	"botech/internal/storage/sqlite"
)

// restore распаковывает копию и открывает её как обычную базу бота.
func restore(t *testing.T, gz string) *sqlite.Store {
	t.Helper()
	in, err := os.Open(gz)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored.db")
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	out.Close()
	st, err := sqlite.Open(dst)
	if err != nil {
		t.Fatalf("копия должна открываться как база бота: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestBackupCreateAndRestore(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sc := e.importScenario(t, scenarioYAML).Scenario
	e.assignOne(t, sc.ID, 1)

	b := NewBackups(e.store, filepath.Join(t.TempDir(), "backups"), 7)
	f, err := b.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size <= 0 || filepath.Ext(f.Path) != ".gz" || !IsBackupName(filepath.Base(f.Path)) {
		t.Fatalf("файл бэкапа: %+v", f)
	}
	// Временные файлы не остаются.
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(f.Path), "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("остались временные файлы: %v", left)
	}

	// Копия содержит данные и открывается штатно (миграции на ней уже применены).
	st := restore(t, f.Path)
	u, err := st.Repos().Users.Get(ctx, firstAdmin)
	if err != nil || !u.IsAdmin() {
		t.Fatalf("в копии нет первого админа: %v", err)
	}
	if n, _ := st.Repos().Tasks.Count(ctx, storage.TaskFilter{}); n != 1 {
		t.Fatalf("в копии %d заданий, ожидали 1", n)
	}
}

// Бэкап делается на работающей базе: параллельные записи не портят копию.
func TestBackupWhileWriting(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	b := NewBackups(e.store, filepath.Join(t.TempDir(), "backups"), 7)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1000); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			now := time.Now().UTC()
			_ = e.store.Repos().Users.Create(ctx, &domain.User{
				TgID: i, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru", CreatedAt: now, UpdatedAt: now,
			})
		}
	}()
	var files []*BackupFile
	for i := 0; i < 3; i++ {
		f, err := b.Create(ctx)
		if err != nil {
			close(stop)
			t.Fatal(err)
		}
		files = append(files, f)
	}
	close(stop)
	wg.Wait()

	for _, f := range files {
		st := restore(t, f.Path)
		var res string
		if err := st.DB().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
			t.Fatalf("целостность копии %s: %q %v", f.Path, res, err)
		}
	}
}

// Хранятся только последние копии.
func TestBackupRotation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "backups")
	b := NewBackups(e.store, dir, 3)
	clk := &clock{t: time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)}
	b.now = clk.Now

	var last *BackupFile
	for i := 0; i < 5; i++ {
		f, err := b.Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		last = f
		clk.Add(24 * time.Hour)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "bot-*.db.gz"))
	if len(files) != 3 {
		t.Fatalf("осталось %d копий, ожидали 3: %v", len(files), files)
	}
	if _, err := os.Stat(last.Path); err != nil {
		t.Fatal("самая свежая копия должна остаться")
	}
	// Две копии в одну секунду не затирают друг друга.
	clk.Set(time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	b.keep = 10
	f1, _ := b.Create(ctx)
	f2, err := b.Create(ctx)
	if err != nil || f1.Path == f2.Path {
		t.Fatalf("одинаковые имена: %s %s %v", f1.Path, f2.Path, err)
	}
	_ = fmt.Sprint
}
