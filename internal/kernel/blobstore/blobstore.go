// Package blobstore stores and retrieves opaque byte blobs (uploaded
// files) behind a small interface, so the storage backend is a swappable
// implementation detail rather than something every caller couples to
// directly (ADR-0024, uc-infra#142). FSStore — a self-hosted,
// filesystem-backed implementation — is the only Store today; a future
// MinIO/S3-compatible backend is a later, deliberately deferred decision
// (needs infra provisioning + a credential, Farshid's call) that slots in
// behind this same interface without touching any caller.
//
// This package knows nothing about tenants, entities, or HTTP — every
// key it's given is an opaque string. Tenant namespacing (a key's
// leading path segment identifying which tenant it belongs to) and the
// requirement to re-verify that segment against the requesting tenant on
// every read live in internal/api/attachment.go, not here (ADR-0024 §2)
// — this package's own job is only "don't let any key escape root,"
// which it enforces unconditionally regardless of what the key claims to
// be.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalidKey is returned by every Store method when key is empty,
// contains a NUL byte, or resolves outside the store's root — a
// caller-side bug (a key should always come from server-computed input,
// never straight from a client), reported as a distinct sentinel so
// tests and callers can assert on it specifically rather than string-
// matching an error message.
var ErrInvalidKey = errors.New("blobstore: invalid key")

// Store is the storage backend interface every caller programs against.
// A key is an opaque, slash-separated path-like string chosen entirely
// by the caller (internal/api/attachment.go computes one server-side per
// upload) — Store itself assigns no meaning to any segment of it beyond
// "where do these bytes live."
type Store interface {
	// Put durably writes r's full contents under key, replacing any
	// existing blob at that key. It does not return until the write is
	// durable (FSStore fsyncs before the final rename — see its own doc
	// comment), so a caller that gets a nil error may safely create a
	// database record referencing key next.
	Put(ctx context.Context, key string, r io.Reader) error

	// Get opens the blob stored at key for reading. The caller must
	// Close the returned ReadCloser. Returns an error wrapping
	// os.ErrNotExist (checkable via errors.Is) if no blob exists at key.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the blob stored at key. Deleting a key that does
	// not exist is not an error — callers use this for orphan cleanup
	// (e.g. a blob written but the record referencing it then failed to
	// create), where "already gone" and "never existed" are the same
	// outcome from the caller's point of view.
	Delete(ctx context.Context, key string) error
}

// FSStore is a Store backed by a directory on the local filesystem —
// self-hosted, no external credential, the same posture as the rest of
// this product's infra (ADR-0024). One FSStore serves every tenant; the
// tenant-namespacing discipline is enforced by callers (see this
// package's own doc comment), not by FSStore itself.
type FSStore struct {
	root string
}

// NewFSStore builds an FSStore rooted at root, creating the directory
// (and any missing parents) if it doesn't already exist. root is
// resolved to an absolute, symlink-free path once here — not
// re-resolved per call — so every subsequent key-containment check in
// resolvePath compares against a stable, canonical boundary rather than
// re-deriving it (and potentially getting a different answer) on every
// Put/Get/Delete.
func NewFSStore(root string) (*FSStore, error) {
	if root == "" {
		return nil, fmt.Errorf("blobstore: root must not be empty")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("blobstore: create root %q: %w", root, err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blobstore: resolve root %q: %w", root, err)
	}
	// EvalSymlinks requires the path to exist, which MkdirAll above just
	// guaranteed — resolving it now means a symlink swapped in later
	// under root's own parent can't retroactively change what "inside
	// root" means for a path computed against this FSStore.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("blobstore: resolve root %q: %w", root, err)
	}
	return &FSStore{root: resolved}, nil
}

// resolvePath turns key into an absolute filesystem path guaranteed to
// be under s.root, or returns ErrInvalidKey. Three independent checks,
// not one — see the doc comment on each for why a single check isn't
// enough:
//
//  1. key must not be absolute. filepath.Join(s.root, key) would in fact
//     safely CONTAIN an absolute-looking key ("/etc/passwd" joins to
//     "root/etc/passwd", not "/etc/passwd" — Join treats a second
//     absolute-looking argument as just another path segment on every
//     platform this ships to) rather than escape root, so skipping this
//     check would not itself be a traversal bug. It's rejected anyway,
//     explicitly, because a caller passing an absolute-looking key is
//     always either a bug or an attempt, and silently reinterpreting it
//     as "somewhere under root, but not where you asked" is a more
//     confusing contract than refusing it outright — every real key this
//     package is ever given is server-computed and relative (see this
//     package's own doc comment).
//  2. filepath.Join(s.root, key) then filepath.Clean collapses any
//     "..".
//  3. The joined-and-cleaned result must be s.root itself or a path
//     under it. A plain strings.HasPrefix(joined, s.root) is NOT
//     sufficient on its own: "/data/attachments-evil" has
//     "/data/attachments" as a string prefix without being a path under
//     it. The check below requires the byte immediately after the
//     prefix to be the OS path separator (or the joined path to equal
//     root exactly), which "-evil" fails.
func (s *FSStore) resolvePath(key string) (string, error) {
	if key == "" || strings.ContainsRune(key, 0) {
		return "", ErrInvalidKey
	}
	if filepath.IsAbs(key) {
		return "", ErrInvalidKey
	}
	joined := filepath.Clean(filepath.Join(s.root, key))
	if joined == s.root {
		return "", ErrInvalidKey // key resolved to root itself, not a file under it
	}
	if !strings.HasPrefix(joined, s.root+string(filepath.Separator)) {
		return "", ErrInvalidKey
	}
	return joined, nil
}

// Put implements Store. The blob is written to a temp file alongside its
// final path and renamed into place after an explicit Sync — a reader
// that opens key concurrently either sees the old contents (if any) or
// the complete new ones, never a partial write, and the rename only
// happens after the bytes are confirmed durable on disk.
func (s *FSStore) Put(ctx context.Context, key string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolvePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("blobstore: create parent dir for %q: %w", key, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blobstore-tmp-*")
	if err != nil {
		return fmt.Errorf("blobstore: create temp file for %q: %w", key, err)
	}
	tmpPath := tmp.Name()
	// Clean up the temp file on any failure path below; once Rename
	// succeeds tmpPath no longer exists, so this Remove is a harmless
	// no-op in the success case.
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return fmt.Errorf("blobstore: write %q: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("blobstore: sync %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blobstore: close %q: %w", key, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("blobstore: finalize %q: %w", key, err)
	}
	return nil
}

// Get implements Store.
func (s *FSStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.resolvePath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("blobstore: open %q: %w", key, err)
	}
	return f, nil
}

// Delete implements Store. Deleting an already-absent key is not an
// error (see the Store interface doc comment).
func (s *FSStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolvePath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blobstore: delete %q: %w", key, err)
	}
	return nil
}
