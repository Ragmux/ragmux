package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pgvector/pgvector-go"
)

// Document status values.
const (
	DocPending    = "pending"
	DocProcessing = "processing"
	DocReady      = "ready"
	DocFailed     = "failed"
)

// Document is an uploaded file attached to a RAG store. The raw bytes live in
// the database and are fetched separately with DocumentContent.
type Document struct {
	ID         int64  `json:"id"`
	RAGStoreID int64  `json:"rag_store_id"`
	Filename   string `json:"filename"`
	Mime       string `json:"mime"`
	SizeBytes  int64  `json:"size_bytes"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	ChunkCount int    `json:"chunk_count"`
	// PageCount is known for PDFs once parsed; nil for other formats.
	PageCount *int `json:"page_count"`
	// ProgressPercent moves 0 -> 100 while the document is ingested and
	// keeps its last value when ingestion fails.
	ProgressPercent int `json:"progress_percent"`
	// Attempts counts how often a replica has claimed this document since
	// it was last queued. A document past the attempt cap is skipped by the
	// claim query, so one poisonous file cannot walk the cluster.
	Attempts int `json:"attempts"`
	// Metadata is the document's filterable attributes (ADR-025): a flat JSON
	// object, '{}' when it has none. See ValidateMetadata for the shape.
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	// ContentSHA256 is the hex SHA-256 of the raw bytes; empty for a row
	// written before the hash existed that was never processed again. An
	// internal detail of dedup, not part of the API.
	ContentSHA256 string `json:"-"`
}

// ContentHash is the digest CreateDocument stores for a document's bytes.
func ContentHash(content []byte) []byte {
	sum := sha256.Sum256(content)
	return sum[:]
}

const docCols = "id, rag_store_id, filename, mime, size_bytes, status, error, chunk_count, page_count, progress_percent, attempts, metadata, created_at, updated_at, content_sha256"

// docColsD is docCols qualified for statements that join another relation
// and would otherwise read ambiguously.
var docColsD = "d." + strings.ReplaceAll(docCols, ", ", ", d.")

func scanDoc(row interface{ Scan(...any) error }) (*Document, error) {
	d := &Document{}
	var created, updated time.Time
	var progress int16
	var hash []byte
	err := row.Scan(&d.ID, &d.RAGStoreID, &d.Filename, &d.Mime, &d.SizeBytes, &d.Status, &d.Error,
		&d.ChunkCount, &d.PageCount, &progress, &d.Attempts, &d.Metadata, &created, &updated, &hash)
	if err != nil {
		return nil, scanErr(err)
	}
	d.ContentSHA256 = hex.EncodeToString(hash)
	d.CreatedAt, d.UpdatedAt = ts(created), ts(updated)
	d.ProgressPercent = int(progress)
	return d, nil
}

// CreateDocument records a pending document together with its raw bytes.
// d.Metadata, when set, must come from ValidateMetadata; empty means '{}'.
func (s *Store) CreateDocument(ctx context.Context, d *Document, content []byte) (*Document, error) {
	if content == nil {
		content = []byte{}
	}
	meta := []byte(d.Metadata)
	if len(meta) == 0 {
		meta = []byte("{}")
	}
	return scanDoc(s.pool.QueryRow(ctx, `INSERT INTO documents (rag_store_id, filename, mime, size_bytes, content, status, metadata, content_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8) RETURNING `+docCols,
		d.RAGStoreID, d.Filename, d.Mime, d.SizeBytes, content, DocPending, meta, ContentHash(content)))
}

// SetDocumentContentHash records the hash of a row that has none (written
// before 0025). A row that already has one is left alone.
func (s *Store) SetDocumentContentHash(ctx context.Context, id int64, hash []byte) error {
	_, err := s.pool.Exec(ctx, "UPDATE documents SET content_sha256 = $1 WHERE id = $2 AND content_sha256 IS NULL", hash, id)
	return err
}

// FindDuplicateDocuments returns up to limit other documents of doc's own
// store that are ready and have the same content hash, newest first. Never
// looks across stores. A document without a hash has none.
//
// Newest first because the newest ready document is the likeliest to have
// been chunked with the store's current settings; an old one may predate a
// chunking change and then match nothing, which must not hide a newer one
// that does.
//
// sameFilename additionally requires the same file name. A store that embeds
// the file name into the chunk text (contextual chunks) needs it: a copy
// under another name has other text to embed, so it is no duplicate.
func (s *Store) FindDuplicateDocuments(ctx context.Context, doc *Document, sameFilename bool, limit int) ([]*Document, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+docColsD+` FROM documents d
		WHERE d.rag_store_id = $1 AND d.id <> $2 AND d.status = $3 AND d.chunk_count > 0
		  AND d.content_sha256 = (SELECT content_sha256 FROM documents WHERE id = $2)
		  AND (NOT $5 OR d.filename = $4)
		ORDER BY d.id DESC LIMIT $6`, doc.RAGStoreID, doc.ID, DocReady, doc.Filename, sameFilename, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDocs(rows)
}

// ChunkVector is one stored chunk of a document with its embedding.
type ChunkVector struct {
	Index int
	// Content and EmbedText are what the chunk holds and what was embedded
	// ("" when the store embeds the content as is).
	Content, EmbedText string
	Embedding          []float32
}

// DocumentChunkVectors returns a document's chunks with their embeddings in
// chunk order. dims is the store's embedding width (RAGStore.Dimensions).
func (s *Store) DocumentChunkVectors(ctx context.Context, docID int64, dims int) ([]ChunkVector, error) {
	if dims < 1 || dims > maxVectorDims {
		return nil, fmt.Errorf("embedding dimension %d out of range (1-%d)", dims, maxVectorDims)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT c.idx, c.content, COALESCE(c.embed_text, ''), e.embedding
		FROM chunks c JOIN %s e ON e.chunk_id = c.id
		WHERE c.document_id = $1 ORDER BY c.idx`, vecTable(dims)), docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkVector
	for rows.Next() {
		var cv ChunkVector
		var vec pgvector.Vector
		if err := rows.Scan(&cv.Index, &cv.Content, &cv.EmbedText, &vec); err != nil {
			return nil, err
		}
		cv.Embedding = vec.Slice()
		out = append(out, cv)
	}
	return out, rows.Err()
}

// DocumentContent returns the raw uploaded bytes of a document.
func (s *Store) DocumentContent(ctx context.Context, id int64) ([]byte, error) {
	var content []byte
	if err := s.pool.QueryRow(ctx, "SELECT content FROM documents WHERE id = $1", id).Scan(&content); err != nil {
		return nil, scanErr(err)
	}
	return content, nil
}

// GetDocument fetches one document.
func (s *Store) GetDocument(ctx context.Context, id int64) (*Document, error) {
	return scanDoc(s.pool.QueryRow(ctx, "SELECT "+docCols+" FROM documents WHERE id = $1", id))
}

// ListDocuments lists the documents of one store.
func (s *Store) ListDocuments(ctx context.Context, storeID int64) ([]*Document, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+docCols+" FROM documents WHERE rag_store_id = $1 ORDER BY id DESC", storeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDocs(rows)
}

// ListDocumentsByStatus returns documents in any of the given states.
func (s *Store) ListDocumentsByStatus(ctx context.Context, statuses ...string) ([]*Document, error) {
	if len(statuses) == 0 {
		return []*Document{}, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT "+docCols+" FROM documents WHERE status = ANY($1) ORDER BY id", statuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDocs(rows)
}

func collectDocs(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*Document, error) {
	out := []*Document{}
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetDocumentStatus updates processing state. Moving a document back to
// pending is an explicit retry (an upload, a reprocess), so it also drops
// any stale claim and resets the attempt counter: the cap is meant to stop
// a crash loop, not to run out over a document's lifetime.
func (s *Store) SetDocumentStatus(ctx context.Context, id int64, status, errMsg string) error {
	_, err := s.pool.Exec(ctx, `UPDATE documents SET status=$1, error=$2, updated_at=now(),
		attempts    = CASE WHEN $1 = 'pending' THEN 0 ELSE attempts END,
		claimed_by  = CASE WHEN $1 = 'pending' THEN NULL ELSE claimed_by END,
		lease_until = CASE WHEN $1 = 'pending' THEN NULL ELSE lease_until END
		WHERE id=$3`, status, errMsg, id)
	return err
}

// claimWhere is the eligibility test of the ingestion queue: a document
// waiting to be picked up, or one whose owner stopped renewing its lease
// (it crashed, was killed, or lost the database). A lease_until of NULL is
// a row written before this schema existed.
const claimWhere = `(status='pending' OR (status='processing' AND (lease_until IS NULL OR lease_until < now())))
	AND attempts < $3`

// claimSet is the write a claim performs: take ownership, start the lease,
// count the attempt and clear the leftovers of any previous run.
const claimSet = `status='processing', claimed_by=$1, claimed_at=now(),
	lease_until=now()+make_interval(secs => $2), attempts=d.attempts+1,
	error='', progress_percent=0, page_count=NULL, updated_at=now()`

// ClaimDocument takes the next claimable document for owner and leases it
// for lease. FOR UPDATE SKIP LOCKED is what makes this safe to run from
// every replica at once: each poller locks a different row instead of
// queueing behind the same one. It returns ErrNotFound when the queue is
// empty, which is the dispatcher's signal to stop polling.
//
// A document that has been claimed maxAttempts times is skipped: without
// that cap a file that reliably kills the process becomes a cluster-wide
// crash loop as replicas take turns on it. The janitor fails those rows.
func (s *Store) ClaimDocument(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (*Document, error) {
	return scanDoc(s.pool.QueryRow(ctx, `WITH next AS (
			SELECT id FROM documents
			 WHERE `+claimWhere+`
			 ORDER BY id
			 FOR UPDATE SKIP LOCKED
			 LIMIT 1)
		UPDATE documents d SET `+claimSet+`
		  FROM next WHERE d.id = next.id
		RETURNING `+docColsD, owner, lease.Seconds(), maxAttempts))
}

// ExtendDocumentLease pushes the lease of a document owner still holds out
// by lease. ErrNotFound means the claim is gone: the lease expired and
// another replica took the document over, so the caller must stop working
// on it and must not write a status.
func (s *Store) ExtendDocumentLease(ctx context.Context, id int64, owner string, lease time.Duration) error {
	res, err := s.pool.Exec(ctx, `UPDATE documents SET lease_until=now()+make_interval(secs => $3)
		WHERE id=$1 AND claimed_by=$2 AND status=$4`, id, owner, lease.Seconds(), DocProcessing)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearDocumentClaim releases the lease a finished ingestion held and
// resets the attempt counter so a later reprocess starts from zero. The
// status is left alone: ReplaceDocumentChunks already wrote it.
func (s *Store) ClearDocumentClaim(ctx context.Context, id int64, owner string) error {
	_, err := s.pool.Exec(ctx, `UPDATE documents SET claimed_by=NULL, lease_until=NULL, attempts=0
		WHERE id=$1 AND claimed_by=$2`, id, owner)
	return err
}

// ReleaseDocuments puts every document owner is still processing back into
// the queue and reports how many. A replica calls it on a clean shutdown so
// the work is picked up immediately instead of after the lease expires.
//
// The attempt is given back: it was cancelled by an orderly restart, not
// spent on a document that took the process down, and a rolling deploy
// during a long ingest would otherwise eat the whole attempt budget.
func (s *Store) ReleaseDocuments(ctx context.Context, owner string) (int64, error) {
	res, err := s.pool.Exec(ctx, `UPDATE documents
		SET status=$2, claimed_by=NULL, lease_until=NULL, attempts=GREATEST(attempts-1, 0), updated_at=now()
		WHERE claimed_by=$1 AND status=$3`, owner, DocPending, DocProcessing)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// CountPendingDocuments is the cluster-wide ingestion backlog: what every
// replica together still has to pick up. Enqueue checks it against
// MAX_PENDING_DOCUMENTS.
func (s *Store) CountPendingDocuments(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM documents WHERE status=$1", DocPending).Scan(&n)
	return n, err
}

// FailExhaustedDocuments marks documents that ran out of claim attempts as
// failed, keeping the error of the last try. They are invisible to the
// claim query at that point, so without this they would sit in
// "processing" forever.
func (s *Store) FailExhaustedDocuments(ctx context.Context, maxAttempts int) (int64, error) {
	res, err := s.pool.Exec(ctx, `UPDATE documents
		SET status=$1, claimed_by=NULL, lease_until=NULL, updated_at=now(),
		    error = CASE WHEN error = '' THEN $3 ELSE error || ' (' || $3 || ')' END
		WHERE status=$2 AND attempts >= $4 AND (lease_until IS NULL OR lease_until < now())`,
		DocFailed, DocProcessing, "ingestion gave up after the maximum number of attempts (INGEST_MAX_ATTEMPTS)", maxAttempts)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// SetDocumentProgress records ingestion progress (clamped to 0..100). With
// pageCount set the page count is stored as well.
// owner, when set, restricts the write to the replica still holding the
// claim: a worker whose lease expired must not keep writing to a row another
// replica has taken over. ErrNotFound reports exactly that. An empty owner
// skips the check, for callers that never claimed the row.
func (s *Store) SetDocumentProgress(ctx context.Context, id int64, percent int, pageCount *int, owner string) error {
	percent = max(0, min(100, percent))
	q := "UPDATE documents SET progress_percent=$1"
	args := []any{int16(percent)}
	if pageCount != nil {
		q += ", page_count=$2"
		args = append(args, *pageCount)
	}
	args = append(args, id)
	q += fmt.Sprintf(" WHERE id=$%d", len(args))
	if owner != "" {
		args = append(args, owner)
		q += fmt.Sprintf(" AND claimed_by=$%d AND status='processing'", len(args))
	}
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 && owner != "" {
		return ErrNotFound
	}
	return nil
}

// DeleteDocument removes the document; chunks and embeddings cascade.
func (s *Store) DeleteDocument(ctx context.Context, id int64) error {
	res, err := s.pool.Exec(ctx, "DELETE FROM documents WHERE id = $1", id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
