package service

import (
	"context"
	"errors"
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
	store storage.Store
	now   func() time.Time
}

// NewPromos создаёт сервис промокодов.
func NewPromos(store storage.Store) *Promos { return &Promos{store: store, now: time.Now} }

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

// Stats остатки пула.
func (p *Promos) Stats(ctx context.Context) (domain.PromoStats, error) {
	return p.store.Repos().Promos.Stats(ctx)
}

// IssuedItem выданный код с именем получателя.
type IssuedItem struct {
	Code *domain.PromoCode
	User *domain.User
}

// Issued страница выданных (used=false) или использованных (used=true) кодов.
func (p *Promos) Issued(ctx context.Context, used bool, limit, offset int) ([]IssuedItem, int, error) {
	r := p.store.Repos()
	list, total, err := r.Promos.ListIssued(ctx, used, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]IssuedItem, 0, len(list))
	for _, c := range list {
		u, err := r.Users.Get(ctx, c.UserID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, 0, err
		}
		out = append(out, IssuedItem{Code: c, User: u})
	}
	return out, total, nil
}

// MarkUsed отмечает код использованным (вручную, админом).
func (p *Promos) MarkUsed(ctx context.Context, actor, id int64) (bool, error) {
	changed := false
	err := p.store.WithTx(ctx, func(r storage.Repos) error {
		now := p.now().UTC()
		ok, err := r.Promos.MarkUsed(ctx, id, actor, now)
		if err != nil || !ok {
			return err
		}
		changed = true
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "promo.used", Entity: "promo", EntityID: fmt.Sprint(id), At: now,
		})
	})
	return changed, err
}

// ByTask код задания (nil, если не выдан).
func (p *Promos) ByTask(ctx context.Context, taskID int64) (*domain.PromoCode, error) {
	c, err := p.store.Repos().Promos.ByTask(ctx, taskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return c, err
}
