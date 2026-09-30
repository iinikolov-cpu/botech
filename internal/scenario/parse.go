// Пакет scenario разбирает и проверяет файлы сценариев (YAML или JSON).
package scenario

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"botech/internal/domain"
)

// Template пример файла сценария, его бот отправляет админу по кнопке.
//
//go:embed template.yaml
var Template []byte

// Ограничения: инструкция и вопросы должны помещаться в сообщения Telegram.
const (
	MaxFileSize   = 100 * 1024
	MaxSteps      = 12
	MaxQuestions  = 30
	MaxStepLen    = 300
	MaxStepsTotal = 3000
	MaxQuestion   = 300
	MaxTitleLen   = 100
)

var keyRe = regexp.MustCompile(`^[a-z0-9_-]{3,40}$`)

// file структура файла сценария. required задаётся указателем, чтобы отличить «не указано» от false.
type file struct {
	Key       string   `yaml:"key"`
	Title     string   `yaml:"title"`
	Operator  string   `yaml:"operator"`
	City      string   `yaml:"city"`
	PVZ       string   `yaml:"pvz"`
	Steps     []string `yaml:"steps"`
	Questions []struct {
		Key      string `yaml:"key"`
		Text     string `yaml:"text"`
		Type     string `yaml:"type"`
		Required *bool  `yaml:"required"`
	} `yaml:"questions"`
}

// Parsed результат разбора.
type Parsed struct {
	Key  string
	Body domain.ScenarioBody
}

// Parse разбирает файл. Возвращает список понятных ошибок (пустой = всё хорошо).
// JSON является подмножеством YAML, поэтому один разборщик читает оба формата.
func Parse(data []byte) (*Parsed, []string) {
	if len(data) > MaxFileSize {
		return nil, []string{fmt.Sprintf("файл слишком большой (максимум %d КБ)", MaxFileSize/1024)}
	}
	if !utf8.Valid(data) {
		return nil, []string{"файл должен быть в кодировке UTF-8"}
	}
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // опечатки в названиях полей должны быть ошибкой, а не молча игнорироваться
	if err := dec.Decode(&f); err != nil {
		return nil, []string{"не удалось прочитать файл: " + cleanYAMLErr(err)}
	}

	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	key := strings.TrimSpace(f.Key)
	if !keyRe.MatchString(key) {
		add("key: нужна латиница в нижнем регистре, цифры, - и _ (3-40 символов)")
	}
	title, operator := strings.TrimSpace(f.Title), strings.TrimSpace(f.Operator)
	switch {
	case title == "":
		add("title: не заполнено")
	case utf8.RuneCountInString(title) > MaxTitleLen:
		add("title: не длиннее %d символов", MaxTitleLen)
	}
	switch {
	case operator == "":
		add("operator: не заполнено")
	case utf8.RuneCountInString(operator) > MaxTitleLen:
		add("operator: не длиннее %d символов", MaxTitleLen)
	}

	steps := make([]string, 0, len(f.Steps))
	total := 0
	for i, s := range f.Steps {
		s = strings.TrimSpace(s)
		n := utf8.RuneCountInString(s)
		switch {
		case s == "":
			add("шаг %d: пустой", i+1)
		case n > MaxStepLen:
			add("шаг %d: %d символов, максимум %d", i+1, n, MaxStepLen)
		}
		total += n
		steps = append(steps, s)
	}
	switch {
	case len(steps) == 0:
		add("steps: нужен хотя бы один шаг инструкции")
	case len(steps) > MaxSteps:
		add("steps: не больше %d шагов", MaxSteps)
	}
	if total > MaxStepsTotal {
		add("steps: суммарно %d символов, максимум %d (инструкция должна помещаться в одно сообщение)", total, MaxStepsTotal)
	}

	questions := make([]domain.Question, 0, len(f.Questions))
	seen := map[string]bool{}
	for i, q := range f.Questions {
		n := i + 1
		qkey, text := strings.TrimSpace(q.Key), strings.TrimSpace(q.Text)
		typ := domain.QuestionType(strings.ToLower(strings.TrimSpace(q.Type)))
		switch {
		case !keyRe.MatchString(qkey):
			add("вопрос %d: key нужен латиницей (3-40 символов: a-z, 0-9, - и _)", n)
		case seen[qkey]:
			add("вопрос %d: key %q уже используется", n, qkey)
		}
		seen[qkey] = true
		switch {
		case text == "":
			add("вопрос %d: не заполнен text", n)
		case utf8.RuneCountInString(text) > MaxQuestion:
			add("вопрос %d: text длиннее %d символов", n, MaxQuestion)
		}
		if !typ.Valid() {
			add("вопрос %d: неизвестный type %q (допустимо: text, rating, yesno, photo, video)", n, q.Type)
		}
		required := true
		if q.Required != nil {
			required = *q.Required
		}
		questions = append(questions, domain.Question{Key: qkey, Text: text, Type: typ, Required: required})
	}
	switch {
	case len(questions) == 0:
		add("questions: нужен хотя бы один вопрос")
	case len(questions) > MaxQuestions:
		add("questions: не больше %d вопросов", MaxQuestions)
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return &Parsed{
		Key: key,
		Body: domain.ScenarioBody{
			Title: title, Operator: operator,
			City: strings.TrimSpace(f.City), PVZ: strings.TrimSpace(f.PVZ),
			Steps: steps, Questions: questions,
		},
	}, nil
}

// cleanYAMLErr делает ошибку разборщика чуть понятнее.
func cleanYAMLErr(err error) string {
	s := strings.TrimPrefix(err.Error(), "yaml: ")
	s = strings.ReplaceAll(s, "not found in type scenario.file", "не существует (проверьте название поля)")
	s = strings.ReplaceAll(s, "field ", "поле ")
	s = strings.ReplaceAll(s, "line ", "строка ")
	return s
}
