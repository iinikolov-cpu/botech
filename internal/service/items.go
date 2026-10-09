package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Ограничения при загрузке айтемов.
const (
	MaxItemBatch = 2000
	MaxItemTitle = 100
	MaxItemURL   = 500
	MaxItemNote  = 200
	MaxItemPrice = 1_000_000_000
)

// itemURLRe ссылка в строке загрузки. Запятая и точка с запятой разделяют колонки, поэтому в ссылку не входят.
var itemURLRe = regexp.MustCompile(`https?://[^\s;,|"'<>]+`)

// itemHeaderWords слова, по которым первая строка без ссылки считается заголовком таблицы.
var itemHeaderWords = []string{"название", "ссылка", "title", "name", "url", "link"}

// ParseItems разбирает текст или CSV: строка = «название; ссылка; цена; заметка». Колонки можно
// разделять точкой с запятой, запятой, табуляцией или «|». Ссылка находится в строке по http(s)://,
// всё до неё считается названием, после неё необязательные цена и заметка.
// Возвращает айтемы и непрошедшие проверку строки (для показа админу).
func ParseItems(raw string) (items []*domain.Item, invalid []string) {
	raw = strings.TrimPrefix(raw, "\xef\xbb\xbf") // BOM из Excel/Блокнота
	first := true
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		wasFirst := first
		first = false
		loc := itemURLRe.FindStringIndex(line)
		if loc == nil {
			low := strings.ToLower(line)
			header := false
			for _, w := range itemHeaderWords {
				if strings.Contains(low, w) {
					header = true
				}
			}
			if !(wasFirst && header) {
				invalid = append(invalid, shortLine(line))
			}
			continue
		}
		title := trimField(line[:loc[0]])
		link := line[loc[0]:loc[1]]
		fields := strings.FieldsFunc(line[loc[1]:], func(r rune) bool { return r == ';' || r == ',' || r == '|' || r == '\t' })

		it := &domain.Item{Title: title, URL: link}
		var noteParts []string
		for i, f := range fields {
			f = trimField(f)
			if f == "" {
				continue
			}
			if i == 0 {
				if p, ok := parsePrice(f); ok {
					it.Price = p
					continue
				}
			}
			noteParts = append(noteParts, f)
		}
		it.Note = strings.Join(noteParts, ", ")

		u, err := url.Parse(link)
		switch {
		case title == "":
			invalid = append(invalid, shortLine(line)+" (нет названия)")
		case err != nil || u.Host == "":
			invalid = append(invalid, shortLine(line)+" (неверная ссылка)")
		case utf8.RuneCountInString(title) > MaxItemTitle:
			invalid = append(invalid, shortLine(line)+fmt.Sprintf(" (название длиннее %d символов)", MaxItemTitle))
		case len(link) > MaxItemURL:
			invalid = append(invalid, shortLine(line)+" (слишком длинная ссылка)")
		case utf8.RuneCountInString(it.Note) > MaxItemNote:
			invalid = append(invalid, shortLine(line)+fmt.Sprintf(" (заметка длиннее %d символов)", MaxItemNote))
		case it.Price > MaxItemPrice:
			invalid = append(invalid, shortLine(line)+" (слишком большая цена)")
		default:
			items = append(items, it)
		}
	}
	return items, invalid
}

// trimField убирает пробелы, кавычки и разделители по краям колонки.
func trimField(s string) string {
	return strings.Trim(s, " \t \"';,|")
}

// parsePrice цена: целое число, пробелы и точки как разделители тысяч допускаются.
func parsePrice(s string) (int64, bool) {
	s = strings.NewReplacer(" ", "", " ", "", ".", "").Replace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func shortLine(s string) string {
	r := []rune(s)
	if len(r) > 50 {
		return string(r[:49]) + "…"
	}
	return s
}

// ParseIDs разбирает список номеров: «3, 5 #7» (запятые, пробелы, точки с запятой, переносы строк).
func ParseIDs(raw string) (ids []int64, invalid []string) {
	seen := map[int64]bool{}
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' ' }) {
		n, err := strconv.ParseInt(strings.TrimPrefix(tok, "#"), 10, 64)
		if err != nil || n < 1 {
			invalid = append(invalid, tok)
			continue
		}
		if !seen[n] {
			seen[n] = true
			ids = append(ids, n)
		}
	}
	return ids, invalid
}

// Items управляет пулом айтемов и выбором айтема покупателем.
type Items struct {
	store storage.Store
	now   func() time.Time
}

// NewItems создаёт сервис айтемов.
func NewItems(store storage.Store) *Items { return &Items{store: store, now: time.Now} }

// ItemAddResult итог загрузки айтемов.
type ItemAddResult struct {
	Added      int
	Duplicates int // ссылка уже есть в пуле или повторяется в загрузке
	Invalid    []string
}

// Add загружает айтемы в пул.
func (s *Items) Add(ctx context.Context, actor int64, raw string) (*ItemAddResult, error) {
	items, invalid := ParseItems(raw)
	if len(items) > MaxItemBatch {
		return nil, fmt.Errorf("%w: за один раз можно загрузить не больше %d айтемов", ErrForbidden, MaxItemBatch)
	}
	seen := make(map[string]bool, len(items))
	unique := make([]*domain.Item, 0, len(items))
	for _, it := range items {
		if !seen[it.URL] {
			seen[it.URL] = true
			unique = append(unique, it)
		}
	}
	res := &ItemAddResult{Invalid: invalid}
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		now := s.now().UTC()
		n, err := r.Items.AddBatch(ctx, unique, actor, now)
		if err != nil {
			return err
		}
		res.Added, res.Duplicates = n, len(items)-n
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "item.add", Entity: "item", Details: fmt.Sprintf("добавлено %d", n), At: now,
		})
	})
	return res, err
}

// Stats сводка по пулу.
func (s *Items) Stats(ctx context.Context) (domain.ItemStats, error) {
	return s.store.Repos().Items.Stats(ctx)
}

// List страница всех айтемов для таблицы админа.
func (s *Items) List(ctx context.Context, limit, offset int) ([]*domain.Item, int, error) {
	return s.store.Repos().Items.List(ctx, limit, offset)
}

// ListFree страница свободных айтемов для выбора покупателем.
func (s *Items) ListFree(ctx context.Context, limit, offset int) ([]*domain.Item, int, error) {
	return s.store.Repos().Items.ListFree(ctx, limit, offset)
}

// Get айтем по номеру.
func (s *Items) Get(ctx context.Context, id int64) (*domain.Item, error) {
	it, err := s.store.Repos().Items.Get(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrNotFound
	}
	return it, err
}

// ItemDeleteResult итог удаления айтемов.
type ItemDeleteResult struct {
	Deleted int
	Skipped int // не удалены: нет такого номера или айтем не свободен
	Invalid []string
}

// DeleteIDs удаляет свободные айтемы по номерам. Выбранные и купленные не удаляются.
func (s *Items) DeleteIDs(ctx context.Context, actor int64, raw string) (*ItemDeleteResult, error) {
	ids, invalid := ParseIDs(raw)
	res := &ItemDeleteResult{Invalid: invalid}
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		n, err := r.Items.DeleteFree(ctx, ids)
		if err != nil {
			return err
		}
		res.Deleted, res.Skipped = n, len(ids)-n
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "item.delete", Entity: "item", Details: fmt.Sprintf("удалено %d", n), At: s.now().UTC(),
		})
	})
	return res, err
}

// DeleteAllFree удаляет все свободные айтемы и возвращает их число.
func (s *Items) DeleteAllFree(ctx context.Context, actor int64) (int, error) {
	n := 0
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		var err error
		if n, err = r.Items.DeleteAllFree(ctx); err != nil {
			return err
		}
		return r.Audit.Add(ctx, &domain.AuditEntry{
			AdminID: actor, Action: "item.delete_all_free", Entity: "item", Details: fmt.Sprintf("удалено %d", n), At: s.now().UTC(),
		})
	})
	return n, err
}

// Choose покупатель выбирает айтем под своё принятое задание. Выбор окончательный: после него
// айтем исчезает из списка свободных, а выбрать другой нельзя (до отмены задания).
func (s *Items) Choose(ctx context.Context, userID, taskID, itemID int64) (*domain.Item, error) {
	var out *domain.Item
	err := s.store.WithTx(ctx, func(r storage.Repos) error {
		t, err := r.Tasks.Get(ctx, taskID)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && t.UserID != userID) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.Status != domain.TaskAccepted && t.Status != domain.TaskExpired {
			return fmt.Errorf("%w: товар выбирается после принятия задания", ErrForbidden)
		}
		if seller, err := isSellerTask(ctx, r, t); err != nil {
			return err
		} else if seller {
			return fmt.Errorf("%w: по заданию продавца товар не выбирается", ErrForbidden)
		}
		if _, err := r.Items.ByTask(ctx, taskID); err == nil {
			return fmt.Errorf("%w: товар уже выбран", ErrForbidden)
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		it, err := r.Items.Get(ctx, itemID)
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("%w: такого товара нет", ErrForbidden)
		}
		if err != nil {
			return err
		}
		now := s.now().UTC()
		ok, err := r.Items.Reserve(ctx, itemID, taskID, now)
		if errors.Is(err, storage.ErrDuplicate) {
			return fmt.Errorf("%w: товар уже выбран", ErrForbidden)
		}
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: этот товар уже выбрали, выберите другой", ErrForbidden)
		}
		it.Status, it.TaskID, it.ReservedAt = domain.ItemReserved, taskID, now
		out = it
		return r.Tasks.AddEvent(ctx, &domain.TaskEvent{
			TaskID: taskID, Kind: "item_chosen", Details: fmt.Sprintf("выбран товар #%d «%s»", it.ID, it.Title), ActorID: userID, At: now,
		})
	})
	return out, err
}
