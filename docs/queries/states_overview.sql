-- states_overview.sql
-- One-line summary per state. The same data the `kl list` CLI
-- prints, exposed as SQL so it can be combined with WHERE/JOIN/UNION.

SELECT
    s.name                                        AS state_name,
    sv.serial                                     AS serial,
    sv.terraform_version                          AS tf_version,
    (SELECT COUNT(*) FROM resources r
     WHERE  r.state_id = s.id
       AND  r.delete_serial IS NULL)              AS resources,
    EXISTS (SELECT 1 FROM state_locks l
            WHERE  l.state_id = s.id)             AS is_locked,
    to_char(s.updated_at AT TIME ZONE 'UTC',
            'YYYY-MM-DD"T"HH24:MI:SS"Z"')         AS updated_at
FROM   states s
LEFT   JOIN state_versions sv ON sv.id = s.current_version_id
ORDER  BY s.name;
