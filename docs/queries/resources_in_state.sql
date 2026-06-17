-- resources_in_state.sql
-- Every resource currently in a single state, with mode, type, and module path.
-- Edit the WHERE clause to pick the state you care about.

SELECT
    address,
    mode,
    type,
    COALESCE(NULLIF(module_path, ''), '(root)') AS module
FROM   current_resources
WHERE  state_name = 'prod'        -- EDIT ME
ORDER  BY module_path, type, resource_name, index_value;
