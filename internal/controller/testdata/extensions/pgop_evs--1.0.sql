-- pgop test fixture: calls f(1) unqualified, so an f(integer) planted in
-- the schema beforehand would be called instead of this f(numeric), as the
-- superuser running the script.
CREATE FUNCTION f(numeric) RETURNS text LANGUAGE sql AS 'SELECT ''extension''';
CREATE TABLE ran AS SELECT f(1) AS result;
