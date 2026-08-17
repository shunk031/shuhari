package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	runnerDigestOnce sync.Once
	runnerDigest     string
	runnerDigestErr  error
)

func RunnerDigest() (string, error) {
	runnerDigestOnce.Do(func() {
		executable, err := os.Executable()
		if err != nil {
			runnerDigestErr = fmt.Errorf("find Shuhari executable: %w", err)
			return
		}
		contents, err := os.ReadFile(executable)
		if err != nil {
			runnerDigestErr = fmt.Errorf("read Shuhari executable: %w", err)
			return
		}
		digest := sha256.Sum256(contents)
		runnerDigest = hex.EncodeToString(digest[:])
	})
	return runnerDigest, runnerDigestErr
}

type Record struct {
	Passed    bool      `json:"passed"`
	CreatedAt time.Time `json:"created_at"`
	Workspace string    `json:"workspace,omitempty"`
}

type Store struct {
	Root string
}

func DefaultStore() (Store, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return Store{}, fmt.Errorf("find user cache directory: %w", err)
	}
	return Store{Root: filepath.Join(root, "shuhari", "v2")}, nil
}

func Key(parts ...[]byte) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write(part)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s Store) GetSuccess(key string) (Record, bool, error) {
	contents, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read cache: %w", err)
	}
	var record Record
	if err := json.Unmarshal(contents, &record); err != nil {
		return Record{}, false, fmt.Errorf("decode cache: %w", err)
	}
	if !record.Passed {
		return Record{}, false, nil
	}
	return record, true, nil
}

func (s Store) PutSuccess(key string, record Record) error {
	if !record.Passed {
		return errors.New("refusing to cache a failed result")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	contents, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}
	temporary, err := os.CreateTemp(s.Root, ".cache-*")
	if err != nil {
		return fmt.Errorf("create cache file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write cache: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close cache: %w", err)
	}
	if err := os.Rename(temporaryName, s.path(key)); err != nil {
		return fmt.Errorf("replace cache: %w", err)
	}
	return nil
}

func (s Store) path(key string) string {
	return filepath.Join(s.Root, key+".json")
}
