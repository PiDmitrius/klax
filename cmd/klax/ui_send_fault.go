package main

import (
	"net/http"
	"sync"
)

// KLAX_UI_SEND_TEST=1 cycles valid sends per user through rejection, waiting for
// client cancellation, and normal admission. Injected failures never enqueue.
type uiSendTest struct {
	enabled  bool
	mu       sync.Mutex
	attempts map[string]uint64
}

func (s *uiSendTest) intercept(w http.ResponseWriter, r *http.Request, user string) bool {
	if !s.enabled {
		return false
	}
	s.mu.Lock()
	if s.attempts == nil {
		s.attempts = make(map[string]uint64)
	}
	s.attempts[user]++
	step := s.attempts[user] % 3
	s.mu.Unlock()
	switch step {
	case 1:
		http.Error(w, "Тест отправки: быстрый отказ (1/3). Сообщение не принято.", http.StatusServiceUnavailable)
		return true
	case 2:
		<-r.Context().Done()
		return true
	default:
		return false
	}
}
