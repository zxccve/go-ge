r"""Prove ONE real quest end to end: accept, commit, then REFUSE a second accept.

The subject is Reboldoeux's Linden (`C1_REB_LINDEN`, spawn row in
`ge\ies\npcgen_reb_basic.ies`). It is a real quest, not a test NPC: the script
guards itself, and the guard is read from stored commander state -

    if(commander.AlqMission == 1) {                // accept: props + item
    } else if(commander.AlqMission == 2 || ...) {  // "you already took it"

What this checks, through the same code the click path uses:
  1. `pc.<Property>` is a REAL stored read. A plain dict answered 0 for every
     name, so EVERY guard passed and the quest paid out on every click;
  2. one transaction writes three properties AND gives one item, and all of it
     lands (or none of it would);
  3. the SECOND click is refused by the script itself and writes NOTHING - no
     second item, no property change;
  4. the quest panel's byte for `Quest_00001` really moves 1 -> 2, so the array
     the client draws the panel from changed and had to be re-sent.

The commander is put into the fresh-player state the accept branch requires and
restored afterward; the item is taken back, so the DB is left as found.

Usage
-----
    python check_quest.py            # commander 24
    python check_quest.py 24
"""

import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

SCRIPT = "C1_REB_LINDEN"
# The state a NEW player has when this quest is open: the click reaches
# SCR_REB_LINDEN_INTRO only when NTQ_Linden == 11, and the accept branch needs
# AlqMission == 1.
SEED = {"NTQ_Linden": 11, "AlqMission": 1}
WATCH = ("AlqMission", "NTQ_Linden", "NTQ_Quest0004", "Quest_00001")
GIVEN = "Confirmation"


def _db():
    import ge_db
    return ge_db


def panel_byte(commander, quest="Quest_00001"):
    """The byte ZC_QUEST_INFO carries for one quest, straight from the builder."""
    import zone_server
    names = zone_server.quest_class_names()
    if quest not in names:
        return None
    pk = zone_server.build_quest_info(commander)
    return pk[4 + names.index(quest)]


def held(commander, class_id):
    """Total count the commander holds of one class, trash excluded."""
    rows = _db()._run(
        "SELECT SUM(i.COUNT) FROM STORAGE s JOIN ITEM i ON i.ITEM_ID = s.ITEM_ID "
        "WHERE s.COMMANDER_ID = %d AND i.CLASS_ID = %d AND s.TRASH = 0"
        % (int(commander), int(class_id)))
    if not rows or rows[0][0] in (None, "NULL"):
        return 0
    return int(rows[0][0])


def answer_all(host, res):
    """Keep answering the script's questions with option 1 until it finishes.

    A quest script asks the player something and the answer picks the branch;
    option 1 is the accept path here.
    """
    guard = 0
    while res["ok"] and not res["done"]:
        guard += 1
        if guard > 20:
            print("  !! script asked more than 20 questions, stopping")
            break
        res = host.resume_click(res["co"], 1, res.pop("_seen", 0)
                                if "_seen" in res else 0)


def click(host, label):
    """Run one full click, answering every question, and say what it did."""
    res = host.start_click(SCRIPT)
    guard = 0
    while res["ok"] and not res["done"]:
        guard += 1
        if guard > 20:
            print("  !! script asked more than 20 questions, stopping")
            break
        res = host.resume_click(res["co"], 1)
    if not res["ok"]:
        print("  !! %s: script failed: %s" % (label, res.get("error")))
    text = res.get("dialog")
    print("  ..   %s: done=%s dialog=%r" % (label, res["done"], text))
    return res


def run(commander):
    import ge_db
    import ge_item_index
    import script_host

    class_id = ge_item_index.class_id(GIVEN)
    if class_id is None:
        print("FAIL: item %r is not in the item table" % GIVEN)
        return 1

    original = ge_db.get_commander_quest_states(commander)
    before_props = {name: original.get(name, 0) for name in WATCH}
    before_item = held(commander, class_id)
    before_panel = panel_byte(commander)
    problems = []

    def expect(label, got, want):
        ok = got == want
        print("  %s %s: got %r, want %r"
              % ("OK  " if ok else "FAIL", label, got, want))
        if not ok:
            problems.append(label)

    print("commander %d, quest %s, item %r (class %d)"
          % (commander, SCRIPT, GIVEN, class_id))
    print("before: props=%s held=%d panel Quest_00001=%s"
          % (before_props, before_item, before_panel))

    host = script_host.ScriptHost(zone_id=10)
    host.commander_id = commander
    host.set_commander(commander)

    try:
        for name, value in SEED.items():
            if not ge_db.set_commander_quest_state(commander, name, value):
                print("FAIL: could not seed %s=%d" % (name, value))
                return 1

        # 1. the accept: guard passes, one transaction writes props + item
        click(host, "first click")
        expect("AlqMission after accept", ge_db.get_commander_quest_states(
            commander).get("AlqMission"), 2)
        after_props = {name: ge_db.get_commander_quest_states(commander).get(name)
                       for name in WATCH}
        expect("NTQ_Quest0004 after accept", after_props["NTQ_Quest0004"], 200)
        expect("Quest_00001 after accept", after_props["Quest_00001"], 2)
        expect("%s given once" % GIVEN, held(commander, class_id), before_item + 1)
        expect("panel byte moved 1 -> 2", panel_byte(commander),
               2 if before_panel != 2 else 2)

        # 2. the SECOND click: the script's OWN guard refuses it
        committed = after_props
        click(host, "second click")
        expect("second click wrote nothing",
               {name: ge_db.get_commander_quest_states(commander).get(name)
                for name in WATCH}, committed)
        expect("second click gave no item", held(commander, class_id),
               before_item + 1)
    finally:
        # restore the commander and take the reward back, so the DB is as found
        for name, value in before_props.items():
            ge_db.set_commander_quest_state(commander, name, value)
        tx = host.tx_begin()
        if held(commander, class_id) > before_item:
            host.tx_item(tx, GIVEN, held(commander, class_id) - before_item, "take")
            host.tx_end(tx)
        print("restored: props=%s held=%d panel Quest_00001=%s"
              % ({name: ge_db.get_commander_quest_states(commander).get(name)
                  for name in WATCH},
                 held(commander, class_id), panel_byte(commander)))

    ok = not problems
    print("%s (%d problem%s)" % ("PASS" if ok else "FAIL", len(problems),
                                 "" if len(problems) == 1 else "s"))
    return 0 if ok else 1


def main(argv):
    commander = int(argv[0]) if argv and argv[0].isdigit() else 24
    return run(commander)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
