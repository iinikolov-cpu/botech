package domain

import (
	"reflect"
	"testing"
)

func TestKindsRoundTrip(t *testing.T) {
	tests := []struct {
		in   string
		want []TaskKind
		join string
	}{
		{"", nil, ""},
		{"buyer", []TaskKind{KindBuyer}, "buyer"},
		{"seller,buyer", []TaskKind{KindSeller, KindBuyer}, "buyer,seller"}, // порядок записи всегда один
		{"buyer,buyer,boss, seller ", []TaskKind{KindBuyer, KindSeller}, "buyer,seller"},
		{"admin", nil, ""},
	}
	for _, tc := range tests {
		got := ParseKinds(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseKinds(%q) = %v, ожидали %v", tc.in, got, tc.want)
		}
		if j := JoinKinds(got); j != tc.join {
			t.Errorf("JoinKinds(%v) = %q, ожидали %q", got, j, tc.join)
		}
	}
}

func TestUserCanDo(t *testing.T) {
	u := &User{Kinds: []TaskKind{KindSeller}}
	if u.CanDo(KindBuyer) || !u.CanDo(KindSeller) {
		t.Fatal("CanDo должен учитывать выбранные типы")
	}
	if (&User{}).CanDo(KindBuyer) || (*User)(nil).CanDo(KindBuyer) {
		t.Fatal("без выбора ничего делать нельзя")
	}
	if got := (&User{}).KindsTitle(); got != "не выбраны" {
		t.Fatalf("подпись без выбора: %q", got)
	}
	if got := (&User{Kinds: AllKinds}).KindsTitle(); got != "покупатель, продавец" {
		t.Fatalf("подпись обоих типов: %q", got)
	}
}
