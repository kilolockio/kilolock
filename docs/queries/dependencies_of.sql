-- dependencies_of.sql
-- Direct dependencies of one resource: "what does X depend on?"
-- Edit the WHERE clause to pick a resource address.

SELECT
    r2.address  AS depends_on,
    r2.type     AS depends_on_type
FROM   current_resources r1
JOIN   current_resource_dependencies rd ON rd.from_resource_id = r1.id
JOIN   current_resources r2 ON r2.id = rd.to_resource_id
WHERE  r1.state_name = 'prod'                    -- EDIT ME
  AND  r1.address    = 'aws_vpc.main'            -- EDIT ME
ORDER  BY r2.address;
