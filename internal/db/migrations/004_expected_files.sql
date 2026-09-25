-- Number of files the sender announced at POST /send. The transfer activates
-- once that many files are complete (TransferStore.TryActivate), not as soon as
-- the files that happen to exist are complete: upload.js creates each file's
-- row only when that file starts uploading, so after file 1 there was nothing
-- left "incomplete" and multi-file transfers went live after the first file.
-- NULL = created before this column existed; those keep the old rule.
ALTER TABLE transfers ADD COLUMN expected_files INTEGER;
