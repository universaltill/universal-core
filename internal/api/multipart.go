package api

// multipartParseMemory is the maxMemory argument every handler in this
// package that accepts a multipart/form-data upload passes to
// r.ParseMultipartForm — attachmentUpload, readUploadedFile,
// issueReportSubmit, and issueReportTranscribe. It is a SEPARATE bound
// from each handler's own hard upload ceiling (maxAttachmentBytes,
// maxUploadBytes, maxIssueReportSubmitBytes, maxVoiceNoteBytes — each
// enforced via its own http.MaxBytesReader on r.Body): ParseMultipartForm's
// second argument is not a size cap at all, it's how much of the upload
// multipart.Reader buffers in the process heap before spilling the rest
// to a temp file (multipart.Reader.ReadForm only spills a file part
// once it exceeds maxMemory).
//
// Passing a handler's own full byte cap here instead of this constant —
// the bug this constant exists to prevent recurring — keeps an entire
// near-cap upload in memory rather than letting Go spill it to disk: a
// real memory-exhaustion risk under concurrent large uploads. Independent
// review found and fixed this at issueReportSubmit first (uc-infra#92),
// then at attachmentUpload/readUploadedFile and issueReportTranscribe
// (uc-infra#171) — the same shape recurring at three more call sites in
// this same package is exactly why this is now one named, shared
// constant instead of four near-identical local ones: a
// r.ParseMultipartForm call in this package that doesn't reference
// multipartParseMemory should be treated as a bug on sight.
//
// Deliberately much smaller than every one of the hard caps above (all
// >= 10 MiB) so a file bigger than this but still under its handler's
// cap reliably spills — see each call site's own comment for what
// spilling does and doesn't buy that particular handler.
//
// One exception: parseRecordFields' own hard cap, maxRecordFieldsBytes
// (2 MiB), is deliberately smaller than THIS constant — it accepts only
// plain entity field values, never a file, so its ceiling is far below
// every upload-carrying endpoint's. Nothing can ever exceed 8 MiB once
// already capped to 2 MiB, so ParseMultipartForm never spills at that
// one call site; still passed for the same "a ParseMultipartForm call
// that doesn't reference multipartParseMemory is a bug on sight"
// consistency reason as everywhere else, not because spilling is reachable
// there.
const multipartParseMemory = 8 << 20 // 8 MiB
