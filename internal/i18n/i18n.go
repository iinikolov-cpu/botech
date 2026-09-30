// Пакет i18n хранит тексты интерфейса покупателя. Сейчас только RU,
// узбекский добавляется отдельной таблицей uz без изменения кода обработчиков.
package i18n

import "fmt"

// Lang код языка интерфейса.
type Lang string

const (
	RU Lang = "ru"
	UZ Lang = "uz" // зарезервировано, тексты будут добавлены позже
)

var tables = map[Lang]map[string]string{
	RU: ru,
}

// T возвращает текст по ключу; при отсутствии перевода откатывается на RU,
// при отсутствии ключа возвращает сам ключ (так пропуск заметен сразу).
func T(lang Lang, key string, args ...any) string {
	s, ok := tables[lang][key]
	if !ok {
		s, ok = tables[RU][key]
	}
	if !ok {
		return key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}
