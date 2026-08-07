package blobstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFSStore_PutGetDelete_RoundTrip(t *testing.T) {
	root := t.TempDir()
	store, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	if err := store.Put(ctx, "tenant-a/Attachment/rec1/hello.txt", strings.NewReader("hello world")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := store.Get(ctx, "tenant-a/Attachment/rec1/hello.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}

	if err := store.Delete(ctx, "tenant-a/Attachment/rec1/hello.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "tenant-a/Attachment/rec1/hello.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist after delete, got %v", err)
	}
}

func TestFSStore_Put_OverwritesExistingKey(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	if err := store.Put(ctx, "k", strings.NewReader("v1")); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := store.Put(ctx, "k", strings.NewReader("v2-longer")); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	rc, err := store.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "v2-longer" {
		t.Fatalf("expected overwrite to replace contents, got %q", got)
	}
}

func TestFSStore_Get_MissingKey_ReturnsNotExist(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	if _, err := store.Get(context.Background(), "never/written"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestFSStore_Delete_MissingKey_IsNotAnError(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	if err := store.Delete(context.Background(), "never/written"); err != nil {
		t.Fatalf("Delete of a missing key should be a no-op, got %v", err)
	}
}

// TestFSStore_RejectsPathTraversal is the adversarial test ADR-0024 and
// uc-infra#142's acceptance criteria both call for explicitly — every
// key here is crafted to try to escape root, and every one must be
// refused with ErrInvalidKey before any filesystem operation happens
// (confirmed by checking nothing was written outside root, not just by
// checking the returned error).
func TestFSStore_RejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	// A real, sensitive file outside root that a successful escape would
	// be able to read/overwrite — its contents are asserted unchanged
	// after every attempt below.
	outsideDir := t.TempDir()
	secretPath := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("do not touch"), 0o600); err != nil {
		t.Fatalf("seed secret file: %v", err)
	}

	store, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	relOutside, err := filepath.Rel(root, secretPath)
	if err != nil {
		t.Fatalf("compute relative escape path: %v", err)
	}

	traversalKeys := []string{
		"../secret.txt",
		"../../etc/passwd",
		"a/../../secret.txt",
		relOutside, // however many "../" filepath.Rel actually computes
		secretPath, // absolute path straight to the file outside root
		"/etc/passwd",
		"..",
		"../",
		string(filepath.Separator) + "..",
		"a/b/../../../secret.txt",
	}

	for _, key := range traversalKeys {
		t.Run(key, func(t *testing.T) {
			if err := store.Put(ctx, key, strings.NewReader("pwned")); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Put(%q): expected ErrInvalidKey, got %v", key, err)
			}
			if _, err := store.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Get(%q): expected ErrInvalidKey, got %v", key, err)
			}
			if err := store.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Delete(%q): expected ErrInvalidKey, got %v", key, err)
			}
		})
	}

	// The secret file outside root must be untouched by any attempt above.
	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("re-read secret file: %v", err)
	}
	if string(got) != "do not touch" {
		t.Fatalf("secret file outside root was modified: %q", got)
	}
}

// TestFSStore_RejectsSiblingDirectoryPrefixCollision is the specific
// "strings.HasPrefix isn't enough" case resolvePath's own doc comment
// calls out: a sibling directory whose NAME has root's path as a string
// prefix must not be treated as being "under" root.
func TestFSStore_RejectsSiblingDirectoryPrefixCollision(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "attachments")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	store, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	sibling := filepath.Join(parent, "attachments-evil")
	if err := os.MkdirAll(sibling, 0o750); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}

	// Key computed to land exactly on the sibling directory once
	// filepath.Join(root, key) is cleaned: "../attachments-evil/x".
	key := "../attachments-evil/x"
	if err := store.Put(context.Background(), key, strings.NewReader("pwned")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey for sibling-prefix-collision key, got %v", err)
	}
	if entries, _ := os.ReadDir(sibling); len(entries) != 0 {
		t.Fatalf("sibling directory outside root was written to: %v", entries)
	}
}

func TestFSStore_RejectsEmptyAndNulKey(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	for _, key := range []string{"", "has\x00nul"} {
		if err := store.Put(ctx, key, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Put(%q): expected ErrInvalidKey, got %v", key, err)
		}
	}
}

func TestFSStore_RejectsKeyResolvingToRootItself(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	if err := store.Put(context.Background(), ".", strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Put(\".\"): expected ErrInvalidKey, got %v", err)
	}
}

func TestFSStore_Put_CreatesNestedParentDirectories(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, "a/b/c/d.txt", strings.NewReader("nested")); err != nil {
		t.Fatalf("Put nested key: %v", err)
	}
	rc, err := store.Get(ctx, "a/b/c/d.txt")
	if err != nil {
		t.Fatalf("Get nested key: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "nested" {
		t.Fatalf("got %q", got)
	}
}

func TestFSStore_Put_LeavesNoTempFileOnSuccess(t *testing.T) {
	root := t.TempDir()
	store, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	if err := store.Put(context.Background(), "f.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "f.txt" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly [f.txt] in root after Put, got %v (stray temp file?)", names)
	}
}

func TestNewFSStore_EmptyRoot_Errors(t *testing.T) {
	if _, err := NewFSStore(""); err == nil {
		t.Fatal("expected error for empty root")
	}
}

func TestNewFSStore_CreatesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	store, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	if err := store.Put(context.Background(), "f", strings.NewReader("x")); err != nil {
		t.Fatalf("Put after creating missing root: %v", err)
	}
}

// TestFSStore_ContextCanceled_RefusesBeforeTouchingDisk confirms every
// method respects an already-canceled context rather than silently doing
// the I/O anyway — Put in particular must not partially write.
func TestFSStore_ContextCanceled_RefusesBeforeTouchingDisk(t *testing.T) {
	root := t.TempDir()
	store, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Put(ctx, "f", strings.NewReader("x")); err == nil {
		t.Fatal("expected Put to refuse a canceled context")
	}
	if _, err := store.Get(ctx, "f"); err == nil {
		t.Fatal("expected Get to refuse a canceled context")
	}
	if err := store.Delete(ctx, "f"); err == nil {
		t.Fatal("expected Delete to refuse a canceled context")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("expected no files written under a canceled context, got %v", entries)
	}
}

func TestFSStore_Put_LargeBlobRoundTrips(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	if err := store.Put(ctx, "big", bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put large blob: %v", err)
	}
	rc, err := store.Get(ctx, "big")
	if err != nil {
		t.Fatalf("Get large blob: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read large blob: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("large blob round-trip mismatch")
	}
}
