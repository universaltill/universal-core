// Generic attachment upload/download (uc-infra#142, ADR-0024): a
// self-hosted filesystem blob store (internal/kernel/blobstore) behind
// two entity-agnostic HTTP endpoints, wired through the existing
// foundation.Attachment entity Definition and the same RBAC-guarded
// crud.Engine every other handler in this file uses — no
// `if entityType == "..."` here, same discipline as createRecord.
//
// Screen recording (uc-infra#92) and any other future file-carrying
// feature build on this directly; it is not itself entity-specific.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/entity"
)

// maxAttachmentBytes bounds a single generic attachment upload — same
// "cap it at the HTTP boundary" reasoning maxUploadBytes (import.go) and
// maxVoiceNoteBytes (issuereport.go) already document. Sized for this
// slice's own scope (arbitrary small-to-medium files attached to a
// record), not video: uc-infra#92's screen-recording follow-up will need
// its own, larger cap for that specific case, not inherited from here.
const maxAttachmentBytes = 20 << 20 // 20 MiB

// attachmentKeyRandomBytes is the number of random bytes hex-encoded
// into each stored blob's key, making it unguessable even by a caller
// who knows the tenant/entity/record it belongs to (see the key shape
// documented on attachmentUpload below).
const attachmentKeyRandomBytes = 16

// attachmentUpload handles POST /api/attachments/{entityType}/{recordID}:
// a multipart/form-data body with a "file" field, attached to an
// existing record identified by entityType+recordID (the same generic
// entity-agnostic addressing foundation.Attachment's own doc comment
// documents for entity_type/record_id).
//
// Order of operations, and why it's this order:
//  1. Resolve the TARGET entity's Definition and confirm the target
//     record actually exists and is readable by this actor (ts.crud.Get
//     is RBAC-guarded) — refusing before ever touching the upload body
//     means a caller can't use this endpoint to probe for the existence
//     of records they can't read, and can't burn storage attaching files
//     to nothing.
//  2. Parse the multipart body (bounded by maxAttachmentBytes).
//  3. Compute storage_path server-side — never trust a client-supplied
//     value, there isn't one accepted here in the first place.
//  4. Store the blob durably (blobstore.Put) BEFORE creating the
//     Attachment record, so a record is never created pointing at bytes
//     that don't exist.
//  5. Create the Attachment record via the normal RBAC-guarded
//     crud.Create + entity.ValidateRecord path, same as any other
//     record write in this file. If this fails, the just-written blob is
//     deleted (orphan cleanup) rather than left stranded.
func (h *Handler) attachmentUpload(w http.ResponseWriter, r *http.Request) {
	if h.blobstore == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "attachment storage is not configured for this deployment")
		return
	}
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}

	targetEntityType := r.PathValue("entityType")
	targetRecordID := r.PathValue("recordID")
	if !isValidID(targetRecordID) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid record id")
		return
	}

	targetDef, err := ts.entityDef(r.Context(), targetEntityType)
	if err != nil {
		writeDefinitionLookupError(w, targetEntityType, err)
		return
	}
	if _, err := ts.crud.Get(r.Context(), targetDef, targetRecordID); err != nil {
		if errors.Is(err, data.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found", targetEntityType, targetRecordID))
			return
		}
		writeCrudError(w, fmt.Sprintf("resolve attachment target %s %s", targetEntityType, targetRecordID), err)
		return
	}

	attachmentDef, err := ts.entityDef(r.Context(), "Attachment")
	if err != nil {
		writeDefinitionLookupError(w, "Attachment", err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes)
	// multipartParseMemory (not maxAttachmentBytes) as ParseMultipartForm's
	// maxMemory bound — see that constant's own doc comment for why. This
	// call site gets the FULL benefit of spilling: h.blobstore.Put below
	// streams file via io.Copy, so a spilled part is never re-materialized
	// in heap at all, unlike readUploadedFile's own call site in import.go.
	if err := r.ParseMultipartForm(multipartParseMemory); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("invalid or oversized upload (max %d MiB): %s", maxAttachmentBytes>>20, err.Error()))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, `no file uploaded (expected a "file" form field)`)
		return
	}
	defer file.Close()

	key, err := attachmentStorageKey(rc.TenantID, targetEntityType, targetRecordID, header.Filename)
	if err != nil {
		writeInternalError(w, "compute attachment storage key", err)
		return
	}

	if err := h.blobstore.Put(r.Context(), key, file); err != nil {
		writeInternalError(w, "store attachment blob", err)
		return
	}

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	fields := map[string]any{
		"entity_type":  targetEntityType,
		"record_id":    targetRecordID,
		"file_name":    header.Filename,
		"mime_type":    mimeType,
		"size_bytes":   float64(header.Size),
		"storage_path": key,
	}

	if err := entity.ValidateRecord(attachmentDef, fields); err != nil {
		h.cleanupOrphanBlob(r.Context(), key) // orphan cleanup — see doc comment step 5
		// nil: this handler never calls EffectiveWriteFields, so no field
		// here can be one restored from a hidden stored value (see
		// writeValidationErrorLocalized's own doc comment, uc-infra#178).
		h.writeValidationErrorLocalized(w, r, "validate new Attachment", err, nil)
		return
	}
	rec, err := ts.crud.Create(r.Context(), attachmentDef, fields, rc.Actor)
	if err != nil {
		h.cleanupOrphanBlob(r.Context(), key) // orphan cleanup — see doc comment step 5
		h.writeCrudErrorLocalized(w, r, "create Attachment record", err)
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, toRecordResponse(rec))
}

// cleanupOrphanBlob deletes a just-written blob after the Attachment
// record referencing it failed to create (attachmentUpload's step 5).
// Deliberately uses context.WithoutCancel(ctx) rather than ctx itself:
// if Create failed BECAUSE the client disconnected (context canceled),
// handing that same canceled context to Delete would make it refuse
// immediately (blobstore.FSStore checks ctx.Err() up front, same as
// every other method) and no-op — silently leaving the blob durably
// orphaned on exactly the failure path this cleanup exists for.
// WithoutCancel (not context.Background()) keeps any request-scoped
// values a future caller might attach while still stripping
// cancellation. Cleanup is best-effort either way: a failure here is
// logged, not surfaced to the client, since the client already has its
// own real error to report.
func (h *Handler) cleanupOrphanBlob(ctx context.Context, key string) {
	if err := h.blobstore.Delete(context.WithoutCancel(ctx), key); err != nil {
		log.Printf("api: cleanup orphaned attachment blob %q: %v", key, err)
	}
}

// attachmentDownload handles GET /api/attachments/{id}: streams the
// blob a previously-uploaded Attachment record points at.
//
// storage_path is NOT trusted at face value even though rec was just
// read through the normal RBAC-guarded path (ADR-0024 §2): Attachment is
// an ordinary entity, so its storage_path field is still writable
// arbitrary text via the generic POST /api/records/Attachment route
// (entity.Definition has no "server-computed, client can't set" concept
// yet). A same-tenant caller could create their own, legitimately-theirs
// Attachment record naming a storage_path under a DIFFERENT tenant's
// namespace — nothing about per-tenant database isolation (ADR-0003)
// catches that, because the forged record lives in the requester's own
// tenant database. So this handler independently re-derives the expected
// tenant-namespace prefix from the REQUESTING tenant (rc.TenantID, from
// the authenticated session, never from the stored record) and refuses
// to serve anything whose storage_path doesn't start with it —
// regardless of what the record claims.
func (h *Handler) attachmentDownload(w http.ResponseWriter, r *http.Request) {
	if h.blobstore == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "attachment storage is not configured for this deployment")
		return
	}
	rc, ok := requestContext(w, r)
	if !ok {
		return
	}
	ts, err := h.scope(r.Context(), rc)
	if err != nil {
		writeInternalError(w, "resolve tenant scope", err)
		return
	}

	id := r.PathValue("id")
	if !isValidID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid attachment id")
		return
	}

	attachmentDef, err := ts.entityDef(r.Context(), "Attachment")
	if err != nil {
		writeDefinitionLookupError(w, "Attachment", err)
		return
	}
	rec, err := ts.crud.Get(r.Context(), attachmentDef, id)
	if err != nil {
		if errors.Is(err, data.ErrNotFound) {
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("Attachment %q not found", id))
			return
		}
		writeCrudError(w, fmt.Sprintf("get Attachment %s", id), err)
		return
	}

	storagePath, _ := rec.Data["storage_path"].(string)
	if !storagePathBelongsToTenant(storagePath, rc.TenantID) {
		// Deliberately the same "access denied" shape as any other authz
		// refusal in this file (writeCrudError's authz.ErrDenied branch)
		// — a tenant-namespace mismatch on an otherwise-valid,
		// same-tenant record is functionally the same class of denial,
		// and the client learns nothing about why.
		log.Printf("api: attachment %s storage_path %q does not resolve under requesting tenant %s — refusing", id, storagePath, rc.TenantID)
		httpx.WriteError(w, http.StatusForbidden, "access denied")
		return
	}

	blob, err := h.blobstore.Get(r.Context(), storagePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("Attachment %q blob not found", id))
			return
		}
		writeInternalError(w, fmt.Sprintf("open attachment blob for %s", id), err)
		return
	}
	defer blob.Close()
	if statter, ok := blob.(interface{ Stat() (os.FileInfo, error) }); ok {
		if fi, err := statter.Stat(); err == nil && fi.IsDir() {
			// storagePath resolved to a directory, not a file — os.Open
			// succeeds on a directory on Linux, and io.Copy below would
			// otherwise fail AFTER WriteHeader(200) already committed the
			// status, leaving the client with a 200 and an empty body.
			// Only reachable via a forged storage_path (nothing this
			// package itself ever writes is a bare directory path), so
			// this is the same "malformed/forged reference" class as the
			// tenant-mismatch case above, not a real not-found.
			httpx.WriteError(w, http.StatusNotFound, fmt.Sprintf("Attachment %q blob not found", id))
			return
		}
	}

	fileName, _ := rec.Data["file_name"].(string)
	mimeType, _ := rec.Data["mime_type"].(string)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mimeType)
	// mimeType is client-supplied (the upload's own Content-Type part
	// header, stored verbatim) — nosniff stops a browser from ever
	// re-interpreting a served blob as something more dangerous than
	// what it's labeled, regardless of what that label claims.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if fileName != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, blob)
}

// storagePathBelongsToTenant is the tenant-namespace check ADR-0024 §2
// requires: true only if storagePath, taken at face value, resolves
// under wantTenantID's own segment.
//
// This is NOT a plain strings.HasPrefix(storagePath, wantTenantID+"/")
// — that was the pre-fix version of this check, and it was a real,
// exploitable bypass (found by independent review): storagePath is
// resolved by blobstore.FSStore via filepath.Join+Clean, which COLLAPSES
// ".." elements, but a raw prefix check does not. A forged storage_path
// of "{ownTenant}/../{victimTenant}/..." starts with the caller's own
// tenant id — passing a naive prefix check — while resolving, once
// cleaned, into the VICTIM tenant's real directory. So this function
// requires storagePath to already be in canonical (path.Clean'd) form
// before ever comparing it to the tenant prefix: any ".." or "."
// segment, or redundant slash, fails closed rather than being resolved
// away downstream. path.Clean (not filepath.Clean) because a
// blobstore key is always "/"-separated by construction (see
// attachmentStorageKey), independent of the host OS.
func storagePathBelongsToTenant(storagePath, wantTenantID string) bool {
	if storagePath == "" || wantTenantID == "" {
		return false
	}
	if path.Clean(storagePath) != storagePath {
		return false
	}
	return strings.HasPrefix(storagePath, wantTenantID+"/")
}

// attachmentStorageKey computes the server-side blobstore key for one
// uploaded file: {tenantID}/{entityType}/{recordID}/{random-hex}-{name}.
// The tenantID segment is what attachmentDownload's tenant-namespace
// check (ADR-0024 §2) verifies against; the random component makes the
// key unguessable independent of that check (defense in depth — even a
// caller who knows another tenant's id can't derive a real key from it
// alone); the sanitized original filename is kept only for a human
// skimming the filesystem directly, never trusted for anything else.
func attachmentStorageKey(tenantID, entityType, recordID, fileName string) (string, error) {
	randBytes := make([]byte, attachmentKeyRandomBytes)
	if _, err := rand.Read(randBytes); err != nil {
		return "", fmt.Errorf("generate random key component: %w", err)
	}
	return fmt.Sprintf("%s/%s/%s/%s-%s", tenantID, entityType, recordID, hex.EncodeToString(randBytes), sanitizeAttachmentFileName(fileName)), nil
}

// maxAttachmentFileNameLength bounds the sanitized filename component
// baked into a storage key. Without a cap, a very long client-supplied
// filename (a few hundred bytes is enough) produces a final path
// component past most filesystems' NAME_MAX, which fails Put with an
// opaque os.Rename ENAMETOOLONG error — a genuinely bad request (400),
// surfacing today as a 500. Well above any real filename in practice;
// this only ever trims the pathological case.
const maxAttachmentFileNameLength = 200

// sanitizeAttachmentFileName strips everything from a client-supplied
// filename except the base name, further strips path separators, and
// caps its length — belt-and-suspenders alongside blobstore.FSStore's
// own independent path-containment check, so a malicious
// "../../etc/cron.d/evil" upload filename can't influence the key's
// directory structure at all, even before it ever reaches FSStore.
func sanitizeAttachmentFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	if len(name) > maxAttachmentFileNameLength {
		name = name[:maxAttachmentFileNameLength]
	}
	return name
}
