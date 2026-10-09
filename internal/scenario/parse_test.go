package scenario

import (
	"strings"
	"testing"

	"botech/internal/domain"
)

func TestTemplateIsValid(t *testing.T) {
	p, errs := Parse(Template)
	if len(errs) > 0 {
		t.Fatalf("шаблон должен проходить проверку, ошибки: %v", errs)
	}
	if p.Key == "" || len(p.Body.Steps) == 0 || len(p.Body.Questions) != 4 {
		t.Fatalf("неожиданный результат разбора: %+v", p)
	}
	if !p.Body.Questions[0].Required || p.Body.Questions[3].Required {
		t.Fatal("required: по умолчанию true, явное false должно сохраняться")
	}
}

func TestParseJSON(t *testing.T) {
	js := `{"key":"abc-1","title":"T","operator":"O","steps":["a"],
	        "questions":[{"key":"q_one","text":"Q","type":"rating"}]}`
	p, errs := Parse([]byte(js))
	if len(errs) > 0 {
		t.Fatalf("JSON должен читаться: %v", errs)
	}
	if p.Body.Questions[0].Type != domain.QRating {
		t.Fatalf("тип %s", p.Body.Questions[0].Type)
	}
}

func TestParseErrors(t *testing.T) {
	const ok = `key: abc-1
title: T
operator: O
steps: [a]
questions:
  - {key: q_one, text: Q, type: text}
`
	tests := []struct {
		name string
		mod  func(string) string
		want string // подстрока в одной из ошибок
	}{
		{"плохой key", func(s string) string { return strings.Replace(s, "key: abc-1", "key: ABC", 1) }, "key:"},
		{"нет title", func(s string) string { return strings.Replace(s, "title: T", "title: ''", 1) }, "title"},
		{"нет operator", func(s string) string { return strings.Replace(s, "operator: O", "operator: ''", 1) }, "operator"},
		{"нет шагов", func(s string) string { return strings.Replace(s, "steps: [a]", "steps: []", 1) }, "нужен хотя бы один шаг"},
		{"пустой шаг", func(s string) string { return strings.Replace(s, "steps: [a]", "steps: ['']", 1) }, "шаг 1: пустой"},
		{"длинный шаг", func(s string) string {
			return strings.Replace(s, "steps: [a]", "steps: ['"+strings.Repeat("я", 301)+"']", 1)
		}, "максимум 300"},
		{"неизвестный тип", func(s string) string { return strings.Replace(s, "type: text", "type: audio", 1) }, "неизвестный type"},
		{"дубль ключа вопроса", func(s string) string {
			return s + "  - {key: q_one, text: Q2, type: text}\n"
		}, "уже используется"},
		{"нет вопросов", func(s string) string {
			return strings.Split(s, "questions:")[0] + "questions: []\n"
		}, "нужен хотя бы один вопрос"},
		{"опечатка в поле", func(s string) string { return s + "operatorr: x\n" }, "не существует"},
		{"не YAML", func(string) string { return "key: [" }, "не удалось прочитать"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := Parse([]byte(tc.mod(ok)))
			if len(errs) == 0 {
				t.Fatal("ожидали ошибку, получили успех")
			}
			if !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Fatalf("ошибки %v не содержат %q", errs, tc.want)
			}
		})
	}
	if _, errs := Parse([]byte(ok)); len(errs) > 0 {
		t.Fatalf("базовый пример должен быть валиден: %v", errs)
	}
}

func TestParseTooBig(t *testing.T) {
	if _, errs := Parse(make([]byte, MaxFileSize+1)); len(errs) == 0 {
		t.Fatal("слишком большой файл должен отклоняться")
	}
}

func TestParseKind(t *testing.T) {
	const base = `key: abc-1
title: T
operator: O
steps: [a]
questions:
  - {key: q_one, text: Q, type: text}
`
	tests := []struct {
		name string
		kind string // строка, добавляемая в файл
		want domain.TaskKind
		err  string
	}{
		{"нет поля: покупатель", "", domain.KindBuyer, ""},
		{"покупатель", "kind: buyer\n", domain.KindBuyer, ""},
		{"продавец", "kind: seller\n", domain.KindSeller, ""},
		{"регистр и пробелы", "kind: ' Seller '\n", domain.KindSeller, ""},
		{"неизвестный тип", "kind: courier\n", "", "kind:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, errs := Parse([]byte(tc.kind + base))
			if tc.err != "" {
				if len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), tc.err) {
					t.Fatalf("ожидали ошибку %q, получили %v", tc.err, errs)
				}
				return
			}
			if len(errs) > 0 || p.Body.Kind != tc.want {
				t.Fatalf("kind = %v, ошибки %v; ожидали %v", p, errs, tc.want)
			}
		})
	}
}

func TestSellerTemplateIsValid(t *testing.T) {
	p, errs := Parse(TemplateSeller)
	if len(errs) > 0 {
		t.Fatalf("шаблон продавца должен проходить проверку: %v", errs)
	}
	if p.Body.Kind != domain.KindSeller {
		t.Fatalf("тип шаблона продавца: %v", p.Body.Kind)
	}
	if b, _ := Parse(Template); b == nil || b.Body.Kind != domain.KindBuyer {
		t.Fatal("шаблон покупателя должен иметь тип buyer")
	}
}
