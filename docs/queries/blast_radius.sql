-- blast_radius.sql
-- Transitive closure of dependencies starting from a resource.
-- "If this VPC changes, what is downstream?" This is the kind of query
-- recursive CTEs make trivial in Postgres and hard in flat .tfstate.

WITH RECURSIVE seed AS (
    SELECT r.id, r.address, r.type, 0 AS depth
    FROM   current_resources r
    WHERE  r.state_name = 'prod'                 -- EDIT ME
      AND  r.address    = 'aws_vpc.main'         -- EDIT ME
),
reachable AS (
    SELECT id, address, type, depth FROM seed
    UNION
    SELECT r.id, r.address, r.type, reachable.depth + 1
    FROM   reachable
    JOIN   current_resource_dependencies rd ON rd.from_resource_id = reachable.id
    JOIN   current_resources r ON r.id = rd.to_resource_id
)
SELECT depth, address, type
FROM   reachable
ORDER  BY depth, address;
