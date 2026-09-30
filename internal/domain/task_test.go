package domain

import "testing"

func TestCanTransition(t *testing.T) {
	all := []TaskStatus{TaskCreated, TaskSent, TaskAccepted, TaskDeclined, TaskReported, TaskExpired, TaskReviewed, TaskCancelled, TaskRework}
	// Полный список разрешённых переходов; всё остальное должно быть запрещено.
	allowed := map[[2]TaskStatus]bool{
		{TaskCreated, TaskSent}:       true,
		{TaskSent, TaskAccepted}:      true,
		{TaskSent, TaskDeclined}:      true,
		{TaskAccepted, TaskReported}:  true,
		{TaskAccepted, TaskExpired}:   true,
		{TaskAccepted, TaskCancelled}: true,
		{TaskExpired, TaskCancelled}:  true,
		{TaskExpired, TaskReported}:   true,
		{TaskReported, TaskReviewed}:  true,
		{TaskReported, TaskRework}:    true,
		{TaskRework, TaskReported}:    true,
		{TaskRework, TaskCancelled}:   true,
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[[2]TaskStatus{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("%s -> %s: получили %v, ожидали %v", from, to, got, want)
			}
		}
	}
}

func TestIsActive(t *testing.T) {
	tests := map[TaskStatus]bool{
		TaskCreated: true, TaskSent: true, TaskAccepted: true, TaskExpired: true,
		TaskDeclined: false, TaskReported: false, TaskReviewed: false, TaskCancelled: false, TaskRework: true,
	}
	for s, want := range tests {
		if got := s.IsActive(); got != want {
			t.Errorf("%s.IsActive() = %v, ожидали %v", s, got, want)
		}
	}
}
