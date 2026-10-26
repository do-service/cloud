-- What the sync folder already held when it was connected (a reinstall,
-- or a folder chosen with files in it): its top-level names. Those
-- entries are adopted as they are — paired with the cloud where it has
-- the same thing and it is checked, otherwise left alone — rather than
-- uploaded as new and checked, as something created there later is.
CREATE TABLE adopted (
  rel_path TEXT PRIMARY KEY
) STRICT;
