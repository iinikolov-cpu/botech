package service

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"botech/internal/storage"
)

// MaxTelegramFile предел размера файла, который бот может отправить в Telegram (50 МБ), с запасом.
const MaxTelegramFile = 49 << 20

// Backups резервные копии базы: согласованный снимок, сжатие, хранение последних копий.
type Backups struct {
	store storage.Store
	dir   string
	keep  int
	now   func() time.Time
}

// NewBackups создаёт сервис. dir куда складывать копии, keep сколько последних хранить.
func NewBackups(store storage.Store, dir string, keep int) *Backups {
	if keep < 1 {
		keep = 7
	}
	return &Backups{store: store, dir: dir, keep: keep, now: time.Now}
}

// BackupFile созданная копия.
type BackupFile struct {
	Path string
	Size int64
}

// Create делает снимок базы (VACUUM INTO, бот при этом продолжает работать), сжимает его gzip
// и удаляет старые копии сверх лимита.
func (b *Backups) Create(ctx context.Context) (*BackupFile, error) {
	if err := os.MkdirAll(b.dir, 0o750); err != nil {
		return nil, fmt.Errorf("создание папки бэкапов: %w", err)
	}
	stamp := b.now().UTC().Format("20060102-150405")
	tmp := filepath.Join(b.dir, "snapshot-"+stamp+".tmp")
	defer os.Remove(tmp)
	if err := b.store.Backup(ctx, tmp); err != nil {
		return nil, fmt.Errorf("снимок базы: %w", err)
	}

	final := filepath.Join(b.dir, "bot-"+stamp+".db.gz")
	for i := 1; fileExists(final); i++ { // две копии в одну секунду не должны затирать друг друга
		final = filepath.Join(b.dir, fmt.Sprintf("bot-%s-%d.db.gz", stamp, i))
	}
	if err := gzipFile(tmp, final); err != nil {
		_ = os.Remove(final)
		return nil, err
	}
	info, err := os.Stat(final)
	if err != nil {
		return nil, err
	}
	b.rotate()
	return &BackupFile{Path: final, Size: info.Size()}, nil
}

// rotate оставляет только последние keep копий.
func (b *Backups) rotate() {
	files, err := filepath.Glob(filepath.Join(b.dir, "bot-*.db.gz"))
	if err != nil {
		return
	}
	sort.Strings(files) // имена содержат время, поэтому порядок по имени это порядок по времени
	for len(files) > b.keep {
		_ = os.Remove(files[0])
		files = files[1:]
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func gzipFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		return err
	}
	return zw.Close()
}

// IsBackupName true для имён файлов наших копий.
func IsBackupName(name string) bool {
	return strings.HasPrefix(name, "bot-") && strings.HasSuffix(name, ".db.gz")
}
