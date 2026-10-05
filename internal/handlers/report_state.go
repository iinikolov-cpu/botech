package handlers

import "botech/internal/domain"

type reportState struct {
	TaskID  int64           `json:"task_id"`
	Step    int             `json:"step"` // растёт при каждом изменении: защита от нажатий на устаревшие кнопки
	Phase   string          `json:"phase"`
	Idx     int             `json:"idx"` // номер вопроса в фазе phaseQuestion
	Answers []domain.Answer `json:"answers"`

	WantComp bool `json:"want_comp"`
	CompOnly bool `json:"comp_only"` // исправление только данных компенсации, без отчёта
	// При доработке отчёта уже отправленную компенсацию повторно не запрашиваем.
	KeepComp        bool   `json:"keep_comp"`     // данные компенсации уже есть (к выплате или выплачена) и остаются как есть
	KeepAmount      int64  `json:"keep_amount"`   // сумма уже отправленной компенсации (для показа)
	CompRejected    bool   `json:"comp_rejected"` // компенсация отклонена: новые данные обязательны
	Amount          int64  `json:"amount"`
	ReceiptFileID   string `json:"receipt_file_id"`
	ReceiptUniqueID string `json:"receipt_unique_id"`

	// Сообщения покупателя с ответами (фото, текст): после отправки отчёта удаляются из чата.
	// Файлы остаются в Telegram, отчёт хранит их file_id.
	Msgs []int `json:"msgs,omitempty"`
}

// setAnswer записывает (или заменяет) ответ на вопрос.
func (st *reportState) setAnswer(an domain.Answer) {
	for i := range st.Answers {
		if st.Answers[i].Key == an.Key {
			st.Answers[i] = an
			return
		}
	}
	st.Answers = append(st.Answers, an)
}

// advance переходит к следующей фазе после ответа на вопрос.
func (st *reportState) advance(total int) {
	switch st.Phase {
	case phaseQuestion:
		switch {
		case st.Idx+1 < total:
			st.Idx++
		case st.KeepComp: // компенсация уже отправлена: сразу к итогу
			st.Phase = phaseConfirm
		case st.CompRejected: // прежние данные отклонены: спрашивать «нужна ли» незачем
			st.WantComp, st.Phase = true, phaseCompAmount
		default:
			st.Phase = phaseCompAsk
		}
	case phaseCompAsk:
		if st.WantComp {
			st.Phase = phaseCompAmount
		} else {
			st.Phase = phaseConfirm
		}
	case phaseCompAmount:
		st.Phase = phaseCompReceipt
	case phaseCompReceipt:
		st.Phase = phaseConfirm
	}
}

// back возвращается на шаг назад и стирает ответ, к которому вернулись.
func (st *reportState) back(total int) {
	switch st.Phase {
	case phaseQuestion:
		if st.Idx > 0 {
			st.Idx--
		}
	case phaseCompAsk:
		st.Phase, st.Idx = phaseQuestion, total-1
	case phaseCompAmount:
		switch {
		case st.CompOnly: // при исправлении компенсации шага «назад» нет
		case st.KeepComp: // передумал менять: возвращаемся к итогу с прежней компенсацией
			st.WantComp, st.Phase = false, phaseConfirm
		case st.CompRejected:
			st.Phase, st.Idx = phaseQuestion, total-1
		default:
			st.Phase = phaseCompAsk
		}
	case phaseCompReceipt:
		st.Phase = phaseCompAmount
	case phaseConfirm:
		switch {
		case st.WantComp:
			st.Phase = phaseCompReceipt
		case st.KeepComp:
			st.Phase, st.Idx = phaseQuestion, total-1
		default:
			st.Phase = phaseCompAsk
		}
	}
}
