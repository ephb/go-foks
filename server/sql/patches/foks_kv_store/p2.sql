-- Who uploaded each large file (server/kv-store/writer.go, kvFileInfo).
--
-- large_file.writer holds the uploader as an encoded FQParty: the user in a
-- user's own store, or the team member whose bearer token the upload used.
-- kvFileInfo hands it to members who may read the file, so an application
-- can check that a file it is about to delete is the caller's own.
--
-- Files uploaded before this patch keep a NULL writer: nothing records who
-- wrote them. A server that runs without this patch keeps working; it just
-- records no writers until the patch is in.
--
-- Safe on a live server: adding a nullable column without a default is a
-- catalog change only, no table rewrite.

ALTER TABLE large_file ADD COLUMN writer BYTEA;
