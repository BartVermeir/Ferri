-- Number of files the sender announced at POST /send. The transfer activates
-- once that many files are complete (TransferStore.TryActivate); upload.js
-- creates each file's row only when that file starts uploading.
-- NULL = created before this column existed; those activate once none of their
-- files is incomplete.
ALTER TABLE transfers ADD COLUMN expected_files INTEGER;
