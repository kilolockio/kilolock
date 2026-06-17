-- unencrypted_s3_buckets.sql
-- JSONB attribute query: find any aws_s3_bucket whose attributes don't
-- include server-side encryption configuration.
--
-- Schemas drift between provider versions; this query is best-effort
-- and demonstrates the *shape* of attribute queries, not a definitive
-- compliance check. Adjust the JSON path expressions to match the
-- provider version you actually use.

SELECT
    state_name,
    address                                                   AS bucket,
    attributes -> 'arn'                                       AS arn
FROM   current_resources
WHERE  type = 'aws_s3_bucket'
  AND  COALESCE(
           attributes -> 'server_side_encryption_configuration',
           '[]'::jsonb
       ) = '[]'::jsonb
ORDER  BY state_name, bucket;
