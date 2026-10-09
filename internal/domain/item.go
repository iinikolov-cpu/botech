package domain

import "time"

// ItemStatus состояние айтема (товара для покупки).
type ItemStatus string

const (
	ItemFree     ItemStatus = "free"     // свободен, виден покупателям в списке
	ItemReserved ItemStatus = "reserved" // выбран покупателем под задание
	ItemUsed     ItemStatus = "used"     // куплен: по заданию отправлен отчёт
)

// Item айтем: товар (объявление), который тайный покупатель должен купить. Одноразовый.
type Item struct {
	ID         int64
	Title      string
	URL        string // ссылка на объявление (http или https)
	Price      int64  // цена в сумах, 0 = не указана
	Note       string // подсказка покупателю
	AddedBy    int64
	CreatedAt  time.Time
	Status     ItemStatus
	TaskID     int64 // задание, под которое выбран айтем (0, если свободен)
	ReservedAt time.Time
	FinishedAt time.Time
}

// ItemStats сводка по пулу айтемов.
type ItemStats struct {
	Total    int
	Free     int
	Reserved int
	Used     int
}
