-- +goose Up
-- Indexes for the read paths that run on every page render. 001_initial only
-- covered items(feed_id), items(date) and items(read), which left the folder
-- view, the starred view, the sidebar unread counts and the feeds.folder
-- foreign key doing full table scans.
--
-- Everything here must be valid on SQLite *and* PostgreSQL, because both
-- backends run this one file. Partial indexes (INDEX ... WHERE) and DESC
-- columns are supported by both.

-- Folder view: WHERE items.folder = ? ORDER BY date DESC.
-- The composite index serves the filter and the sort together, so the
-- database never has to sort the matching rows.
CREATE INDEX IF NOT EXISTS idx_items_folder_date ON items(folder, date DESC);

-- Feed view: WHERE items.feed_id = ? ORDER BY date DESC. idx_items_feed from
-- 001 covers the filter but not the sort.
CREATE INDEX IF NOT EXISTS idx_items_feed_date ON items(feed_id, date DESC);

-- Starred view: WHERE starred = 1 ORDER BY date DESC. Partial, because
-- starred items are a small minority of the table and an index over the
-- unstarred majority would be dead weight on every insert.
CREATE INDEX IF NOT EXISTS idx_items_starred_date ON items(date DESC) WHERE starred = 1;

-- Sidebar per-feed unread badge:
--   SELECT feed_id, COUNT(*) FROM items WHERE read = 0 GROUP BY feed_id
-- This runs on every single render of the sidebar. The partial index lets it
-- be answered by an index scan over just the unread rows, grouped in index
-- order, instead of scanning every item ever fetched.
CREATE INDEX IF NOT EXISTS idx_items_unread_feed ON items(feed_id) WHERE read = 0;

-- feeds.folder is a foreign key with no index. Deleting a folder (and, on
-- PostgreSQL, any check of the referencing side) had to scan feeds while
-- holding locks.
CREATE INDEX IF NOT EXISTS idx_feeds_folder ON feeds(folder);

-- +goose Down
DROP INDEX IF EXISTS idx_feeds_folder;
DROP INDEX IF EXISTS idx_items_unread_feed;
DROP INDEX IF EXISTS idx_items_starred_date;
DROP INDEX IF EXISTS idx_items_feed_date;
DROP INDEX IF EXISTS idx_items_folder_date;
