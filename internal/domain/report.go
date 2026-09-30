package domain

import "time"

// PromoStatus состояние промокода в пуле.
type PromoStatus string

const (
	PromoFree   PromoStatus = "free"
	PromoIssued PromoStatus = "issued"
	PromoUsed   PromoStatus = "used"
)

// PromoCode промокод на оплату доставки.
type PromoCode struct {
	ID        int64
	Code      string
	Status    PromoStatus
	TaskID    int64
	UserID    int64
	AddedBy   int64
	CreatedAt time.Time
	IssuedAt  time.Time
	UsedAt    time.Time
	UsedBy    int64
}

// PromoStats остатки пула.
type PromoStats struct {
	Free, Issued, Used int
}

// Answer ответ на вопрос чек-листа.
type Answer struct {
	Key          string       `json:"key"`
	Type         QuestionType `json:"type"`
	Value        string       `json:"value,omitempty"` // текст, оценка "1".."5" или "yes"/"no"
	FileID       string       `json:"file_id,omitempty"`
	FileUniqueID string       `json:"file_unique_id,omitempty"`
	Skipped      bool         `json:"skipped,omitempty"`
}

// Report отчёт по заданию.
type Report struct {
	ID          int64
	TaskID      int64
	UserID      int64
	Late        bool // отправлен после срока
	SubmittedAt time.Time
	Answers     []Answer
}

// CompStatus статус компенсации.
type CompStatus string

const (
	CompPending CompStatus = "pending" // к выплате
	CompPaid    CompStatus = "paid"
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
	CreatedAt       time.Time
	PaidAt          time.Time
	PaidBy          int64
}
