-- +goose Up
-- THE ADDRESS THE CLIENT IS TOLD TO DIAL, IN THE DATABASE AND NOT IN THE BINARY.
--
-- WHAT WAS WRONG. Two Go constants decided where a player is sent: `presenceIP`
-- (main.go), written into the presence file each zone publishes, and
-- `DefaultZoneAddr` (barrack\handler.go), the barrack's answer when no presence
-- file names the map. Both said 127.0.0.1, so a player on a SECOND PC could not
-- connect at all - and nothing logged an error, because nothing was wrong on this
-- machine. The owner's words: "we need to move the IP to database just like how
-- original server did it. that way is configurable and not hardcoded in the code."
--
-- WHAT THE ORIGINAL DID, and this table is that idea and no more. The shipped v28
-- server kept an address book in its RESOURCE database - MACHINE (MACHINE_ID,
-- GROUP_ID, MACHINE_NAME, MACHINE_IP), beside PVP_MACHINE (which adds a PORT) and
-- SERVER_INFO (SERVER_ID, NAME, PORT) - so a machine could be moved or added
-- without a rebuild (docker-port\history\shipped-binaries.md:364-379).
--
-- WHY ONLY THE IP IS HERE AND NOT A PORT. A town's port is the one its own
-- listener BOUND: the presence file carries the port that actually opened, and a
-- configured port that disagrees with the listener would point the client at a
-- closed door. So the address book answers the MACHINE's address (which ip), and
-- every role keeps reporting the port it really has. MACHINE_IP is the only part
-- that ever needed to be configurable.
--
-- WHY A NAMED ROW AND NOT ONE GLOBAL. The servers already run one process per
-- town and will spread over a VPS later; a name per machine means the same
-- database can serve a LAN test and the real host without editing code. Each
-- process reads the row named by its -machine flag (default 'local'), and the
-- -advertise flag overrides the row for a one-off run.
--
-- THE SEEDED ROW IS TODAY'S BEHAVIOUR, ON PURPOSE: ip 127.0.0.1 means every
-- existing start line, presence file and barrack answer stays exactly as it was
-- until someone runs one UPDATE.
--
-- IDEMPOTENT ON PURPOSE. Every statement here is safe to run twice: goose
-- normally applies this once, but a hand-applied run (psql, for evidence) must
-- not leave the next server start facing a half-made table.
CREATE TABLE IF NOT EXISTS machine (
    machine_id   integer     PRIMARY KEY,
    machine_name text        NOT NULL UNIQUE,
    machine_ip   text        NOT NULL,
    note         text,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE machine IS
    'The address book: which ip this machine tells the game client to dial. The '
    'Go servers read the row named by -machine at startup (geserver\address). '
    'A port is deliberately absent: a town reports the port its listener bound.';

COMMENT ON COLUMN machine.machine_ip IS
    'IPv4/IPv6 literal, or a resolvable host name. READ BY THE CLIENT, so it must '
    'be an address the PLAYER''s machine can reach - not 127.0.0.1 if they play '
    'on another PC. Blank is treated as missing: the server then uses 127.0.0.1 '
    'and logs one line saying so.';

INSERT INTO machine (machine_id, machine_name, machine_ip, note)
VALUES (1, 'local', '127.0.0.1',
        'this machine, as it was before the address book existed')
ON CONFLICT (machine_id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS machine;
