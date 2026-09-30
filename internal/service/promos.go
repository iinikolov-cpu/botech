package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

// MaxPromoBatch ограничение на число кодов за одну загрузку.
const MaxPromoBatch = 5000

var promoRe = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)

// promoHeaders слова, которые считаем заголовком таблицы, а не кодом.
var promoHeaders = map[string]bool{"code": true, "codes": true, "promo": true, "promocode": true, "промокод": true, "код": true}

// ParseCodes разбирает текст или CSV: один код в строке. Если в строке несколько колонок
// (через запятую, точку с запятой или табуляцию), берётся первая. Пробелы внутри строки
// разделяют коды (удобно при вставке списка в одну строку).
func ParseCodes(raw string) (valid, invalid []string) {
	raw = strings.TrimPrefix(raw, "\xef\xbb\xbf") // BOM из Excel/Блокнота
	first := true
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field := line
		if i := strings.IndexAny(line, ",;\t"); i >= 0 {
			field = line[:i]
		}
		for _, tok := range strings.Fields(strings.Trim(field, `"' `)) {
			tok = strings.Trim(tok, `"'`)
			if tok == "" {
				continue
			}
			if first && promoHeaders[strings.ToLower(tok)] {
				first = false
				continue
			}
			first = false
			if promoRe.MatchString(tok) {
				valid = append(valid, tok)
			} else {
				invalid = append(invalid, tok)
			}
		}
	}
	return valid, invalid
}

// Promos управляет пулом промокодов.
type Promos struct {
	store   storage.Store
	now     func() time.Time
	maxUses int // сколько раз можно использовать каждый код
}

// NewPromos создаёт сервис промокодов. maxUses: сколько раз можно использовать каждый код.
func NewPromos(store storage.Store, maxUses int) *Promos {
	return &Promos{store: store, now: time.Now, maxUses: maxUses}
}

// MaxUses лимит использований одного кода.
func (p *Promos) MaxUses() int { return p.maxUses }

// AddResult итог загрузки кодов.
type AddResult struct {
	Added      int
	Duplicates int // уже были в пуле или повторяются в загрузке
	Invalid    []string
}

// Add загружает коды в пул.
func (p *Promos) Add(ctx context.Context, actor int64, raw string) (*AddResult, error) {
	valid, invalid := ParseCodes(raw)
	if len(valid) > MaxPromoBatch {
		return nil, fmt.Errorf("%w: за один раз можно загрузить не больше %d кодов", ErrForbidden, MaxPromoBatch)
	}
	seen := make(map[string]bool, len(valid))
	unique := make([]string, 0, len(valid))
	for _, c := range valid {
		if !seen[c] {
			seen[c] = true
			unique = append(unique, c)
		}
	}
	res := &AddResult{Invalid: invalid}
	err := p.store.WithTx(ctx, func(r storage.Repos) error {
		now := p.now().UTC()
		n, err := r.Promos.AddBatch(ctx, unique, actor, now)
		if err != nil {
			return err
		}
		res.Added = n
		res.Duplicates = len(valid) - n
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "promo.add", Entity: "promo", Details: fmt.Sprintf("добавлено %d", n), At: now,
		})
	})
	return res, err
}

// Stats сводка по пулу.
func (p *Promos) Stats(ctx context.Context) (domain.PromoStats, error) {
	return p.store.Repos().Promos.Stats(ctx, p.maxUses)
}

// Row строка таблицы кодов.
type Row struct {
	Code      string
	UsedCount int
	Left      int   // сколько раз код ещё можно использовать
	TaskID    int64 // активное задание (0, если код свободен)
}

// List страница таблицы кодов с общим числом.
func (p *Promos) List(ctx context.Context, limit, offset int) ([]Row, int, error) {
	list, total, err := p.store.Repos().Promos.List(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Row, 0, len(list))
	for _, c := range list {
		left := p.maxUses - c.UsedCount
		if left < 0 {
			left = 0
		}
		out = append(out, Row{Code: c.Code, UsedCount: c.UsedCount, Left: left, TaskID: c.ActiveTaskID})
	}
	return out, total, nil
}

// DeleteResult итог удаления кодов.
type DeleteResult struct {
	Deleted int
	Skipped int // не удалены: нет в пуле или заняты активным заданием
	Invalid []string
}

// DeleteFree удаляет из пула коды из списка (по одному или пачкой). Коды, закреплённые за
// активными заданиями, не удаляются: их сначала должны завершить. История выдач сохраняется.
func (p *Promos) DeleteFree(ctx context.Context, actor int64, raw string) (*DeleteResult, error) {
	valid, invalid := ParseCodes(raw)
	seen := make(map[string]bool, len(valid))
	unique := make([]string, 0, len(valid))
	for _, c := range valid {
		if !seen[c] {
			seen[c] = true
			unique = append(unique, c)
		}
	}
	res := &DeleteResult{Invalid: invalid}
	err := p.store.WithTx(ctx, func(r storage.Repos) error {
		n, err := r.Promos.DeleteFree(ctx, unique)
		if err != nil {
			return err
		}
		res.Deleted, res.Skipped = n, len(unique)-n
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "promo.delete", Entity: "promo", Details: fmt.Sprintf("удалено %d", n), At: p.now().UTC(),
		})
	})
	return res, err
}

// DeleteAllFree удаляет все коды, не занятые активными заданиями, и возвращает их число.
func (p *Promos) DeleteAllFree(ctx context.Context, actor int64) (int, error) {
	n := 0
	err := p.store.WithTx(ctx, func(r storage.Repos) error {
		var err error
		if n, err = r.Promos.DeleteAllFree(ctx); err != nil {
			return err
		}
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "promo.delete_all_free", Entity: "promo", Details: fmt.Sprintf("удалено %d", n), At: p.now().UTC(),
		})
	})
	return n, err
}
