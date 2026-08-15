package cache

import (
	"strings"
	"testing"
	"time"
)

func TestSuccessRoundTrip(t *testing.T) {
	t.Parallel()

	store := Store{Root: t.TempDir()}
	record := Record{Passed: true, CreatedAt: time.Unix(1, 0).UTC()}
	if err := store.PutSuccess("abc", record); err != nil {
		t.Fatalf("PutSuccess() error = %v", err)
	}
	got, ok, err := store.GetSuccess("abc")
	if err != nil {
		t.Fatalf("GetSuccess() error = %v", err)
	}
	if !ok || !got.Passed || !got.CreatedAt.Equal(record.CreatedAt) {
		t.Fatalf("GetSuccess() = %#v, %v", got, ok)
	}
}

func TestRunnerDigestIsStable(t *testing.T) {
	t.Parallel()

	first, err := RunnerDigest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunnerDigest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 || strings.Trim(first, "0123456789abcdef") != "" {
		t.Fatalf("RunnerDigest() = %q, %q", first, second)
	}
}

func TestFailuresAreNotStored(t *testing.T) {
	t.Parallel()

	store := Store{Root: t.TempDir()}
	if err := store.PutSuccess("abc", Record{Passed: false}); err == nil {
		t.Fatal("PutSuccess() accepted a failed record")
	}
}
