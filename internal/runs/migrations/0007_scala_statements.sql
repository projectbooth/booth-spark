-- Scala sessions (ADR 0121 ruling 5; docs/design-scala.md): a Scala-flavoured session's statements
-- are of kind scala, beside sql. A session's language is in its spec (Language), not a column.
ALTER TABLE statements DROP CONSTRAINT statements_kind_check;
ALTER TABLE statements ADD CONSTRAINT statements_kind_check CHECK (kind IN ('sql', 'python', 'scala'));
