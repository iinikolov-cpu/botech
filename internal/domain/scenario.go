package domain

import "time"

// QuestionType тип ответа на вопрос чек-листа.
type QuestionType string

const (
	QText   QuestionType = "text"
	QRating QuestionType = "rating" // оценка 1-5
	QYesNo  QuestionType = "yesno"
	QPhoto  QuestionType = "photo"
	QVideo  QuestionType = "video"
)

// Valid true для известных типов.
func (t QuestionType) Valid() bool {
	switch t {
	case QText, QRating, QYesNo, QPhoto, QVideo:
		return true
	}
	return false
}

// Question вопрос чек-листа отчёта.
type Question struct {
	Key      string       `json:"key"`
	Text     string       `json:"text"`
	Type     QuestionType `json:"type"`
	Required bool         `json:"required"`
}

// ScenarioBody полное содержимое версии сценария (хранится JSON-ом).
type ScenarioBody struct {
	Kind      TaskKind   `json:"kind,omitempty"` // пусто в старых версиях: считается «покупатель»
	Title     string     `json:"title"`
	Operator  string     `json:"operator"`
	City      string     `json:"city,omitempty"`
	PVZ       string     `json:"pvz,omitempty"`
	Steps     []string   `json:"steps"`
	Questions []Question `json:"questions"`
}

// Scenario сценарий: ключ + признак архива. Содержимое в версиях.
type Scenario struct {
	ID            int64
	Key           string
	Kind          TaskKind // тип сценария, задаётся при создании и не меняется
	Title         string
	Operator      string
	Archived      bool
	LatestVersion int // номер последней версии, заполняется при чтении
	CreatedBy     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ScenarioVersion неизменяемая версия сценария.
type ScenarioVersion struct {
	ID         int64
	ScenarioID int64
	Version    int
	Body       ScenarioBody
	CreatedBy  int64
	CreatedAt  time.Time
}
