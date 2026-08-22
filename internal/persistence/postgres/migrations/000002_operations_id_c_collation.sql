-- LatestForResource orders Operations by requested_at_ns descending and then
-- by Operation ID descending. The "C" collation makes the identifier tiebreak
-- a byte-wise comparison, so the ordering is deterministic and independent of
-- database locale settings. Text equality (and therefore foreign keys) does
-- not depend on collation.
ALTER TABLE operations ALTER COLUMN id TYPE text COLLATE "C";
