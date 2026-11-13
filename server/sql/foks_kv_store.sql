
CREATE TABLE root (
    short_host_id SMALLINT NOT NULL,
    party_id BYTEA NOT NULL,
    root_node_id BYTEA NOT NULL, -- must be a directory and cannot be NULL
    root_node_version INT NOT NULL, -- incremented whenever the root node is bumped (first is n=1)
    ptk_gen INT NOT NULL,
    read_role_type SMALLINT NOT NULL,
    read_role_viz_level SMALLINT NOT NULL,
    binding_mac BYTEA NOT NULL, -- binds the root node (and version) to the party_id
    ctime TIMESTAMP NOT NULL,
    mtime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, party_id)
);

CREATE TYPE dir_status as ENUM('dead', 'active', 'encrypting');

CREATE TABLE dir (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    dir_id BYTEA NOT NULL,
    version INT NOT NULL,
    ptk_gen INT NOT NULL,
    read_role_type SMALLINT NOT NULL,
    read_role_viz_level SMALLINT NOT NULL,
    write_role_type SMALLINT NOT NULL,
    write_role_viz_level SMALLINT NOT NULL,
    seed_box BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    mtime TIMESTAMP NOT NULL,
    status dir_status NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, dir_id, version)
);

CREATE INDEX dir_mtime_gc_idx ON dir(mtime) WHERE (status = 'dead');

CREATE TABLE dir_refcount (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    dir_id BYTEA NOT NULL,
    refcount INT NOT NULL,
    mtime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, dir_id)
);

CREATE INDEX dir_refcount_mtime_gc_idx ON dir_refcount(mtime) WHERE (refcount = 0);

CREATE TABLE dirent (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    dir_id BYTEA NOT NULL,
    dirent_id BYTEA NOT NULL,
    version INT NOT NULL,
    dir_version INT NOT NULL,
    name_box BYTEA NOT NULL, -- encoded SecretBox
    value BYTEA NOT NULL,
    write_role_type SMALLINT NOT NULL,
    write_role_viz_level SMALLINT NOT NULL,
    name_mac BYTEA NOT NULL,
    binding_mac BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    active BOOLEAN NOT NULL, -- only max version is active
    PRIMARY KEY(short_host_id, short_party_id, dir_id, dirent_id, version),
    FOREIGN KEY(short_host_id, short_party_id, dir_id, dir_version) REFERENCES dir(short_host_id, short_party_id, dir_id, version)
);

CREATE INDEX dirent_ctime_idx ON dirent(short_host_id, short_party_id, dir_id, ctime) 
   WHERE (active = true);

/*
 * Locks are held per dirent. If you have /a/b/c and a hardlink /x/y/c that points to /a/b/c, they will be
 * two different locks. Let's try this system for now. Used in implementing billyFS for git, especially
 * packing and unpacking.
 */
CREATE TABLE locks (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    dir_id BYTEA NOT NULL,
    dirent_id BYTEA NOT NULL,
    lock_id BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, dir_id, dirent_id)
);

CREATE INDEX dirent_dir_idx ON dirent(short_host_id, short_party_id, dir_id, dir_version, name_mac);
CREATE INDEX dirent_ctime_gc_idx ON dirent(ctime) WHERE (active = false);
CREATE INDEX dirent_name_idx ON dirent(short_host_id, short_party_id, dir_id, name_mac, version);
CREATE INDEX dirent_list_idx ON dirent(short_host_id, short_party_id, dir_id, name_mac) WHERE (active = true);

CREATE TYPE storage_type as ENUM('sql'); /* will eventually include s3 */
CREATE TYPE large_file_status as ENUM('dead', 'active', 'uploading');

/*
 * All large_files are stored here, but depending on configuration, chunks can be stored in the large_file_chunk
 * table, or a file storage like S3.
 */
CREATE TABLE large_file (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    file_id BYTEA NOT NULL,
    size INT NOT NULL,
    refcount INT NOT NULL,
    status large_file_status NOT NULL,
    storage_type storage_type NOT NULL,
    ctime TIMESTAMP NOT NULL,
    mtime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, file_id)
);

CREATE TABLE large_file_key (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    file_id BYTEA NOT NULL,
    version INT NOT NULL,
    read_role_type SMALLINT NOT NULL,
    read_role_viz_level SMALLINT NOT NULL,
    ptk_gen INT NOT NULL,
    key_box BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    mtime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, file_id, version),
    FOREIGN KEY(short_host_id, short_party_id, file_id) REFERENCES large_file(short_host_id, short_party_id, file_id)
      ON DELETE CASCADE
);

CREATE INDEX large_file_mtime_gc_idx ON large_file(mtime) WHERE (refcount = 0);
CREATE INDEX large_file_mtime_gc_active_idx ON large_file(mtime) WHERE (status = 'dead');
CREATE INDEX large_file_gc_unlinked_idx ON large_file(mtime)
    WHERE (refcount = 0 AND status = 'active');
CREATE INDEX large_file_gc_uploading_idx ON large_file(mtime)
    WHERE (status = 'uploading');

CREATE TABLE large_file_chunk (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    file_id BYTEA NOT NULL,
    chunk_offset INT NOT NULL,
    data BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    final BOOLEAN NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, file_id, chunk_offset),
    FOREIGN KEY(short_host_id, short_party_id, file_id) REFERENCES large_file(short_host_id, short_party_id, file_id)
      ON DELETE CASCADE
);

CREATE TABLE small_file_or_symlink (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    node_id BYTEA NOT NULL, -- can be a small file ID or a symlink ID
    ptk_gen INT NOT NULL,
    read_role_type SMALLINT NOT NULL,
    read_role_viz_level SMALLINT NOT NULL,
    size INT NOT NULL,
    box BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    mtime TIMESTAMP NOT NULL,
    refcount INT NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, node_id)
);

CREATE INDEX small_file_mtime_gc_idx ON small_file_or_symlink(mtime) WHERE (refcount = 0);

CREATE TABLE usage (
    short_host_id SMALLINT NOT NULL,
    party_id BYTEA NOT NULL,
    num_small INT NOT NULL,
    num_large INT NOT NULL,
    num_large_chunks INT NOT NULL,
    sum_small BIGINT NOT NULL,
    sum_large BIGINT NOT NULL,
    PRIMARY KEY(short_host_id, party_id)
);

CREATE TABLE usage_vhost (
    short_host_id SMALLINT NOT NULL PRIMARY KEY,
    num_small INT NOT NULL,
    num_large INT NOT NULL,
    num_large_chunks INT NOT NULL,
    sum_small BIGINT NOT NULL,
    sum_large BIGINT NOT NULL
);

CREATE TABLE quota_check (
    short_host_id SMALLINT NOT NULL,
    party_id BYTEA NOT NULL,
    num_new_writes INT NOT NULL,
    check_time TIMESTAMP NOT NULL,
    in_quota BOOLEAN NOT NULL,
    PRIMARY KEY(short_host_id, party_id)
);

CREATE TABLE quota_check_vhost (
    short_host_id SMALLINT NOT NULL PRIMARY KEY,
    num_new_writes INT NOT NULL,
    check_time TIMESTAMP NOT NULL,
    in_quota BOOLEAN NOT NULL
);

CREATE INDEX quota_check_idx ON quota_check(short_host_id, check_time) WHERE (num_new_writes > 0);
CREATE INDEX quota_check_vhost_idx ON quota_check_vhost(check_time) WHERE (num_new_writes > 0);

CREATE TABLE schema_patches (
    id INTEGER NOT NULL PRIMARY KEY,
    ctime TIMESTAMP NOT NULL
);
INSERT INTO schema_patches (id, ctime) VALUES (1, NOW());
