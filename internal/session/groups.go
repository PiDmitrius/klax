package session

import (
	"errors"
	"sort"
	"strings"
	"unicode"
)

// Group names live in a URL fragment (`#work`, `#work/<created>`) that must stay unambiguous against
// two neighbours in the same namespace: a bare session id (`#1783809783`) and a computed view
// (`#is:unread`). Hence "not all digits" and "no colon" — they are the parse rule, not cosmetics.
const (
	MaxGroupLen   = 32
	MaxGroupCount = 16
)

// ValidateGroup reports why a group name is unusable, or nil. The caller is expected to have
// trimmed it.
func ValidateGroup(group string) error {
	switch {
	case group == "":
		return errors.New("Пустое имя группы")
	case len([]rune(group)) > MaxGroupLen:
		return errors.New("Имя группы длиннее 32 символов")
	case strings.ContainsAny(group, "/#:"):
		return errors.New("В имени группы нельзя использовать / # :")
	case group == "*":
		return errors.New("«*» зарезервирована под все сессии")
	case isAllDigits(group):
		return errors.New("Имя группы не может состоять только из цифр")
	}
	for _, r := range group {
		if unicode.IsControl(r) {
			return errors.New("В имени группы нельзя использовать управляющие символы")
		}
	}
	return nil
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// NormalizeGroups trims, drops empties, rejects invalid names and removes duplicates while KEEPING
// the caller's order — the field is a set, but the user's own order is the one the settings field
// shows back to them.
func NormalizeGroups(groups []string) ([]string, error) {
	out := make([]string, 0, len(groups))
	seen := make(map[string]bool, len(groups))
	for _, raw := range groups {
		group := strings.TrimSpace(raw)
		if group == "" {
			continue
		}
		if err := ValidateGroup(group); err != nil {
			return nil, err
		}
		if seen[group] {
			continue
		}
		seen[group] = true
		out = append(out, group)
	}
	if len(out) > MaxGroupCount {
		return nil, errors.New("Слишком много групп у одной сессии")
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// KnownGroups is the chat's group set: distinct names across its sessions, sorted for a stable list.
// There is no group registry — a group exists exactly as long as some session carries it.
func (s *Store) KnownGroups(chatID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	seen := map[string]bool{}
	out := []string{}
	for _, sess := range cs.Sessions {
		for _, group := range sess.Groups {
			if !seen[group] {
				seen[group] = true
				out = append(out, group)
			}
		}
	}
	// Case-insensitive: "Klax" and "dev" should read as one alphabet, not as two blocks. The
	// case-sensitive tiebreaker keeps names that differ only by case in a stable order.
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i]), strings.ToLower(out[j])
		if li != lj {
			return li < lj
		}
		return out[i] < out[j]
	})
	return out
}
