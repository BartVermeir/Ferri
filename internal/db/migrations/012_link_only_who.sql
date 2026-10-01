-- A download of a link-only transfer shows as "shared link", not as the
-- sender's address on its one recipient row. Admin downloads stay "admin".
UPDATE download_streams SET who = 'shared link'
WHERE who <> 'admin'
  AND transfer_id IN (SELECT id FROM transfers WHERE notify_recipients = 0);
