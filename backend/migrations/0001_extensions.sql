-- Extensions the schema depends on.
-- vector: embeddings for entity matching and Ask-the-Canon retrieval.
-- citext: case-insensitive text (emails, entity aliases).
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS citext;
