package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"botech/internal/domain"
)

const sellerYAML = `key: seller-test
kind: seller
title: Продавец
operator: BTS
steps: [Шаг]
questions:
  - {key: q_one, text: Вопрос, type: text}
`

// addUser создаёт активного исполнителя с заданными типами заданий.
func (e *env) addUser(t *testing.T, id int64, kinds ...domain.TaskKind) {
	t.Helper()
	now := time.Now().UTC()
	if err := e.store.Repos().Users.Create(context.Background(), &domain.User{
		TgID: id, Role: domain.RoleBuyer, Status: domain.StatusActive, Lang: "ru", Kinds: kinds, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSetKindsAndUsersForKind(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t) // пользователи 1..3 выбрали оба типа
	e.addUser(t, 10, domain.KindSeller)
	e.addUser(t, 11) // ещё не выбирал
	e.addUser(t, 12, domain.KindBuyer)

	buyers, nb, err := e.access.UsersForKind(ctx, domain.KindBuyer, 50, 0)
	if err != nil || nb != 4 || len(buyers) != 4 { // 1,2,3 и 12
		t.Fatalf("покупатели: %d %v", nb, err)
	}
	sellers, ns, _ := e.access.UsersForKind(ctx, domain.KindSeller, 50, 0)
	if ns != 4 || len(sellers) != 4 { // 1,2,3 и 10
		t.Fatalf("продавцы: %d", ns)
	}
	for _, u := range append(buyers, sellers...) {
		if u.TgID == 11 {
			t.Fatal("не выбравший пользователь не должен попадать в списки")
		}
	}

	if err := e.access.SetKinds(ctx, 11, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("пустой выбор: %v", err)
	}
	if err := e.access.SetKinds(ctx, 11, []domain.TaskKind{"boss"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("неизвестный тип: %v", err)
	}
	if err := e.access.SetKinds(ctx, 9999, domain.AllKinds); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет пользователя: %v", err)
	}
	if err := e.access.SetKinds(ctx, 11, []domain.TaskKind{domain.KindSeller, domain.KindSeller}); err != nil {
		t.Fatal(err)
	}
	u, _ := e.access.Lookup(ctx, 11)
	if len(u.Kinds) != 1 || u.Kinds[0] != domain.KindSeller {
		t.Fatalf("сохранённые типы: %v", u.Kinds)
	}
}

// Задание нельзя выдать пользователю, который не выбрал тип сценария (или вообще не выбирал).
func TestAssignRespectsKinds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addUser(t, 10, domain.KindSeller)
	e.addUser(t, 11) // не выбирал
	e.addUser(t, 12, domain.KindBuyer)
	buyerSc := e.importScenario(t, scenarioYAML).Scenario
	sellerSc := e.importScenario(t, sellerYAML).Scenario
	if buyerSc.Kind != domain.KindBuyer || sellerSc.Kind != domain.KindSeller {
		t.Fatalf("типы сценариев: %v %v", buyerSc.Kind, sellerSc.Kind)
	}

	tests := []struct {
		name      string
		scenario  int64
		users     []int64
		wantTasks int
		wrong     []int64
	}{
		{"покупательский: продавец и не выбравший отсеиваются", buyerSc.ID, []int64{12, 10, 11}, 1, []int64{10, 11}},
		{"продавец: покупатель отсеивается", sellerSc.ID, []int64{10, 12}, 1, []int64{12}},
		{"оба типа подходят везде", sellerSc.ID, []int64{1, 2}, 2, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.tasks.Assign(ctx, firstAdmin, tc.scenario, 3, tc.users)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Created) != tc.wantTasks || len(res.WrongKind) != len(tc.wrong) || len(res.Invalid) != 0 {
				t.Fatalf("создано %d, не тот тип %v, недоступны %v", len(res.Created), res.WrongKind, res.Invalid)
			}
			for i, id := range tc.wrong {
				if res.WrongKind[i] != id {
					t.Fatalf("не тот тип: %v, ожидали %v", res.WrongKind, tc.wrong)
				}
			}
		})
	}
}

// Тип сценария задаётся при создании: поменять его у существующего key нельзя.
func TestImportKindImmutable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.importScenario(t, sellerYAML)
	changed := strings.Replace(sellerYAML, "kind: seller", "kind: buyer", 1)
	res, problems, err := e.scenarios.Import(ctx, firstAdmin, []byte(changed))
	if err != nil || res != nil || len(problems) != 1 || !strings.Contains(problems[0], "поменять его") {
		t.Fatalf("смена типа: res=%v problems=%v err=%v", res, problems, err)
	}
	// Обычное обновление того же типа работает.
	again := strings.Replace(sellerYAML, "Шаг", "Новый шаг", 1)
	if r, p, err := e.scenarios.Import(ctx, firstAdmin, []byte(again)); err != nil || len(p) != 0 || r.Version.Version != 2 {
		t.Fatalf("новая версия: %v %v %v", r, p, err)
	}
}
