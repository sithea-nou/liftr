-- SPDX-License-Identifier: Apache-2.0

-- Supports the operational sampler's Dead-count aggregate (ADR-0018).
--
-- Terminal outbox rows are immutable and retained forever by design
-- (000001_initial.sql trigger), so `SELECT count(*) WHERE state = 'Dead'`
-- degrades into a full-history sequential scan as the queue grows. This
-- partial index keeps that sample an index-only scan over the (rare)
-- quarantined rows alone.
--
-- Plan evidence (PostgreSQL 17, seeded suite):
--   without index: Seq Scan on outbox_messages  (rows = all-time messages)
--   with index:    Index Only Scan using outbox_dead
-- pinned by TestOperationalSamplerQueriesRideIndexes in the postgres
-- integration suite.

CREATE INDEX IF NOT EXISTS outbox_dead
    ON outbox_messages (dead_at)
    WHERE state = 'Dead';
