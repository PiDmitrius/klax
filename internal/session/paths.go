package session

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyDir is a filesystem-safe, injective directory name for an account key.
func KeyDir(key string) string {
	hint := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, key)
	if len(hint) > 32 {
		hint = hint[:32]
	}
	return hint + "--" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func WorkDir(key, klaxID string) string {
	return filepath.Join(StoreDir(), "sessions", KeyDir(key), klaxID)
}

// Key migration runs before stores are opened. A directory already at the destination is
// resumed after an interrupted migration; conflicting directories are never overwritten.
func moveSessionDirs(oldKey, targetKey string, sessions []*Session) error {
	for _, sess := range sessions {
		src, dst := WorkDir(oldKey, sess.KlaxID), WorkDir(targetKey, sess.KlaxID)
		_, srcErr := os.Lstat(src)
		_, dstErr := os.Lstat(dst)
		if srcErr != nil && !os.IsNotExist(srcErr) {
			return srcErr
		}
		if dstErr != nil && !os.IsNotExist(dstErr) {
			return dstErr
		}
		if srcErr == nil && dstErr == nil {
			return fmt.Errorf("session migration: both %s and %s exist", src, dst)
		}
		if os.IsNotExist(srcErr) && os.IsNotExist(dstErr) {
			continue
		}
		if srcErr == nil {
			if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
				return err
			}
			if err := syncDir(filepath.Dir(filepath.Dir(dst))); err != nil {
				return err
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
		for _, parent := range []string{filepath.Dir(src), filepath.Dir(dst)} {
			if err := syncDir(parent); err != nil {
				return err
			}
		}
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
