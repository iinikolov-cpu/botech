package domain

import "time"

// PromoCode промокод в пуле. Код многоразовый: сколько раз его можно использовать
// всего, определяет общая настройка (PromoMaxUses), а здесь хранится, сколько раз уже использован.
type PromoCode struct {
	ID           int64
	Code         string
	UsedCount    int   // сколько заданий уже выполнено с этим кодом
	ActiveTaskID int64 // задание, за которым код закреплён сейчас (0, если код свободен)
	AddedBy      int64
	CreatedAt    time.Time
}

// PromoOutcome чем закончилась выдача кода заданию.
type PromoOutcome string

const (
	PromoActive   PromoOutcome = "active"   // задание в работе, код закреплён за ним
	PromoUsed     PromoOutcome = "used"     // задание выполнено, использование засчитано
	PromoReleased PromoOutcome = "released" // задание отменено, использование не засчитано
)

// PromoAssignment выдача кода заданию.
type PromoAssignment struct {
	ID         int64
	PromoID    int64
	Code       string
	TaskID     int64
	UserID     int64
	IssuedAt   time.Time
	FinishedAt time.Time
	Outcome    PromoOutcome
}

// PromoStats сводка по пулу (при заданном лимите использований).
type PromoStats struct {
	Total        int // всего кодов
	Available    int // можно выдать прямо сейчас: не заняты и есть остаток использований
	Busy         int // закреплены за активными заданиями
	Exhausted    int // не заняты, но использования закончились
	WithUsesLeft int // коды с остатком использований (свободные и занятые вместе)
}

// MaxFilesPerAnswer сколько фото или видео можно приложить к одному ответу.
const MaxFilesPerAnswer = 10

// File файл в Telegram (фото или видео): идентификатор для отправки и уникальный для сравнения.
type File struct {
	ID       string `json:"id"`
	UniqueID string `json:"uid,omitempty"`
}

// Answer ответ на вопрос чек-листа.
type Answer struct {
	Key          string       `json:"key"`
	Type         QuestionType `json:"type"`
	Value        string       `json:"value,omitempty"`          // текст, оценка "1".."5" или "yes"/"no"
	FileID       string       `json:"file_id,omitempty"`        // первый файл (для совместимости)
	FileUniqueID string       `json:"file_unique_id,omitempty"` // его уникальный идентификатор
	Files        []File       `json:"files,omitempty"`          // все файлы ответа (фото или видео), включая первый
	Skipped      bool         `json:"skipped,omitempty"`
}

// AllFiles все файлы ответа: Files, а для старых ответов с одним файлом FileID.
func (a Answer) AllFiles() []File {
	if len(a.Files) > 0 {
		return a.Files
	}
	if a.FileID != "" {
		return []File{{ID: a.FileID, UniqueID: a.FileUniqueID}}
	}
	return nil
}

// SetFiles записывает файлы ответа и поддерживает поля первого файла.
func (a *Answer) SetFiles(files []File) {
	a.Files = files
	a.FileID, a.FileUniqueID = "", ""
	if len(files) > 0 {
		a.FileID, a.FileUniqueID = files[0].ID, files[0].UniqueID
	}
}

// Решение админа по отчёту.
const (
	ReportPending  = ""         // ещё не рассмотрен
	ReportAccepted = "accepted" // принят (задание «проверено»)
	ReportRework   = "rework"   // возвращён на доработку
)

// Report отчёт по заданию. После доработки появляется новая версия (Revision), старые хранятся.
type Report struct {
	ID           int64
	TaskID       int64
	UserID       int64
	Revision     int  // номер версии отчёта: 1, 2, ...
	Late         bool // отправлен после срока
	SubmittedAt  time.Time
	Decision     string // ReportPending, ReportAccepted, ReportRework
	AdminComment string // комментарий админа при возврате на доработку
	DecidedAt    time.Time
	Answers      []Answer
}

// CompStatus статус компенсации.
type CompStatus string

const (
	CompPending  CompStatus = "pending" // к выплате
	CompPaid     CompStatus = "paid"
	CompRejected CompStatus = "rejected" // данные неверны, покупателю нужно исправить
)

// Compensation данные для компенсации стоимости товара.
type Compensation struct {
	ID              int64
	TaskID          int64
	UserID          int64
	Amount          int64 // сумма в сумах
	ReceiptFileID   string
	ReceiptUniqueID string
	Status          CompStatus
	AdminComment    string // причина отклонения
	CreatedAt       time.Time
	PaidAt          time.Time
	PaidBy          int64
}
