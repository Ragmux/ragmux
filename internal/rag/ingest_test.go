package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

type noEmbedder struct{ t *testing.T }

func (n noEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	n.t.Error("embedder must not be called for a document over the chunk limit")
	return nil, errors.New("unexpected")
}

func TestProcessRejectsTooManyChunks(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "e", ProviderType: "custom_openai", BaseURL: "http://example.invalid/v1", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := st.CreateRAGStore(ctx, &store.RAGStore{Name: "s", EmbeddingConnectionID: conn.ID, ChunkSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	// Every heading starts a new section and therefore a new chunk.
	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, "# Section %d\n\nA paragraph of text.\n\n", i)
	}
	text := b.String()
	doc, err := st.CreateDocument(ctx, &store.Document{RAGStoreID: rs.ID, Filename: "many.md", SizeBytes: int64(len(text))}, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	factory := func(*store.ModelConnection) (provider.Embedder, error) { return noEmbedder{t}, nil }
	ing := NewIngester(ctx, st, factory, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer ing.Stop()
	ing.MaxChunksPerDocument = 3
	err = ing.Process(ctx, doc.ID)
	if err == nil || !strings.Contains(err.Error(), "MAX_CHUNKS_PER_DOCUMENT") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnqueueReportsFullQueue(t *testing.T) {
	ing := &Ingester{queue: make(chan int64, 1), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ing.Enqueue(1); err != nil {
		t.Fatal(err)
	}
	if err := ing.Enqueue(2); !errors.Is(err, ErrQueueFull) {
		t.Errorf("err = %v", err)
	}
}
