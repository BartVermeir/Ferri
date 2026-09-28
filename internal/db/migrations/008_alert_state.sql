-- State of the admin alerts (jobs/alerts.go), one row per kind of alert.
-- since:        when the condition was first seen, NULL = not active now.
--               Some alerts only go out once it lasted long enough.
-- last_sent_at: when the last alert of this kind was mailed; at most one per
--               kind per 24 hours. For failed mails it also marks which
--               failures were already reported.
CREATE TABLE IF NOT EXISTS alert_state (
    kind          TEXT    NOT NULL PRIMARY KEY,
    since         INTEGER,
    last_sent_at  INTEGER
);
