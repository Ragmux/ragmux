package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Document status values.
const (
	DocPending    = "pending"
	DocProcessing = "processing"
	DocReady      = "ready"
	DocFailed     = "failed"
)

// Document is an uploaded file attached to a RAG store.
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
	err := row.Scan(&d.ID, &d.RAGStoreID, &d.Filename, &d.Mime, &d.SizeBytes, &d.Status, &d.Error,
		&d.ChunkCount, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, scanErr(err)
	}
	return d, nil
}

// DocumentPath returns where a document's raw bytes live on disk.
func (s *Store) DocumentPath(d *Document) string {
	return filepath.Join(s.UploadsDir, strconv.FormatInt(d.ID, 10), filepath.Base(d.Filename))
}

// CreateDocument records a pending document; the caller writes the file to
// DocumentPath afterwards.
func (s *Store) CreateDocument(ctx context.Context, d *Document) (*Document, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO documents (rag_store_id, filename, mime, size_bytes, status)
		VALUES (?, ?, ?, ?, ?)`, d.RAGStoreID, d.Filename, d.Mime, d.SizeBytes, DocPending)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetDocument(ctx, id)
}

// GetDocument fetches one document.
func (s *Store) GetDocument(ctx context.Context, id int64) (*Document, error) {
	return scanDoc(s.db.QueryRowContext(ctx, "SELECT "+docCols+" FROM documents WHERE id = ?", id))
}

// ListDocuments lists the documents of one store.
func (s *Store) ListDocuments(ctx context.Context, storeID int64) ([]*Document, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+docCols+" FROM documents WHERE rag_store_id = ? ORDER BY id DESC", storeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// ListDocumentsByStatus returns documents in any of the given states.
func (s *Store) ListDocumentsByStatus(ctx context.Context, statuses ...string) ([]*Document, error) {
	if len(statuses) == 0 {
		return []*Document{}, nil
	}
	q := "SELECT " + docCols + " FROM documents WHERE status IN ("
	args := make([]any, len(statuses))
	for i, st := range statuses {
		if i > 0 {
			q += ","
		}
		q += "?"
		args[i] = st
	}
	q += ") ORDER BY id"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	_, err := s.db.ExecContext(ctx, "UPDATE documents SET status=?, error=?, updated_at=? WHERE id=?",
		status, errMsg, now(), id)
	return err
}

// DeleteDocument removes the document row, its chunks, vectors and file.
func (s *Store) DeleteDocument(ctx context.Context, id int64) error {
	d, err := s.GetDocument(ctx, id)
	if err != nil {
		return err
	}
	r, err := s.GetRAGStore(ctx, d.RAGStoreID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.VecAvailable && r.Dimensions > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)", vecTable(r.Dimensions)), id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM documents WHERE id = ?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	os.RemoveAll(filepath.Dir(s.DocumentPath(d)))
	return nil
}
