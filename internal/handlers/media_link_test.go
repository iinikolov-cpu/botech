package handlers

import "testing"

func TestParseMediaPayload(t *testing.T) {
	tests := []struct {
		in   string
		id   int64
		key  string
		want bool
	}{
		{"m12xphoto", 12, "photo", true},
		{"m7xphoto_1-b", 7, "photo_1-b", true},
		{"m5xx", 5, "x", true},
		{"", 0, "", false},
		{"12xphoto", 0, "", false},   // нет префикса m
		{"m12", 0, "", false},        // нет ключа
		{"m12x", 0, "", false},       // пустой ключ
		{"mabcxphoto", 0, "", false}, // номер не число
		{"m0xphoto", 0, "", false},   // номер должен быть положительным
	}
	for _, tc := range tests {
		id, key, ok := parseMediaPayload(tc.in)
		if ok != tc.want || id != tc.id || key != tc.key {
			t.Errorf("parseMediaPayload(%q) = %d %q %v; ожидали %d %q %v", tc.in, id, key, ok, tc.id, tc.key, tc.want)
		}
	}
}

func TestStartPayload(t *testing.T) {
	for in, want := range map[string]string{"/start": "", "/start m1xa": "m1xa", "/start@bot m1xa": "m1xa", "привет": "", "/help x": ""} {
		if got := startPayload(in); got != want {
			t.Errorf("startPayload(%q) = %q, ожидали %q", in, got, want)
		}
	}
}
