package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"botech/internal/domain"
	"botech/internal/storage"
)

// Analytics статистика и выгрузки в CSV.
type Analytics struct {
	store storage.Store
	loc   *time.Location
}

// NewAnalytics создаёт сервис. loc: часовой пояс для дат в выгрузках.
func NewAnalytics(store storage.Store, loc *time.Location) *Analytics {
	return &Analytics{store: store, loc: loc}
}

// QuestionStat итог по одному вопросу у оператора.
type QuestionStat struct {
	Key   string
	Text  string
	Type  domain.QuestionType
	N     int     // сколько ответов учтено
	Avg   float64 // средняя оценка (для rating)
	YesPc float64 // доля «да» в процентах (для yesno)
}

// OperatorStat итог по оператору.
type OperatorStat struct {
	Operator  string
	Issued    int
	Completed int
	Overdue   int
	AvgTime   time.Duration // от принятия до отчёта
	Questions []QuestionStat
}

// Stats общая статистика за период.
type Stats struct {
	Total     int // всего заданий
	Issued    int // доставлено покупателям
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

	CompPending, CompPaid, CompRejected int
	SumPending, SumPaid                 int64
}

// Rows выборка заданий для выгрузок и статистики.
func (a *Analytics) Rows(ctx context.Context, f storage.ExportFilter) ([]storage.ExportRow, error) {
	return a.store.Repos().Stats.Export(ctx, f)
}

type ratingAcc struct {
	n, sum, yes int
	text        string
	typ         domain.QuestionType
}

// Stats считает сводку по заданиям периода.
func (a *Analytics) Stats(ctx context.Context, f storage.ExportFilter) (*Stats, error) {
	rows, err := a.Rows(ctx, f)
	if err != nil {
		return nil, err
	}
	texts, err := a.questionTexts(ctx, rows)
	if err != nil {
		return nil, err
	}
	st := &Stats{Total: len(rows)}
	type opAcc struct {
		OperatorStat
		dur  time.Duration
		durN int
		q    map[string]*ratingAcc
		keys []string
	}
	ops := map[string]*opAcc{}
	var totalDur time.Duration
	var totalN int

	for _, r := range rows {
		op := ops[r.Operator]
		if op == nil {
			op = &opAcc{OperatorStat: OperatorStat{Operator: r.Operator}, q: map[string]*ratingAcc{}}
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
		for _, an := range r.Answers {
			if an.Skipped || (an.Type != domain.QRating && an.Type != domain.QYesNo) {
				continue
			}
			acc := op.q[an.Key]
			if acc == nil {
				acc = &ratingAcc{typ: an.Type, text: texts[r.Operator+"\x00"+an.Key]}
				op.q[an.Key] = acc
				op.keys = append(op.keys, an.Key)
			}
			switch an.Type {
			case domain.QRating:
				if v, err := strconv.Atoi(an.Value); err == nil {
					acc.n++
					acc.sum += v
				}
			case domain.QYesNo:
				acc.n++
				if an.Value == "yes" {
					acc.yes++
				}
			}
		}
	}
	if totalN > 0 {
		st.AvgTime = totalDur / time.Duration(totalN)
	}
	for _, op := range ops {
		if op.durN > 0 {
			op.AvgTime = op.dur / time.Duration(op.durN)
		}
		for _, k := range op.keys {
			acc := op.q[k]
			if acc.n == 0 {
				continue
			}
			q := QuestionStat{Key: k, Text: acc.text, Type: acc.typ, N: acc.n}
			if acc.typ == domain.QRating {
				q.Avg = float64(acc.sum) / float64(acc.n)
			} else {
				q.YesPc = 100 * float64(acc.yes) / float64(acc.n)
			}
			op.Questions = append(op.Questions, q)
		}
		st.Operators = append(st.Operators, op.OperatorStat)
	}
	sort.Slice(st.Operators, func(i, j int) bool { return st.Operators[i].Operator < st.Operators[j].Operator })
	return st, nil
}

// questionTexts формулировки вопросов по последним версиям сценариев: ключ "оператор\x00вопрос".
func (a *Analytics) questionTexts(ctx context.Context, rows []storage.ExportRow) (map[string]string, error) {
	out := map[string]string{}
	seen := map[int64]bool{}
	for _, r := range rows {
		if seen[r.ScenarioID] {
			continue
		}
		seen[r.ScenarioID] = true
		v, err := a.store.Repos().Scenarios.LatestVersion(ctx, r.ScenarioID)
		if err != nil {
			continue // сценарий без версии: подписи будут по ключу
		}
		for _, q := range v.Body.Questions {
			k := r.Operator + "\x00" + q.Key
			if _, ok := out[k]; !ok {
				out[k] = q.Text
			}
		}
	}
	return out, nil
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

var compTitle = map[domain.CompStatus]string{
	domain.CompPending: "к выплате", domain.CompPaid: "выплачено", domain.CompRejected: "отклонено",
}

// TasksCSV все задания периода: статусы, даты этапов, напоминания, промокод, компенсация.
func (a *Analytics) TasksCSV(rows []storage.ExportRow) []byte {
	w, buf := newCSV()
	_ = w.Write([]string{"№ задания", "Покупатель", "Username", "Оператор", "Сценарий", "Версия сценария", "Статус",
		"Создано", "Отправлено", "Принято", "Срок до", "Отчёт", "Проверено", "Версий отчёта", "После срока", "Решение по отчёту",
		"Напоминаний", "Промокод", "Сумма компенсации", "Статус компенсации", "Выплачено"})
	for _, r := range rows {
		_ = w.Write([]string{
			strconv.FormatInt(r.TaskID, 10), safe(buyerName(r)), safe(r.Username), safe(r.Operator), safe(r.Title),
			strconv.Itoa(r.Version), r.Status.Title(),
			a.when(r.CreatedAt), a.when(r.SentAt), a.when(r.AcceptedAt), a.when(r.DueAt), a.when(r.ReportedAt), a.when(r.ReviewedAt),
			optInt(r.Revision), yesNo(r.Late), decisionTitle(r.Decision),
			optInt(r.Reminders), safe(r.PromoCode), optInt64(r.CompAmount), compTitle[r.CompStatus], a.when(r.CompPaidAt),
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

// AnswersCSV ответы по одному сценарию: одна строка на отчёт, по колонке на каждый вопрос.
// Фото и видео в таблицу не попадают (файлы хранятся в Telegram), вместо них отметка «приложено».
func (a *Analytics) AnswersCSV(ctx context.Context, scenarioID int64, rows []storage.ExportRow) ([]byte, error) {
	v, err := a.store.Repos().Scenarios.LatestVersion(ctx, scenarioID)
	if err != nil {
		return nil, err
	}
	var keys []string
	title := map[string]string{}
	typ := map[string]domain.QuestionType{}
	for _, q := range v.Body.Questions {
		keys = append(keys, q.Key)
		title[q.Key] = q.Text
		typ[q.Key] = q.Type
	}
	// Вопросы старых версий, которых нет в последней, добавляются в конец.
	for _, r := range rows {
		for _, an := range r.Answers {
			if _, ok := typ[an.Key]; !ok {
				keys = append(keys, an.Key)
				title[an.Key] = an.Key
				typ[an.Key] = an.Type
			}
		}
	}
	w, buf := newCSV()
	head := []string{"№ задания", "Дата отчёта", "Покупатель", "Оператор", "Версия сценария", "Статус", "После срока", "Версия отчёта"}
	for _, k := range keys {
		h := k
		if t := title[k]; t != "" && t != k {
			h = k + ": " + t
		}
		head = append(head, safe(h))
	}
	head = append(head, "Сумма компенсации", "Статус компенсации")
	_ = w.Write(head)
	for _, r := range rows {
		if r.Revision == 0 || r.ScenarioID != scenarioID {
			continue
		}
		byKey := map[string]domain.Answer{}
		for _, an := range r.Answers {
			byKey[an.Key] = an
		}
		rec := []string{strconv.FormatInt(r.TaskID, 10), a.when(r.ReportedAt), safe(buyerName(r)), safe(r.Operator),
			strconv.Itoa(r.Version), r.Status.Title(), yesNo(r.Late), strconv.Itoa(r.Revision)}
		for _, k := range keys {
			rec = append(rec, answerCell(byKey[k], byKey[k].Key != ""))
		}
		rec = append(rec, optInt64(r.CompAmount), compTitle[r.CompStatus])
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes(), nil
}

func answerCell(an domain.Answer, present bool) string {
	switch {
	case !present:
		return ""
	case an.Skipped:
		return "(пропущено)"
	}
	switch an.Type {
	case domain.QPhoto:
		return "фото приложено"
	case domain.QVideo:
		return "видео приложено"
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
	_ = w.Write([]string{"№ задания", "Покупатель", "Оператор", "Сценарий", "Отчёт", "Сумма (сум)", "Статус", "Выплачено"})
	for _, r := range rows {
		if r.CompStatus == "" {
			continue
		}
		_ = w.Write([]string{strconv.FormatInt(r.TaskID, 10), safe(buyerName(r)), safe(r.Operator), safe(r.Title),
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
