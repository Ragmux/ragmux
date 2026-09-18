package store

import (
	"context"
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
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

const docCols = "id, rag_store_id, filename, mime, size_bytes, status, error, chunk_count, created_at, updated_at"

func scanDoc(row interface{ Scan(...any) error }) (*Document, error) {
	d := &Document{}
	var created, updated time.Time
	err := row.Scan(&d.ID, &d.RAGStoreID, &d.Filename, &d.Mime, &d.SizeBytes, &d.Status, &d.Error,
		&d.ChunkCount, &created, &updated)
	if err != nil {
		return nil, scanErr(err)
	}
	d.CreatedAt, d.UpdatedAt = ts(created), ts(updated)
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

// SetDocumentStatus updates processing state.
func (s *Store) SetDocumentStatus(ctx context.Context, id int64, status, errMsg string) error {
	_, err := s.pool.Exec(ctx, "UPDATE documents SET status=$1, error=$2, updated_at=now() WHERE id=$3",
		status, errMsg, id)
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
