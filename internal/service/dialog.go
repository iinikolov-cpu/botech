package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"botech/internal/storage"
)

// Dialog хранит состояние пошаговых диалогов в БД, поэтому оно переживает рестарт бота.
type Dialog struct {
	store storage.Store
	now   func() time.Time
}

// NewDialog создаёт сервис диалогов.
func NewDialog(store storage.Store) *Dialog { return &Dialog{store: store, now: time.Now} }

// Get читает состояние пользователя в out и возвращает имя состояния ("" если диалога нет).
func (d *Dialog) Get(ctx context.Context, userID int64, out any) (string, error) {
	state, data, err := d.store.Repos().FSM.Get(ctx, userID)
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if out != nil {
		if err := json.Unmarshal([]byte(data), out); err != nil {
			return "", err
		}
	}
	return state, nil
}

// Set сохраняет состояние.
func (d *Dialog) Set(ctx context.Context, userID int64, state string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return d.store.Repos().FSM.Set(ctx, userID, state, string(b), d.now().UTC())
}

// Clear завершает диалог.
func (d *Dialog) Clear(ctx context.Context, userID int64) error {
	return d.store.Repos().FSM.Clear(ctx, userID)
}
