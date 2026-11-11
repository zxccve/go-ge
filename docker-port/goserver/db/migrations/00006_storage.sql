-- +goose Up
-- Who owns which item. This is the table the Go server was missing: `item` holds a
-- row and its count, but nothing said whose bag it is in - so a script could not
-- count a player's items at all (`GetItemCountInInventory`, 1,364 call sites).
--
-- Shape follows the legacy STORAGE the Python oracle reads, minus the columns
-- nothing here uses yet:
--   type   - 0 is the bag; the original also used it for the warehouse and other
--            containers, which this side does not serve.
--   trash  - 1 marks a deleted-but-not-yet-purged row; every read filters it out,
--            and a take DELETES at count zero (the legacy proc relied on a cascade,
--            which is why the row must go rather than linger at 0).
--   insert_date - the bag ORDER. The client draws the inventory in the order the
--            rows were added, and `get_inventory_full` sorts by it, so a new item
--            must land at the end rather than in the middle.
--
-- Keyed by (commander_id, item_id): one item id belongs to one bag, and the legacy
-- schema allowed the same ITEM_ID twice, which would double-count a player's bag.
--
-- WHO WRITES IT: goserver\zone, on a committed `TxEnd` (`TxGiveItem`/`TxTakeItem`)
-- and, later, on a purchase. WHO READS IT: the same, for the counts a script asks.

CREATE TABLE storage (
    item_id     bigint      NOT NULL,
    commander_id integer    NOT NULL REFERENCES commander (commander_id) ON DELETE CASCADE,
    type        smallint    NOT NULL DEFAULT 0,
    trash       smallint    NOT NULL DEFAULT 0,
    insert_date timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (commander_id, item_id)
);

-- The count query is "everything in this commander's bag, grouped by class", so
-- the bag is the leading column and the joins walk item by item_id.
CREATE INDEX storage_item ON storage (item_id);
CREATE INDEX storage_bag ON storage (commander_id, insert_date);

-- +goose Down
DROP TABLE storage;
