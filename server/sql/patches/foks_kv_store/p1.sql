-- Indexes for the large-file garbage collector (server/kv-store/gc.go,
-- kv_store.gc in the server config).
--
-- The GC collects finished files that nothing links to (status 'active',
-- refcount 0) and uploads that stalled (status 'uploading'), oldest first.
-- The existing large_file_mtime_gc_idx (refcount = 0) also holds the
-- tombstones of files already collected, which only grow; these two partial
-- indexes hold only what the GC still has to look at.
--
-- Safe on a live server: it only adds indexes. Writes to large_file wait
-- while they build, a moment for a table of a few hundred thousand rows.
-- The GC works without them, only more slowly. This is the first patch for
-- the KV shards; patch-db creates their schema_patches table.

CREATE INDEX large_file_gc_unlinked_idx ON large_file(mtime)
    WHERE (refcount = 0 AND status = 'active');

CREATE INDEX large_file_gc_uploading_idx ON large_file(mtime)
    WHERE (status = 'uploading');
