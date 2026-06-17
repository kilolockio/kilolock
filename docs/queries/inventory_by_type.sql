-- inventory_by_type.sql
-- Counts of resources by type across every managed state.
-- Useful for at-a-glance inventory: "how many EC2 instances do we
-- actually have?", "are we paying for that many NAT gateways?".

SELECT
    type                                    AS resource_type,
    COUNT(*)                                AS instances,
    COUNT(DISTINCT state_name)              AS in_states
FROM   current_resources
GROUP  BY type
ORDER  BY instances DESC, resource_type;
