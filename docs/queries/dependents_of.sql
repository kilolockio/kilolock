-- dependents_of.sql
-- Direct dependents of one resource: "if I touch X, who breaks?"
-- The reverse edge from dependencies_of.sql.

SELECT
    r1.address  AS depended_on_by,
    r1.type     AS depended_on_by_type
FROM   current_resources r2
JOIN   current_resource_dependencies rd ON rd.to_resource_id = r2.id
JOIN   current_resources r1 ON r1.id = rd.from_resource_id
WHERE  r2.state_name = 'prod'                    -- EDIT ME
  AND  r2.address    = 'aws_vpc.main'            -- EDIT ME
ORDER  BY r1.address;
