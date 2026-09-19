package store

import (
	"context"
	"strings"
	"time"
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
	Attempts  int    `json:"attempts"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

const docCols = "id, rag_store_id, filename, mime, size_bytes, status, error, chunk_count, page_count, progress_percent, attempts, created_at, updated_at"

// docColsD is docCols qualified for statements that join another relation
// and would otherwise read ambiguously.
var docColsD = "d." + strings.ReplaceAll(docCols, ", ", ", d.")

func scanDoc(row interface{ Scan(...any) error }) (*Document, error) {
	d := &Document{}
	var created, updated time.Time
	var progress int16
	err := row.Scan(&d.ID, &d.RAGStoreID, &d.Filename, &d.Mime, &d.SizeBytes, &d.Status, &d.Error,
		&d.ChunkCount, &d.PageCount, &progress, &d.Attempts, &created, &updated)
	if err != nil {
		return nil, scanErr(err)
	}
	d.CreatedAt, d.UpdatedAt = ts(created), ts(updated)
	d.ProgressPercent = int(progress)
	return d, nil
}

// CreateDocument records a pending document together with its raw bytes.
func (s *Store) CreateDocument(ctx context.Context, d *Document, content []byte) (*Document, error) {
	if content == nil {
		content = []byte{}
	}
	return scanDoc(s.pool.QueryRow(ctx, `INSERT INTO documents (rag_store_id, filename, mime, size_bytes, content, status)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+docCols,
		d.RAGStoreID, d.Filename, d.Mime, d.SizeBytes, content, DocPending))
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

// ClaimDocumentByID claims one named document, whatever its status, unless
// another owner still holds a live lease on it. It backs Ingester.Process,
// which runs a document on demand rather than off the queue.
func (s *Store) ClaimDocumentByID(ctx context.Context, id int64, owner string, lease time.Duration, maxAttempts int) (*Document, error) {
	return scanDoc(s.pool.QueryRow(ctx, `WITH next AS (
			SELECT id FROM documents
			 WHERE id = $4
			   AND (claimed_by IS NULL OR claimed_by = $1 OR lease_until IS NULL OR lease_until < now())
			   AND attempts < $3
			 FOR UPDATE SKIP LOCKED)
		UPDATE documents d SET `+claimSet+`
		  FROM next WHERE d.id = next.id
		RETURNING `+docColsD, owner, lease.Seconds(), maxAttempts, id))
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
func (s *Store) SetDocumentProgress(ctx context.Context, id int64, percent int, pageCount *int) error {
	percent = max(0, min(100, percent))
	if pageCount != nil {
		_, err := s.pool.Exec(ctx, "UPDATE documents SET progress_percent=$1, page_count=$2 WHERE id=$3", int16(percent), *pageCount, id)
		return err
	}
	_, err := s.pool.Exec(ctx, "UPDATE documents SET progress_percent=$1 WHERE id=$2", int16(percent), id)
	return err
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
