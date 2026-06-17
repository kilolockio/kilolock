-- orphaned_data_sources.sql
-- Data sources (mode = 'data') that no managed resource depends on.
-- Candidates for removal, or signs of dead code in modules.

SELECT
    state_name,
    address  AS data_source
FROM   current_resources r
WHERE  r.mode = 'data'
  AND  NOT EXISTS (
           SELECT 1
           FROM   current_resource_dependencies rd
           WHERE  rd.to_resource_id = r.id
       )
ORDER  BY state_name, data_source;
