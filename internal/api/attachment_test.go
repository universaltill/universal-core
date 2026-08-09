package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/universaltill/universal-core/internal/data"
	"github.com/universaltill/universal-core/internal/httpx"
	"github.com/universaltill/universal-core/internal/kernel/blobstore"
	"github.com/universaltill/universal-core/internal/kernel/entity"
	"github.com/universaltill/universal-core/internal/kernel/foundation"
)

// publishEntityOnly is publishEntityAndForm without the form half —
// attachment.go never looks up a form Definition for Attachment (or for
// the target entity type), so tests in this file only need the entity
// side published.
func publishEntityOnly(t *testing.T, db *sql.DB, entDef *entity.Definition) {
	t.Helper()
	ctx := context.Background()
	actor := humanActor()
	raw, err := json.Marshal(entDef)
	if err != nil {
		t.Fatalf("marshal entity def: %v", err)
	}
	repo := data.NewEntityDefinitionRepo(db)
	if _, err := repo.CreateDraft(ctx, entDef.EntityType, entDef.Version, raw, actor); err != nil {
		t.Fatalf("CreateDraft %s: %v", entDef.EntityType, err)
	}
	if err := repo.Approve(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Approve %s: %v", entDef.EntityType, err)
	}
	if err := repo.Publish(ctx, entDef.EntityType, entDef.Version, actor); err != nil {
		t.Fatalf("Publish %s: %v", entDef.EntityType, err)
	}
}

// newFSStoreAt builds a real blobstore.FSStore rooted at root, failing
// the test on any construction error.
func newFSStoreAt(t *testing.T, root string) blobstore.Store {
	t.Helper()
	store, err := blobstore.NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore(%s): %v", root, err)
	}
	return store
}

func newMultipartFileRequest(t *testing.T, target, tenantID, actorID, fieldName, filename string, content []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(fieldName, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if tenantID != "" {
		r.Header.Set("X-Tenant-ID", tenantID)
	}
	if actorID != "" {
		r.Header.Set("X-Actor-ID", actorID)
	}
	return r
}

// countFilesUnder walks root and returns how many regular files exist —
// used to assert a blob was actually cleaned up (or never written),
// without needing to know its exact key.
func countFilesUnder(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}

// TestAttachmentUpload_Download_FullLoop exercises the whole slice end
// to end through real HTTP: publish Vendor+Attachment, create a Vendor
// record, upload a file attached to it, then download it back and
// confirm the bytes round-trip exactly.
func TestAttachmentUpload_Download_FullLoop(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	blobRoot := t.TempDir()
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	// Create the target Vendor record the attachment will be attached to.
	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create Vendor: expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	vendorID := created.Data.ID

	content := []byte("hello attachment world, this is a test file")
	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+vendorID, tenantID, "farshid", "file", "notes.txt", content)
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("upload: expected 201, got %d: %s", uploadRec.Code, uploadRec.Body.String())
	}

	var uploaded struct {
		Data struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	attachmentID := uploaded.Data.ID
	if attachmentID == "" {
		t.Fatalf("expected a non-empty attachment id, got response: %s", uploadRec.Body.String())
	}
	if got := uploaded.Data.Data["entity_type"]; got != "Vendor" {
		t.Fatalf("expected entity_type=Vendor, got %v", got)
	}
	if got := uploaded.Data.Data["record_id"]; got != vendorID {
		t.Fatalf("expected record_id=%s, got %v", vendorID, got)
	}
	if got := uploaded.Data.Data["file_name"]; got != "notes.txt" {
		t.Fatalf("expected file_name=notes.txt, got %v", got)
	}
	if got, _ := uploaded.Data.Data["size_bytes"].(float64); int(got) != len(content) {
		t.Fatalf("expected size_bytes=%d, got %v", len(content), uploaded.Data.Data["size_bytes"])
	}
	storagePath, _ := uploaded.Data.Data["storage_path"].(string)
	if storagePath == "" {
		t.Fatal("expected a non-empty storage_path")
	}
	if wantPrefix := tenantID + "/Vendor/" + vendorID + "/"; len(storagePath) <= len(wantPrefix) || storagePath[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("expected storage_path to start with %q, got %q", wantPrefix, storagePath)
	}

	// Download it back and confirm the bytes match exactly.
	downloadReq := newRequest("GET", "/api/attachments/"+attachmentID, tenantID, "farshid", nil)
	downloadRec := httptest.NewRecorder()
	mux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusOK {
		t.Fatalf("download: expected 200, got %d: %s", downloadRec.Code, downloadRec.Body.String())
	}
	if !bytes.Equal(downloadRec.Body.Bytes(), content) {
		t.Fatalf("downloaded content mismatch: got %q, want %q", downloadRec.Body.Bytes(), content)
	}
	if ct := downloadRec.Header().Get("Content-Type"); ct == "" {
		t.Fatal("expected a non-empty Content-Type header")
	}
	if cd := downloadRec.Header().Get("Content-Disposition"); cd == "" {
		t.Fatal("expected a Content-Disposition header naming the file")
	}
}

// TestAttachmentUpload_TargetRecordNotFound_404 confirms the handler
// checks the target record exists (and is readable) BEFORE accepting
// any file bytes — a nonexistent vendor id must 404, not silently
// create an orphaned attachment.
func TestAttachmentUpload_TargetRecordNotFound_404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	blobRoot := t.TempDir()
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	fakeID := "00000000-0000-0000-0000-000000000000"
	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+fakeID, tenantID, "farshid", "file", "notes.txt", []byte("x"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent target record, got %d: %s", rec.Code, rec.Body.String())
	}
	if countFilesUnder(t, blobRoot) != 0 {
		t.Fatal("expected no blob written when the target record doesn't exist")
	}
}

// TestAttachmentUpload_InvalidRecordID_400 confirms the recordID path
// segment is validated (same idPattern every other id-taking route
// enforces) before it's used for anything, including a lookup.
func TestAttachmentUpload_InvalidRecordID_400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/not-a-uuid", tenantID, "farshid", "file", "notes.txt", []byte("x"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid record id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAttachmentUpload_NoFileField_400 confirms a multipart body missing
// the "file" field is a 400, not a panic or a 500.
func TestAttachmentUpload_NoFileField_400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("not_file", "x")
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/api/attachments/Vendor/"+created.Data.ID, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-Tenant-ID", tenantID)
	r.Header.Set("X-Actor-ID", "farshid")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing file field, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAttachmentUpload_OversizedFile_Rejected confirms the handler
// enforces its own upload size cap rather than accepting an unbounded
// body.
func TestAttachmentUpload_OversizedFile_Rejected(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	blobRoot := t.TempDir()
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	oversized := bytes.Repeat([]byte("x"), maxAttachmentBytes+1024)
	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+created.Data.ID, tenantID, "farshid", "file", "big.bin", oversized)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an oversized upload, got %d: %s", rec.Code, rec.Body.String())
	}
	if countFilesUnder(t, blobRoot) != 0 {
		t.Fatal("expected no blob written for a rejected oversized upload")
	}
}

// TestAttachmentUpload_LargeFileSpillsToDiskNotHeap is a regression test
// for uc-infra#171: ParseMultipartForm's second argument bounds how much
// of the request multipart.Reader buffers in the process heap before
// spilling the rest to a temp file — it is NOT a size cap
// (http.MaxBytesReader, via maxAttachmentBytes, is the actual ceiling).
// Passing the full maxAttachmentBytes cap as that argument (the pre-fix
// behavior) meant an upload anywhere near the cap was retained entirely
// in memory instead of spilling.
//
// This calls the real attachmentUpload handler (not a reimplementation
// of its parsing logic) with an upload comfortably over
// multipartParseMemory but well under maxAttachmentBytes, then inspects
// the SAME *http.Request object's MultipartForm afterward — the handler
// is invoked directly (bypassing DevAuth's r.WithContext, which would
// otherwise mutate a copy the test can no longer see) so the request
// pointer the test holds is exactly the one ParseMultipartForm populated
// in production code. Asserts the resulting file part is backed by
// *os.File — i.e. actually spilled to a temp file — which fails against
// the pre-fix code (ParseMultipartForm(maxAttachmentBytes) keeps a file
// this size entirely in memory) and passes after.
func TestAttachmentUpload_LargeFileSpillsToDiskNotHeap(t *testing.T) {
	router := newTestRouter(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	withDevAuthEnabled(t)
	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	// 1 MiB over the in-memory threshold, ~11 MiB under the hard cap —
	// big enough to prove spilling happened, nowhere near maxAttachmentBytes.
	content := bytes.Repeat([]byte("x"), multipartParseMemory+(1<<20))
	req := newMultipartFileRequest(t, "/api/attachments/Vendor/"+created.Data.ID, "", "", "file", "big.bin", content)
	req.SetPathValue("entityType", "Vendor")
	req.SetPathValue("recordID", created.Data.ID)
	req = req.WithContext(httpx.WithRequestContext(req.Context(), httpx.RequestContext{TenantID: tenantID, Actor: humanActor()}))

	rec := httptest.NewRecorder()
	h.attachmentUpload(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("attachmentUpload: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	if req.MultipartForm == nil {
		t.Fatal("expected req.MultipartForm to be populated after attachmentUpload ran")
	}
	defer func() {
		if err := req.MultipartForm.RemoveAll(); err != nil {
			t.Fatalf("cleanup temp files: %v", err)
		}
	}()

	fhs := req.MultipartForm.File["file"]
	if len(fhs) != 1 {
		t.Fatalf("expected exactly 1 file part, got %d", len(fhs))
	}
	f, err := fhs[0].Open()
	if err != nil {
		t.Fatalf("Open uploaded file part: %v", err)
	}
	defer f.Close()

	if _, ok := f.(*os.File); !ok {
		// If this ever fails unexpectedly after adding a second file part
		// to this request, check that first: FileHeader.Open() only
		// returns a bare *os.File when exactly one part spilled — two or
		// more sharing a temp file (fh.tmpshared) return the same
		// in-memory-shaped wrapper type this assertion is trying to rule
		// out, even though the data legitimately is on disk. Harmless
		// here (this request has exactly one file part), but not a
		// generalizable "any *os.File means spilled" check on its own.
		t.Fatalf("file part of %d bytes (> multipartParseMemory=%d) was not spilled to a temp file at the real attachmentUpload call site — got %T, want *os.File; it is being buffered entirely in the process heap", len(content), multipartParseMemory, f)
	}
}

// TestAttachmentUpload_Download_LargeFile_RoundTrips confirms the
// smaller multipartParseMemory (uc-infra#171) doesn't change observable
// behavior for a caller: a file well over that threshold, still under
// maxAttachmentBytes, uploads and downloads back byte-for-byte through
// the real handler + blobstore, exactly like the small-file case in
// TestAttachmentUpload_Download_FullLoop.
//
// The upload step calls attachmentUpload directly rather than routing
// through the real mux, for the same reason
// TestAttachmentUpload_LargeFileSpillsToDiskNotHeap above does: DevAuth's
// r.WithContext makes a request COPY, and this test needs to reach the
// exact request object ParseMultipartForm populated afterward so it can
// remove the spilled multipart temp file — production never needs to
// do this by hand, Go's own net/http server calls
// req.MultipartForm.RemoveAll() automatically once a real request
// finishes (net/http/server.go's finishRequest), but httptest.NewRecorder
// never reaches that code path. An earlier version of this test went
// through the real mux and silently leaked a ~9 MiB temp file per run —
// caught by independent review, confirmed against files it found
// actually accumulating in /tmp from prior runs.
func TestAttachmentUpload_Download_LargeFile_RoundTrips(t *testing.T) {
	router := newTestRouter(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	blobRoot := t.TempDir()
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	withDevAuthEnabled(t)
	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme Textiles"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create Vendor: expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	content := bytes.Repeat([]byte("x"), multipartParseMemory+(1<<20))
	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+created.Data.ID, "", "", "file", "big.bin", content)
	uploadReq.SetPathValue("entityType", "Vendor")
	uploadReq.SetPathValue("recordID", created.Data.ID)
	uploadReq = uploadReq.WithContext(httpx.WithRequestContext(uploadReq.Context(), httpx.RequestContext{TenantID: tenantID, Actor: humanActor()}))

	uploadRec := httptest.NewRecorder()
	h.attachmentUpload(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("upload: expected 201, got %d: %s", uploadRec.Code, uploadRec.Body.String())
	}
	if uploadReq.MultipartForm != nil {
		t.Cleanup(func() {
			if err := uploadReq.MultipartForm.RemoveAll(); err != nil {
				t.Fatalf("cleanup temp files: %v", err)
			}
		})
	}
	var uploaded struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}

	downloadReq := newRequest("GET", "/api/attachments/"+uploaded.Data.ID, tenantID, "farshid", nil)
	downloadRec := httptest.NewRecorder()
	mux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusOK {
		t.Fatalf("download: expected 200, got %d: %s", downloadRec.Code, downloadRec.Body.String())
	}
	if !bytes.Equal(downloadRec.Body.Bytes(), content) {
		t.Fatal("downloaded content did not match the uploaded content byte-for-byte")
	}
}

// TestAttachment_BlobstoreNotConfigured_503 confirms both endpoints
// degrade to 503 (not a panic) when SetBlobstore was never called — the
// nil-safe contract this package's other optional integrations
// (ai/speech/members) already follow.
func TestAttachment_BlobstoreNotConfigured_503(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router) // no SetBlobstore call
	mux := http.NewServeMux()
	h.Routes(mux)

	fakeID := "00000000-0000-0000-0000-000000000000"
	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+fakeID, tenantID, "farshid", "file", "x.txt", []byte("x"))
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("upload: expected 503 with no blobstore configured, got %d: %s", uploadRec.Code, uploadRec.Body.String())
	}

	downloadReq := newRequest("GET", "/api/attachments/"+fakeID, tenantID, "farshid", nil)
	downloadRec := httptest.NewRecorder()
	mux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("download: expected 503 with no blobstore configured, got %d: %s", downloadRec.Code, downloadRec.Body.String())
	}
}

// TestAttachmentUpload_CreateDenied_OrphanBlobCleanedUp is the
// integration-level proof of ADR-0024's orphan-cleanup step: if the
// Attachment record write is refused (here, by RBAC — the same 403 an
// authz denial anywhere else in this API produces) AFTER the blob was
// already durably stored, the blob must not be left behind.
func TestAttachmentUpload_CreateDenied_OrphanBlobCleanedUp(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	// Vendor is left ungoverned (no permission rows — open to everyone,
	// same "no rules for this entity type = default open" semantics
	// TestAPI_RBAC_EntityLevel_Enforced403 documents), so the target-
	// record read still succeeds; only Attachment writes are denied.
	seedRBAC(t, db,
		map[string][]string{"clerk": {"user-clerk"}},
		[]map[string]any{
			{"role": "clerk", "entity_type": "Attachment", "can_read": true, "can_write": false},
		},
	)

	h := testHandler(t, router)
	blobRoot := t.TempDir()
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "user-clerk", []byte(`{"name":"Acme"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create Vendor (ungoverned): expected 201, got %d: %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+created.Data.ID, tenantID, "user-clerk", "file", "notes.txt", []byte("some bytes"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when Attachment writes are denied, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := countFilesUnder(t, blobRoot); n != 0 {
		t.Fatalf("expected the orphaned blob to be cleaned up after the denied Create, found %d file(s) under %s", n, blobRoot)
	}
}

// TestAttachmentDownload_NotFound_404 and
// TestAttachmentDownload_InvalidID_400 cover the same two guard clauses
// getRecord's own tests already establish for /api/records/{type}/{id}.
func TestAttachmentDownload_NotFound_404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	fakeID := "00000000-0000-0000-0000-000000000000"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest("GET", "/api/attachments/"+fakeID, tenantID, "farshid", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAttachmentDownload_InvalidID_400(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest("GET", "/api/attachments/not-a-uuid", tenantID, "farshid", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAttachmentDownload_ForgedStoragePath_CrossTenant_Denied is the
// adversarial integration test ADR-0024 §2 and uc-infra#142's own
// acceptance criteria call for: a real file is uploaded (legitimately)
// under tenant A's namespace, then tenant B — in ITS OWN database, via
// the ordinary generic POST /api/records/Attachment route, not this
// package's upload handler — creates its own, entirely valid Attachment
// row whose storage_path is a forged copy of tenant A's real key.
// Downloading that record as tenant B must be refused, and must not
// leak tenant A's file content, even though the record itself is
// legitimately tenant B's own and the read-RBAC check on it passes.
func TestAttachmentDownload_ForgedStoragePath_CrossTenant_Denied(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)

	tenantA, dbA := newTestTenant(t, router)
	publishEntityAndForm(t, dbA, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, dbA, foundation.Attachment())

	tenantB, dbB := newTestTenant(t, router)
	publishEntityAndForm(t, dbB, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, dbB, foundation.Attachment())

	h := testHandler(t, router)
	blobRoot := t.TempDir() // one shared store, same as production
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	// Tenant A legitimately uploads a real, secret file.
	secretContent := []byte("tenant A's confidential file contents")
	createAReq := newRequest("POST", "/api/records/Vendor", tenantA, "farshid", []byte(`{"name":"Acme A"}`))
	createAReq.Header.Set("Content-Type", "application/json")
	createARec := httptest.NewRecorder()
	mux.ServeHTTP(createARec, createAReq)
	var createdA struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createARec.Body.Bytes(), &createdA)

	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+createdA.Data.ID, tenantA, "farshid", "file", "secret.txt", secretContent)
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("tenant A upload: expected 201, got %d: %s", uploadRec.Code, uploadRec.Body.String())
	}
	var uploaded struct {
		Data struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	_ = json.Unmarshal(uploadRec.Body.Bytes(), &uploaded)
	realStoragePath, _ := uploaded.Data.Data["storage_path"].(string)
	if realStoragePath == "" {
		t.Fatal("expected tenant A's upload to yield a real storage_path")
	}
	realAttachmentID := uploaded.Data.ID

	// Tenant B creates its own Vendor record, then a forged Attachment
	// row (via the ordinary generic record API — the exact gap
	// ADR-0024 describes: storage_path is a plain writable field)
	// naming tenant A's REAL storage_path.
	createBReq := newRequest("POST", "/api/records/Vendor", tenantB, "farshid", []byte(`{"name":"Acme B"}`))
	createBReq.Header.Set("Content-Type", "application/json")
	createBRec := httptest.NewRecorder()
	mux.ServeHTTP(createBRec, createBReq)
	var createdB struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createBRec.Body.Bytes(), &createdB)

	forgedPayload, _ := json.Marshal(map[string]any{
		"entity_type":  "Vendor",
		"record_id":    createdB.Data.ID,
		"file_name":    "innocuous.txt",
		"mime_type":    "text/plain",
		"size_bytes":   1,
		"storage_path": realStoragePath, // forged: tenant A's real key
	})
	forgeReq := newRequest("POST", "/api/records/Attachment", tenantB, "farshid", forgedPayload)
	forgeReq.Header.Set("Content-Type", "application/json")
	forgeRec := httptest.NewRecorder()
	mux.ServeHTTP(forgeRec, forgeReq)
	if forgeRec.Code != http.StatusCreated {
		t.Fatalf("forging the Attachment record itself should succeed (that's the gap this test proves is closed downstream): expected 201, got %d: %s", forgeRec.Code, forgeRec.Body.String())
	}
	var forged struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(forgeRec.Body.Bytes(), &forged)

	// Tenant B downloads its own (forged) record — must be refused, not
	// served tenant A's file.
	downloadReq := newRequest("GET", "/api/attachments/"+forged.Data.ID, tenantB, "farshid", nil)
	downloadRec := httptest.NewRecorder()
	mux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a forged cross-tenant storage_path, got %d: %s", downloadRec.Code, downloadRec.Body.String())
	}
	if bytes.Contains(downloadRec.Body.Bytes(), secretContent) {
		t.Fatal("tenant A's secret file content leaked into tenant B's response")
	}

	// Confirm tenant A can still download its own file normally — the
	// fix doesn't collaterally break the legitimate same-tenant case.
	legitDownloadReq := newRequest("GET", "/api/attachments/"+realAttachmentID, tenantA, "farshid", nil)
	legitDownloadRec := httptest.NewRecorder()
	mux.ServeHTTP(legitDownloadRec, legitDownloadReq)
	if legitDownloadRec.Code != http.StatusOK {
		t.Fatalf("tenant A's own legitimate download: expected 200, got %d: %s", legitDownloadRec.Code, legitDownloadRec.Body.String())
	}
	if !bytes.Equal(legitDownloadRec.Body.Bytes(), secretContent) {
		t.Fatal("tenant A's own legitimate download did not return its own file content")
	}
}

// TestAttachmentDownload_ForgedStoragePath_DotDotPrefixBypass_Denied is
// the regression test for the critical finding from this card's
// independent review: the original tenant-namespace check compared the
// RAW storage_path string against "{tenantID}/" via strings.HasPrefix,
// but blobstore.FSStore resolves keys through filepath.Join+Clean, which
// COLLAPSES "..". A forged storage_path of the shape
// "{ownTenant}/../{victimTenant}/..." starts with the caller's own
// tenant id (passing a naive prefix check) while resolving, once
// cleaned, to the VICTIM tenant's real directory — the exact bypass
// TestAttachmentDownload_ForgedStoragePath_CrossTenant_Denied above does
// NOT cover, because that test forges tenant A's key VERBATIM (which
// correctly fails a naive prefix check without needing any traversal
// element at all). This test failed against the pre-fix code (200 with
// tenant A's real file content); the fix must reject any storage_path
// that isn't already in canonical (Clean'd) form before ever comparing
// it to the requesting tenant's prefix.
func TestAttachmentDownload_ForgedStoragePath_DotDotPrefixBypass_Denied(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)

	tenantA, dbA := newTestTenant(t, router)
	publishEntityAndForm(t, dbA, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, dbA, foundation.Attachment())

	tenantB, dbB := newTestTenant(t, router)
	publishEntityAndForm(t, dbB, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, dbB, foundation.Attachment())

	h := testHandler(t, router)
	blobRoot := t.TempDir() // one shared store, same as production
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	// Tenant A legitimately uploads a real, secret file.
	secretContent := []byte("tenant A's other confidential file")
	createAReq := newRequest("POST", "/api/records/Vendor", tenantA, "farshid", []byte(`{"name":"Acme A"}`))
	createAReq.Header.Set("Content-Type", "application/json")
	createARec := httptest.NewRecorder()
	mux.ServeHTTP(createARec, createAReq)
	var createdA struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createARec.Body.Bytes(), &createdA)

	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+createdA.Data.ID, tenantA, "farshid", "file", "secret2.txt", secretContent)
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("tenant A upload: expected 201, got %d: %s", uploadRec.Code, uploadRec.Body.String())
	}
	var uploaded struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	_ = json.Unmarshal(uploadRec.Body.Bytes(), &uploaded)
	realStoragePath, _ := uploaded.Data.Data["storage_path"].(string)
	if realStoragePath == "" {
		t.Fatal("expected tenant A's upload to yield a real storage_path")
	}

	// Tenant B creates its own Vendor record, then a forged Attachment
	// row whose storage_path STARTS WITH TENANT B'S OWN ID (so a naive
	// strings.HasPrefix(storagePath, tenantB+"/") check passes) but
	// contains a ".." segment that, once filepath.Clean'd by FSStore,
	// resolves into tenant A's real directory.
	createBReq := newRequest("POST", "/api/records/Vendor", tenantB, "farshid", []byte(`{"name":"Acme B"}`))
	createBReq.Header.Set("Content-Type", "application/json")
	createBRec := httptest.NewRecorder()
	mux.ServeHTTP(createBRec, createBReq)
	var createdB struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createBRec.Body.Bytes(), &createdB)

	forgedStoragePath := tenantB + "/../" + realStoragePath
	forgedPayload, _ := json.Marshal(map[string]any{
		"entity_type":  "Vendor",
		"record_id":    createdB.Data.ID,
		"file_name":    "innocuous.txt",
		"mime_type":    "text/plain",
		"size_bytes":   1,
		"storage_path": forgedStoragePath,
	})
	forgeReq := newRequest("POST", "/api/records/Attachment", tenantB, "farshid", forgedPayload)
	forgeReq.Header.Set("Content-Type", "application/json")
	forgeRec := httptest.NewRecorder()
	mux.ServeHTTP(forgeRec, forgeReq)
	if forgeRec.Code != http.StatusCreated {
		t.Fatalf("forging the Attachment record itself should succeed: expected 201, got %d: %s", forgeRec.Code, forgeRec.Body.String())
	}
	var forged struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(forgeRec.Body.Bytes(), &forged)

	downloadReq := newRequest("GET", "/api/attachments/"+forged.Data.ID, tenantB, "farshid", nil)
	downloadRec := httptest.NewRecorder()
	mux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a \"..\"-forged storage_path that passes a naive prefix check, got %d: %s", downloadRec.Code, downloadRec.Body.String())
	}
	if bytes.Contains(downloadRec.Body.Bytes(), secretContent) {
		t.Fatal("tenant A's secret file content leaked into tenant B's response via a \"..\" prefix-check bypass")
	}
}

// TestSanitizeAttachmentFileName is a pure unit test for the one
// genuinely security-relevant pure function in this file (independent
// review, uc-infra#142) — every existing integration test only ever
// uploads a plain, harmless filename, so none of these branches were
// exercised anywhere before this test.
func TestSanitizeAttachmentFileName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain filename", "notes.txt", "notes.txt"},
		{"unix path traversal keeps only the base name", "../../etc/cron.d/evil", "evil"},
		{"windows-style backslash path", `..\..\Windows\System32\evil.dll`, "evil.dll"},
		{"absolute unix path", "/etc/passwd", "passwd"},
		{"empty string falls back", "", "file"},
		{"bare dot falls back", ".", "file"},
		{"bare dot-dot falls back", "..", "file"},
		{"whitespace-only trims to empty and falls back", "   ", "file"},
		{"leading/trailing whitespace trimmed", "  notes.txt  ", "notes.txt"},
		{"trailing slash leaves an empty base name, falls back", "a/b/", "file"},
		{"very long filename is truncated, not rejected", strings.Repeat("x", 500) + ".txt", strings.Repeat("x", 200)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeAttachmentFileName(c.in)
			if got != c.want {
				t.Fatalf("sanitizeAttachmentFileName(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.ContainsAny(got, `/\`) {
				t.Fatalf("sanitizeAttachmentFileName(%q) = %q still contains a path separator", c.in, got)
			}
			if len(got) > maxAttachmentFileNameLength {
				t.Fatalf("sanitizeAttachmentFileName(%q) = %q exceeds the %d-byte cap", c.in, got, maxAttachmentFileNameLength)
			}
		})
	}
}

// TestStoragePathBelongsToTenant is the pure unit-level regression test
// for the critical bypass fixed above: a raw prefix check alone accepts
// a "{tenant}/../{other-tenant}/..." key. Every case a caller could
// realistically construct is exercised here directly, independent of
// the full HTTP round trip the two "ForgedStoragePath" integration tests
// above also cover.
func TestStoragePathBelongsToTenant(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	const other = "22222222-2222-2222-2222-222222222222"

	cases := []struct {
		name        string
		storagePath string
		tenantID    string
		want        bool
	}{
		{"legitimate own-tenant path", tenant + "/Vendor/rec/hex-file.txt", tenant, true},
		{"empty storage_path", "", tenant, false},
		{"empty tenant id", tenant + "/Vendor/rec/hex-file.txt", "", false},
		{"different tenant's real path, no traversal", other + "/Vendor/rec/hex-file.txt", tenant, false},
		{"dot-dot prefix bypass: own-tenant prefix, traverses into another", tenant + "/../" + other + "/Vendor/rec/hex-file.txt", tenant, false},
		{"dot-dot bypass targeting the requesting tenant itself is still non-canonical, refused", tenant + "/../" + tenant + "/Vendor/rec/hex-file.txt", tenant, false},
		{"single dot segment", tenant + "/./Vendor/rec/hex-file.txt", tenant, false},
		{"double slash", tenant + "//Vendor/rec/hex-file.txt", tenant, false},
		{"trailing slash only", tenant + "/", tenant, false},
		{"prefix string-collision, not a real path boundary", tenant + "-evil/Vendor/rec/hex-file.txt", tenant, false},
		{"bare tenant id with no trailing segment", tenant, tenant, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := storagePathBelongsToTenant(c.storagePath, c.tenantID); got != c.want {
				t.Fatalf("storagePathBelongsToTenant(%q, %q) = %v, want %v", c.storagePath, c.tenantID, got, c.want)
			}
		})
	}
}

// TestAttachmentUpload_AdversarialFileName_DoesNotEscapeTenantNamespace
// drives the real HTTP upload path (not just the pure sanitizer unit
// test above) with a path-traversal filename, and confirms the
// resulting storage_path stays correctly namespaced — closing the loop
// between attachmentStorageKey and sanitizeAttachmentFileName end to end.
func TestAttachmentUpload_AdversarialFileName_DoesNotEscapeTenantNamespace(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	blobRoot := t.TempDir()
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	uploadReq := newMultipartFileRequest(t, "/api/attachments/Vendor/"+created.Data.ID, tenantID, "farshid", "file", "../../etc/cron.d/evil", []byte("payload"))
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for an adversarial-but-otherwise-valid upload, got %d: %s", uploadRec.Code, uploadRec.Body.String())
	}
	var uploaded struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	_ = json.Unmarshal(uploadRec.Body.Bytes(), &uploaded)
	storagePath, _ := uploaded.Data.Data["storage_path"].(string)
	wantPrefix := tenantID + "/Vendor/" + created.Data.ID + "/"
	if !strings.HasPrefix(storagePath, wantPrefix) {
		t.Fatalf("adversarial filename escaped the expected key namespace: storage_path=%q, want prefix %q", storagePath, wantPrefix)
	}
	if strings.Contains(storagePath, "..") {
		t.Fatalf("adversarial filename left a \"..\" segment in the computed storage_path: %q", storagePath)
	}
}

// TestAttachmentDownload_MissingBlob_404 and
// TestAttachmentDownload_StoragePathIsDirectory_404 cover the two
// "storage_path passed the tenant check but doesn't resolve to a real
// file" cases the independent review flagged: previously both fell
// through to a bare 500, and the directory case returned 200 with an
// empty body (status already committed by the time io.Copy failed).
func TestAttachmentDownload_MissingBlob_404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, t.TempDir()))
	mux := http.NewServeMux()
	h.Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var createdVendor struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &createdVendor)

	// A well-formed, correctly-namespaced Attachment record whose blob
	// was never actually written (e.g. deleted out-of-band, or an
	// ephemeral-filesystem restart) — created directly via the generic
	// record API rather than the upload handler, since the upload
	// handler itself always writes the blob first.
	payload, _ := json.Marshal(map[string]any{
		"entity_type":  "Vendor",
		"record_id":    createdVendor.Data.ID,
		"file_name":    "gone.txt",
		"mime_type":    "text/plain",
		"size_bytes":   1,
		"storage_path": tenantID + "/Vendor/" + createdVendor.Data.ID + "/never-written.txt",
	})
	createAttReq := newRequest("POST", "/api/records/Attachment", tenantID, "farshid", payload)
	createAttReq.Header.Set("Content-Type", "application/json")
	createAttRec := httptest.NewRecorder()
	mux.ServeHTTP(createAttRec, createAttReq)
	if createAttRec.Code != http.StatusCreated {
		t.Fatalf("create Attachment record: expected 201, got %d: %s", createAttRec.Code, createAttRec.Body.String())
	}
	var createdAtt struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createAttRec.Body.Bytes(), &createdAtt)

	downloadReq := newRequest("GET", "/api/attachments/"+createdAtt.Data.ID, tenantID, "farshid", nil)
	downloadRec := httptest.NewRecorder()
	mux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a record whose blob was never written, got %d: %s", downloadRec.Code, downloadRec.Body.String())
	}
}

func TestAttachmentDownload_StoragePathIsDirectory_404(t *testing.T) {
	router := newTestRouter(t)
	withDevAuthEnabled(t)
	tenantID, db := newTestTenant(t, router)
	publishEntityAndForm(t, db, vendorEntityDef(), vendorFormDef())
	publishEntityOnly(t, db, foundation.Attachment())

	blobRoot := t.TempDir()
	h := testHandler(t, router)
	h.SetBlobstore(newFSStoreAt(t, blobRoot))
	mux := http.NewServeMux()
	h.Routes(mux)

	createReq := newRequest("POST", "/api/records/Vendor", tenantID, "farshid", []byte(`{"name":"Acme"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var createdVendor struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &createdVendor)

	// storage_path resolving to a real directory that already exists
	// under blobRoot (the tenant's own namespace directory, created as a
	// side effect of any prior Put under it) rather than a file.
	dirStoragePath := tenantID + "/Vendor/" + createdVendor.Data.ID
	if err := os.MkdirAll(filepath.Join(blobRoot, dirStoragePath), 0o750); err != nil {
		t.Fatalf("seed directory at storage_path: %v", err)
	}

	payload, _ := json.Marshal(map[string]any{
		"entity_type":  "Vendor",
		"record_id":    createdVendor.Data.ID,
		"file_name":    "not-a-file.txt",
		"mime_type":    "text/plain",
		"size_bytes":   1,
		"storage_path": dirStoragePath,
	})
	createAttReq := newRequest("POST", "/api/records/Attachment", tenantID, "farshid", payload)
	createAttReq.Header.Set("Content-Type", "application/json")
	createAttRec := httptest.NewRecorder()
	mux.ServeHTTP(createAttRec, createAttReq)
	if createAttRec.Code != http.StatusCreated {
		t.Fatalf("create Attachment record: expected 201, got %d: %s", createAttRec.Code, createAttRec.Body.String())
	}
	var createdAtt struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(createAttRec.Body.Bytes(), &createdAtt)

	downloadReq := newRequest("GET", "/api/attachments/"+createdAtt.Data.ID, tenantID, "farshid", nil)
	downloadRec := httptest.NewRecorder()
	mux.ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a storage_path resolving to a directory, got %d: %s", downloadRec.Code, downloadRec.Body.String())
	}
	if downloadRec.Body.Len() != 0 {
		// Not strictly required, but if this ever becomes non-empty
		// while still reporting 404, that's itself worth knowing.
		t.Logf("note: 404 response body was non-empty: %q", downloadRec.Body.String())
	}
}
