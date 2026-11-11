r"""Prove Leonardo's "Increase Inventory" service really works.

It is the first script that spends money, so it checks the two host bugs that
made it impossible before:

  1. `GetItemCountInInventory(pc, 'Feso')` was stubbed to 0, so the script's own
     affordability test (`if (GetItemCountInInventory(...) < Take2)`) always fired
     and the player was told "You don't have sufficient Feso" while holding
     millions. It is now a real count.
  2. `IMCFormat("<$>10137067</>", 50000)` formatted the TOKEN, which has no `%d`
     in it, so the player read a literal `%d`. The token is resolved to text
     first now.

What it does, through the same click path the server uses: click Leonardo ->
menu "inventory" -> "use the service" -> "10 slots", then reads the DB. It
restores the Feso and the slot count afterwards, so the character is unchanged.

Run from docker-port\src. It needs no server.

    python check_inventory_service.py           # commander 24
    python check_inventory_service.py 24
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

SCRIPT = "LEO_SERVICE"
PROP = "Lock_InventoryCount"
COIN = "Feso"
WANT = 10              # the "10 slots (discount)" option
PRICE = 460000         # Take2 in leonardo_def.scp


def _db():
    import ge_db
    return ge_db


def held(commander, class_id):
    rows = _db()._run(
        "SELECT SUM(i.COUNT) FROM STORAGE s JOIN ITEM i ON i.ITEM_ID = s.ITEM_ID "
        "WHERE s.COMMANDER_ID = %d AND i.CLASS_ID = %d AND s.TRASH = 0"
        % (int(commander), int(class_id)))
    if not rows or rows[0][0] in (None, "NULL"):
        return 0
    return int(rows[0][0])


def slots(commander):
    return _db().get_commander_quest_states(commander).get(PROP, 0)


def answer(host, res, choice, label):
    """Answer the next question and say what was asked."""
    prompt = res["question"]["prompt"]
    options = res["question"]["options"]
    print("  ..   %s: prompt=%r options=%d -> choose %d"
          % (label, prompt, len(options), choice))
    if not 1 <= choice <= len(options):
        raise RuntimeError("%s: only %d options, cannot choose %d"
                           % (label, len(options), choice))
    return host.resume_click(res["co"], choice)


def run(commander):
    import ge_item_index
    import script_host

    class_id = ge_item_index.class_id(COIN)
    before_feso = held(commander, class_id)
    before_slots = slots(commander)
    problems = []

    def expect(label, got, want):
        ok = got == want
        print("  %s %s: got %r, want %r"
              % ("OK  " if ok else "FAIL", label, got, want))
        if not ok:
            problems.append(label)

    print("commander %d: slots=%d feso=%d" % (commander, before_slots, before_feso))
    if not 248 <= before_slots <= 288:
        print("SKIP: this test needs slots between 248 and 288 (the 10-slot menu)")
        return 0
    if before_feso < PRICE:
        print("SKIP: needs at least %d Feso for the 10-slot option" % PRICE)
        return 0

    host = script_host.ScriptHost(zone_id=10)
    host.commander_id = commander
    host.set_commander(commander)
    try:
        # click -> menu; the inventory option is 5 when slots < 298
        res = host.start_click(SCRIPT)
        res = answer(host, res, 5, "menu")
        res = answer(host, res, 2, "service")
        res = answer(host, res, 3, "how many")
        if not res["ok"]:
            print("  !! script failed: %s" % res.get("error"))
        if not res["done"]:
            print("  !! script is still waiting for an answer")

        expect("slots increased by %d" % WANT, slots(commander), before_slots + WANT)
        expect("feso paid", held(commander, class_id), before_feso - PRICE)
        # the script reads the property back AFTER the commit and prints it
        said = [c.args[1] for c in host.calls
                if c.name == "ShowOkDlg" and len(c.args) > 1]
        expect("the success line shows the NEW count",
               str(before_slots + WANT) in str(said[-1] if said else ""), True)
        spent = [(c.name, c.args) for c in host.calls if c.name in
                 ("TxTakeItem", "TxSetCommProp", "TxEnd")]
        for name, args in spent:
            print("  ..   %s%r" % (name, args))
    finally:
        # put everything back: the fee, then the slot count
        now_feso = held(commander, class_id)
        if now_feso < before_feso:
            tx = host.tx_begin()
            host.tx_item(tx, COIN, before_feso - now_feso, "give")
            host.tx_end(tx)
        _db().set_commander_quest_state(commander, PROP, before_slots)
        print("restored: slots=%d feso=%d"
              % (slots(commander), held(commander, class_id)))

    ok = not problems
    print("%s (%d problem%s)" % ("PASS" if ok else "FAIL", len(problems),
                                 "" if len(problems) == 1 else "s"))
    return 0 if ok else 1


def main(argv):
    commander = int(argv[0]) if argv and argv[0].isdigit() else 24
    return run(commander)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
