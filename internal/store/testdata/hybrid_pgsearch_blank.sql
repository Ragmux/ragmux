
			WITH v AS (
				SELECT e.chunk_id, ROW_NUMBER() OVER (ORDER BY e.embedding <=> $1::vector) AS rank
				FROM chunk_embeddings_4 e
				WHERE e.rag_store_id = $2
				ORDER BY e.embedding <=> $1::vector
				LIMIT $3),
			f AS (
				SELECT NULL::bigint AS chunk_id, 0::float8 AS lex_score, 0::bigint AS rank
				WHERE false),
			m AS (
				SELECT COALESCE(v.chunk_id, f.chunk_id) AS chunk_id,
				       COALESCE(v.rank, 0) AS vrank, COALESCE(f.rank, 0) AS frank,
				       COALESCE(f.lex_score, 0) AS lex_score,
				       COALESCE(1.0/(60+v.rank), 0) + COALESCE(1.0/(60+f.rank), 0) AS score
				FROM v FULL OUTER JOIN f ON v.chunk_id = f.chunk_id)
			SELECT m.chunk_id, c.document_id, c.idx, c.content, d.filename,
			       COALESCE(c.metadata->>'section', ''), COALESCE((c.metadata->>'page')::int, 0),
			       e.embedding <=> $1::vector AS distance, m.score, m.vrank, m.frank, m.lex_score
			FROM m
			JOIN chunks c ON c.id = m.chunk_id
			JOIN documents d ON d.id = c.document_id
			JOIN chunk_embeddings_4 e ON e.chunk_id = m.chunk_id
			WHERE ($4::float8 = 0 OR (e.embedding <=> $1::vector) <= $4::float8)
			ORDER BY m.score DESC, distance, m.chunk_id
			LIMIT $3