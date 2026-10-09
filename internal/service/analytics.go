package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Analytics статистика и выгрузки в CSV.
type Analytics struct {
	store storage.Store
	loc   *time.Location

	mu  sync.RWMutex
	bot string // username бота: нужен для ссылок на медиа в выгрузке
}

// NewAnalytics создаёт сервис. loc: часовой пояс для дат в выгрузках.
func NewAnalytics(store storage.Store, loc *time.Location) *Analytics {
	return &Analytics{store: store, loc: loc}
}

// SetBotUsername запоминает username бота (без @) для ссылок вида https://t.me/<бот>?start=...
func (a *Analytics) SetBotUsername(name string) {
	a.mu.Lock()
	a.bot = strings.TrimPrefix(strings.TrimSpace(name), "@")
	a.mu.Unlock()
}

func (a *Analytics) botName() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.bot
}

// OperatorStat итог по оператору.
type OperatorStat struct {
	Operator  string
	Issued    int
	Completed int
	Overdue   int
	AvgTime   time.Duration // от принятия до отчёта
}

// Stats статистика за период по одному типу заданий (покупатели или продавцы).
type Stats struct {
	Kind      domain.TaskKind
	Total     int // всего заданий
	Issued    int // доставлено исполнителям
	Accepted  int
	Declined  int
	Cancelled int
	InWork    int // принято или на доработке, отчёта ещё нет
	Waiting   int // отправлено, ответа нет
	Overdue   int // просрочено сейчас или сдано после срока
	Completed int // есть отчёт
	Reviewed  int // проверено админом
	AvgTime   time.Duration
	Operators []OperatorStat

	// Только для покупателей.
	ItemsChosen                         int // выбран айтем
	ItemsBought                         int // айтем куплен (отчёт отправлен)
	CompPending, CompPaid, CompRejected int
	SumPending, SumPaid                 int64
}

// Rows выборка заданий для выгрузок и статистики.
func (a *Analytics) Rows(ctx context.Context, f storage.ExportFilter) ([]storage.ExportRow, error) {
	return a.store.Repos().Stats.Export(ctx, f)
}

// Stats считает сводку по заданиям периода для одного типа (покупатель или продавец).
func (a *Analytics) Stats(ctx context.Context, f storage.ExportFilter, kind domain.TaskKind) (*Stats, error) {
	rows, err := a.Rows(ctx, f)
	if err != nil {
		return nil, err
	}
	st := &Stats{Kind: kind}
	type opAcc struct {
		OperatorStat
		dur  time.Duration
		durN int
	}
	ops := map[string]*opAcc{}
	var totalDur time.Duration
	var totalN int

	for _, r := range rows {
		if r.Kind != kind {
			continue
		}
		st.Total++
		op := ops[r.Operator]
		if op == nil {
			op = &opAcc{OperatorStat: OperatorStat{Operator: r.Operator}}
			ops[r.Operator] = op
		}
		if !r.SentAt.IsZero() {
			st.Issued++
			op.Issued++
		}
		if !r.AcceptedAt.IsZero() {
			st.Accepted++
		}
		switch r.Status {
		case domain.TaskDeclined:
			st.Declined++
		case domain.TaskCancelled:
			st.Cancelled++
		case domain.TaskSent, domain.TaskCreated:
			st.Waiting++
		case domain.TaskAccepted, domain.TaskRework:
			st.InWork++
		case domain.TaskReviewed:
			st.Reviewed++
		}
		if r.Status == domain.TaskExpired || r.Late {
			st.Overdue++
			op.Overdue++
		}
		if r.Revision > 0 {
			st.Completed++
			op.Completed++
			if !r.AcceptedAt.IsZero() && !r.ReportedAt.IsZero() && r.ReportedAt.After(r.AcceptedAt) {
				d := r.ReportedAt.Sub(r.AcceptedAt)
				op.dur += d
				op.durN++
				totalDur += d
				totalN++
			}
		}
		if r.ItemURL != "" {
			st.ItemsChosen++
			if r.Revision > 0 {
				st.ItemsBought++
			}
		}
		switch r.CompStatus {
		case domain.CompPending:
			st.CompPending++
			st.SumPending += r.CompAmount
		case domain.CompPaid:
			st.CompPaid++
			st.SumPaid += r.CompAmount
		case domain.CompRejected:
			st.CompRejected++
		}
	}
	if totalN > 0 {
		st.AvgTime = totalDur / time.Duration(totalN)
	}
	for _, op := range ops {
		if op.durN > 0 {
			op.AvgTime = op.dur / time.Duration(op.durN)
		}
		st.Operators = append(st.Operators, op.OperatorStat)
	}
	sort.Slice(st.Operators, func(i, j int) bool { return st.Operators[i].Operator < st.Operators[j].Operator })
	return st, nil
}

// ---------- CSV ----------

// newCSV создаёт писатель: разделитель «;» и BOM, чтобы Excel открывал русский текст без настроек.
func newCSV() (*csv.Writer, *bytes.Buffer) {
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf")
	w := csv.NewWriter(&buf)
	w.Comma = ';'
	return w, &buf
}

// safe защищает от формул в Excel: ячейка, начинающаяся с = + - @, получает апостроф.
func safe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *Analytics) when(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(a.loc).Format("02.01.2006 15:04")
}

func yesNo(b bool) string {
	if b {
		return "да"
	}
	return ""
}

func buyerName(r storage.ExportRow) string {
	if r.UserName != "" {
		return r.UserName
	}
	return strconv.FormatInt(r.UserID, 10)
}

// kindTitle роль исполнителя в выгрузке («покупатель» или «продавец»).
func kindTitle(k domain.TaskKind) string {
	if !k.Valid() {
		return domain.KindBuyer.Title()
	}
	return k.Title()
}

var compTitle = map[domain.CompStatus]string{
	domain.CompPending: "к выплате", domain.CompPaid: "выплачено", domain.CompRejected: "отклонено",
}

// TasksCSV все задания периода: роль, Telegram ID, статусы, даты этапов, напоминания, айтем, промокод, компенсация.
func (a *Analytics) TasksCSV(rows []storage.ExportRow) []byte {
	w, buf := newCSV()
	_ = w.Write([]string{"№ задания", "Роль", "Telegram ID", "Имя", "Username", "Оператор", "Сценарий", "Версия сценария", "Статус",
		"Создано", "Отправлено", "Принято", "Срок до", "Отчёт", "Проверено", "Версий отчёта", "После срока", "Решение по отчёту",
		"Напоминаний", "Айтем", "Ссылка на айтем", "Промокод", "Сумма компенсации", "Статус компенсации", "Выплачено"})
	for _, r := range rows {
		_ = w.Write([]string{
			strconv.FormatInt(r.TaskID, 10), kindTitle(r.Kind), strconv.FormatInt(r.UserID, 10), safe(r.UserName), safe(r.Username),
			safe(r.Operator), safe(r.Title), strconv.Itoa(r.Version), r.Status.Title(),
			a.when(r.CreatedAt), a.when(r.SentAt), a.when(r.AcceptedAt), a.when(r.DueAt), a.when(r.ReportedAt), a.when(r.ReviewedAt),
			optInt(r.Revision), yesNo(r.Late), decisionTitle(r.Decision),
			optInt(r.Reminders), safe(r.ItemTitle), safe(r.ItemURL), safe(r.PromoCode),
			optInt64(r.CompAmount), compTitle[r.CompStatus], a.when(r.CompPaidAt),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func optInt(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func optInt64(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func decisionTitle(d string) string {
	switch d {
	case domain.ReportAccepted:
		return "принят"
	case domain.ReportRework:
		return "на доработку"
	}
	return ""
}

// MediaLink ссылка на медиафайл отчёта: открывает бота, и тот присылает файл админу.
// Прямая ссылка Telegram содержит токен бота, поэтому не используется. Без username бота ссылки нет.
func MediaLink(bot string, reportID int64, questionKey string) string {
	if bot == "" {
		return ""
	}
	return fmt.Sprintf("https://t.me/%s?start=m%dx%s", bot, reportID, questionKey)
}

// answerColumn колонка вопроса в сводной выгрузке: вопрос конкретного сценария.
type answerColumn struct {
	scenario string
	key      string
	title    string
}

// AnswersCSV ответы по отчётам всех сценариев сразу: одна строка на отчёт (последняя версия),
// у каждого вопроса каждого сценария своя колонка (пустая там, где вопроса нет). В начале роль и
// Telegram ID. Фото и видео в таблицу не попадают: вместо них ссылка, по которой бот покажет файл админу.
func (a *Analytics) AnswersCSV(ctx context.Context, rows []storage.ExportRow) ([]byte, error) {
	reported := make([]storage.ExportRow, 0, len(rows))
	for _, r := range rows {
		if r.Revision > 0 {
			reported = append(reported, r)
		}
	}
	// По возрастанию номера задания: старые отчёты сверху.
	sort.Slice(reported, func(i, j int) bool { return reported[i].TaskID < reported[j].TaskID })

	// Колонки: сценарии по ключу, вопросы в порядке сценария; вопросы старых версий в конец.
	scenarios := map[string]int64{}
	for _, r := range reported {
		scenarios[r.Key] = r.ScenarioID
	}
	keys := make([]string, 0, len(scenarios))
	for k := range scenarios {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cols []answerColumn
	index := map[[2]string]int{}
	add := func(sc, key, title string) {
		if _, ok := index[[2]string{sc, key}]; ok {
			return
		}
		index[[2]string{sc, key}] = len(cols)
		cols = append(cols, answerColumn{scenario: sc, key: key, title: title})
	}
	for _, sc := range keys {
		v, err := a.store.Repos().Scenarios.LatestVersion(ctx, scenarios[sc])
		if err != nil {
			continue // нет версии: колонки возьмём из ответов
		}
		for _, q := range v.Body.Questions {
			add(sc, q.Key, q.Text)
		}
	}
	for _, r := range reported {
		for _, an := range r.Answers {
			add(r.Key, an.Key, an.Key)
		}
	}

	w, buf := newCSV()
	head := []string{"№ задания", "Роль", "Telegram ID", "Имя", "Username", "Оператор", "Сценарий", "Версия сценария", "Статус",
		"После срока", "Дата отчёта", "Версия отчёта", "Айтем", "Ссылка на айтем"}
	for _, c := range cols {
		h := c.scenario + " / " + c.key
		if c.title != "" && c.title != c.key {
			h += ": " + c.title
		}
		head = append(head, safe(h))
	}
	head = append(head, "Сумма компенсации", "Статус компенсации")
	_ = w.Write(head)

	bot := a.botName()
	for _, r := range reported {
		rec := []string{strconv.FormatInt(r.TaskID, 10), kindTitle(r.Kind), strconv.FormatInt(r.UserID, 10), safe(r.UserName), safe(r.Username),
			safe(r.Operator), safe(r.Key), strconv.Itoa(r.Version), r.Status.Title(),
			yesNo(r.Late), a.when(r.ReportedAt), strconv.Itoa(r.Revision), safe(r.ItemTitle), safe(r.ItemURL)}
		cells := make([]string, len(cols))
		for _, an := range r.Answers {
			if i, ok := index[[2]string{r.Key, an.Key}]; ok {
				cells[i] = answerCell(an, MediaLink(bot, r.ReportID, an.Key))
			}
		}
		rec = append(rec, cells...)
		rec = append(rec, optInt64(r.CompAmount), compTitle[r.CompStatus])
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes(), nil
}

func answerCell(an domain.Answer, mediaLink string) string {
	if an.Skipped {
		return "(пропущено)"
	}
	switch an.Type {
	case domain.QPhoto, domain.QVideo:
		if mediaLink != "" {
			return mediaLink
		}
		if an.Type == domain.QVideo {
			return "видео приложено"
		}
		return "фото приложено"
	case domain.QYesNo:
		if an.Value == "yes" {
			return "да"
		}
		return "нет"
	}
	return safe(an.Value)
}

// CompsCSV компенсации периода (только задания, где есть данные компенсации).
func (a *Analytics) CompsCSV(rows []storage.ExportRow) []byte {
	w, buf := newCSV()
	_ = w.Write([]string{"№ задания", "Telegram ID", "Имя", "Оператор", "Сценарий", "Отчёт", "Сумма (сум)", "Статус", "Выплачено"})
	for _, r := range rows {
		if r.CompStatus == "" {
			continue
		}
		_ = w.Write([]string{strconv.FormatInt(r.TaskID, 10), strconv.FormatInt(r.UserID, 10), safe(buyerName(r)), safe(r.Operator), safe(r.Title),
			a.when(r.ReportedAt), strconv.FormatInt(r.CompAmount, 10), compTitle[r.CompStatus], a.when(r.CompPaidAt)})
	}
	w.Flush()
	return buf.Bytes()
}

// AuditCSV журнал действий админов (последние limit записей).
func (a *Analytics) AuditCSV(ctx context.Context, limit int) ([]byte, error) {
	list, err := a.store.Repos().Audit.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	w, buf := newCSV()
	_ = w.Write([]string{"Время", "Админ (ID)", "Действие", "Объект", "ID объекта", "Подробности"})
	for _, e := range list {
		_ = w.Write([]string{a.when(e.At), strconv.FormatInt(e.AdminID, 10), safe(e.Action), safe(e.Entity), safe(e.EntityID), safe(e.Details)})
	}
	w.Flush()
	return buf.Bytes(), nil
}

// Period переводит код периода в границы: "w" последние 7 дней, "m" 30 дней, "a" всё время.
func Period(code string, now time.Time) (from, to time.Time, title string) {
	switch code {
	case "w":
		return now.AddDate(0, 0, -7), time.Time{}, "7 дней"
	case "m":
		return now.AddDate(0, 0, -30), time.Time{}, "30 дней"
	}
	return time.Time{}, time.Time{}, "всё время"
}

// FormatDuration «2 дн. 3 ч», «5 ч», «40 мин».
func FormatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "нет данных"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч", int(d.Hours()))
	}
	h := int(d.Hours())
	if h%24 == 0 {
		return fmt.Sprintf("%d дн.", h/24)
	}
	return fmt.Sprintf("%d дн. %d ч", h/24, h%24)
}
