"""check_docs - report doc line counts vs their ceilings and index gaps.

Run from src\\:
    python check_docs.py

Mirrors the guide sizes in ..\\README.md -> "How these docs work". Reports:
  - any doc past its guide size (WARNING - does not fail)
  - any doc at 90% or more of its guide but not over ("near ceiling", a WARNING)
  - any history\\*.md not listed in ..\\HISTORY.md (fails)
  - a missing top-level doc (fails)

Exit 0 if nothing structural is wrong, 1 if a doc is missing or unindexed.
A doc over its guide is not a failure: the guide is a prompt to ask "is this
still one job?", not a hard limit. The "near ceiling" band was added 2026-10-30
because README.md 234/240, history\\multiplayer.md 1493/1500 and ROADMAP.md
1330/1330 all printed a plain OK until the last line, which is how HANDOFF.md
went over its guide unnoticed.
"""
import os
import re
import sys

import ge_paths

ROOT = ge_paths.ROOT
PROJECT_ROOT = os.path.dirname(ROOT)

# The sizes below are GUIDES, not limits. They exist to prompt the question "is
# this still ONE job?" - never to force deleting knowledge to fit a number.
# Raised 2026-10-24 because the project grew: the old numbers were making the
# agent compress real findings (e.g. the Go transition) just to stay green.
# Owner's call 2026-10-26: a guide may be revised when a doc's one job genuinely
# needs the room - raise the number here and add a dated line to README.md.
# ROADMAP 640 -> 660, same day: R49 (custom-shop barter) and R50 (Feso shops) are
# two new open task SPECS with their evidence; still one job, just bigger.
# Fourth pass 2026-10-26: ROADMAP 660 -> 680 (R50 closed for buy AND sell, so it now
# states what remains; R10 narrowed to n > 1 records) and HANDOFF 200 -> 210 (two new
# traps: the sell packet carries no coin, and most items are not Feso-sellable). Both
# are one job each; the extra lines are the Feso sale's evidence, not a second topic.
# Fifth pass 2026-10-27: ROADMAP 680 -> 690. R51 is a new open task (cmd 16 must be
# chunked at 191 records - the live server does, measured on a 201-character account)
# and its evidence does not fit in one line. Same one job: the open work.
# Travel pass 2026-10-27: HANDOFF 210 -> 220. The page now carries two live zones and
# their ports, the two-account-store trap, and the end-of-test state to undo - all of
# it current state, which is HANDOFF's one job. Nothing was split out to make room
# because each line is a fact a new session must not re-derive.
# Evening pass 2026-10-27: HANDOFF 220 -> 235. A second map is now LIVE (arrival and the
# team warp proven in game), so the headline, the doorway and two traps changed at once:
# a warp is three packets per character, and two costume NPCs differ only by caption.
# Still one job - current state - and the proven line is what a new session reads first.
# Late-evening pass 2026-10-27: HANDOFF 235 -> 255. A pktmon capture of the LIVE server replaced
# the warp's shape (three stops, reverse order, leader last, one tick), which turned one trap into
# two and added the per-slot cell rule; and a NEW OPEN BLOCKER had to go first in the next steps
# (the client never answers 3686), which pushed every later item down. Still one job: a new
# session's first page. Nothing was split out to make room.
# 2026-10-28, later: ROADMAP 710 -> 720. R57's travel half and R58 went PROVEN IN GAME in the
# same session, so their proof numbers had to be recorded, and a NEW item R59 opened for the two
# tax requests the client sends at login. Nothing was cut to make room.
# 2026-10-28, evening: ROADMAP 720 -> 760. R60 opened for equip/unequip from the bag, and it has
# to carry four packet layouts, a refusal rule, the minted test kit and four open items on its own
# row. Nothing was cut to make room.
# 2026-10-28, night: ROADMAP 760 -> 780. R32c appended under R32 for the other player's walk: the
# stop it now sends, the +13 value the live server really sends, and the one thing still unproven
# (the T-pose, and the stance a worn weapon calls for). Detail stays in history\multiplayer.md.
# Nothing was cut to make room.
# 2026-10-28, night, later: ROADMAP 780 -> 790. R32d appended under R32 for the two holes behind
# R32c - the stop rule was unreachable (no read deadline, so no tick) and the idle is a separate
# ZC_CHANGE_STANCE 3554 the live server always pushes - plus what is still open (the weapon-matched
# stance). R32c's "Left:" line became a pointer to R32d, so nothing was cut; the entry needed the
# room. Detail stays in history\multiplayer.md.
# 2026-10-28, latest: ROADMAP 790 -> 815. R61 and R61b appended after R60: the Go zone now sends the
# six base stats (the offset/width/order proof, the oracle fixture and what is left), and R61b states
# the item/stance columns still missing before the rest of the sheet can be right. Two new task rows,
# so nothing was cut; detail stays in history\gameplay.md.
# 2026-10-28, later again: ROADMAP 815 -> 830. R61b was REWRITTEN, not added: the shipped client proved
# the rest of the sheet arrives in ZC_UPDATE_ALL_STATUS 3684 (125 B, BATTLE_STAT 36 not 34), so that
# bullet now carries the carrier, the layout and the three missing input tables instead of an open
# question. Nothing was cut; the proof lives in history\zone-entry.md and history\gameplay.md.
# 2026-10-28, latest: ROADMAP 830 -> 850. R32f appended under R32: the stance chooser is now
# weapon-aware (the condition table's weapon TYPE read from the item_weapon ClassType column), the owed
# ZC_CHANGE_STANCE 3554 is sent after a stop instead of never, and the per-character spawn line makes
# the choice readable in the log. One new task row; R32d's "Left:" became a pointer to it, so nothing
# was cut. Detail stays in history\multiplayer.md.
# 2026-10-28, latest (R32h): ROADMAP 850 -> 890. R32h appended under R32 - the live spawn's tail is
# EQUIP_ITEM_OPTION records (1017/1101 bytes) and ZC_SKILL_DISABLE 3572 is the spawn's partner, sent
# immediately after it - and R62 opened for the one open data question R32h turned up (character 40's sword
# class: 7203 in PostgreSQL against 7201 in the frozen barrack oracle). Two new task rows; R32d's "3554 pair"
# sentence was CORRECTED in place, not shortened, and nothing was cut. Detail: history\multiplayer.md.
# 2026-10-29, latest (R32i): ROADMAP 890 -> 940. R32i appended under R32 - the armed remote character
# T-posed because the spawn's +11 is a CO-ROUTINE SKILL INDEX, not a weapon id, and we were writing the
# item's class id into it; the row carries the offsets, the A/B log lines and the three unproven items.
# R32's parenthetical was CORRECTED in place ("+11 carries the weapon class" is false) and nothing was
# cut. Still one job: the open work. Detail: history\multiplayer.md, "CORRECTION 2026-10-29".
# 2026-10-29, evening (R65 / R66 / R67): ROADMAP 940 -> 970. Three task rows and one parked item were
# added, not rewrites: R65 (the 3684 status burst was answering CZ_START_GAME, where the client's handler
# drops every packet on a record-count gate still at 0 - the row carries the live capture, the 123-vs-125
# tool trap and the placement test), R66 (CSPD follows the stance, and "Cast Time" is client-computed, with
# the 400 floor named as the one open number), R67 (the four saved-teleport ids, the 3642 layout and the
# new table) and the PARKED jumpy-stop item in the owner's own words. R61 and R64 were REWRITTEN to their
# end state (R64 from 15 lines to 18 including the parked item) and R61 COMPRESSED, so nothing was cut.
# Still one job: the open work. Detail: history\gameplay.md, history\zone-town-npcs.md, history\movement.md.
# 2026-10-29, late night (R68 / R69 / R70 / R71 + THE INCIDENT): ROADMAP 970 -> 1060. Four task rows and one
# parked item were ADDED: R68 (the Save as Start Point button 3468 -> 3467, and why 3468 names no point),
# R69 (the Last Point - no client request exists, three triggers and one writer, plus ZC_LOGOUT_OK 3784 and
# the measured 10 s close), R70 (the storage split taken from the SQL oracle, the three facts and the
# Coimbra-overwritten bug), R71 (the barrack rollback and what it costs), THE 21:31 INCIDENT as its own row
# (the barrack received nothing, PKS_BC_CHANNEL_LIST's per-destination counts, the unchecked ->name, and the
# open map-numbering read) and the PARKED mission button in the owner's words. R65 / R65b / R66 / R67 keep
# their ids and their text; nothing was cut. Still one job: the open work. Detail:
# history\barrack-entry-points.md, history\barrack.md, history\npc-shops.md.
# 2026-10-29, 22:13 (R72): ROADMAP 1060 -> 1090. ONE task row was added for work that landed AFTER the pass
# above had started writing: R72, the two maps going back on the wire WITH a channel list that carries both.
# R68 / R69 / R70 / R71 and THE INCIDENT row were NOT rewritten - they are correct and green, and the new row
# sits between R71 and the incident. Documenting a deployed fix is not a second job: it is the same row set,
# so the number went up rather than a fact being cut. Detail: history\barrack-entry-points.md, "R72".
#
# 2026-10-30 (R74): ROADMAP 1090 -> 1110. ONE task row changed STATE, no new lane: R74 closes as a
# DEPLOYMENT gap (four client .tok meshes now deployed in navmesh\mesh\, the startup warning split into
# tool-missing vs mesh-missing, one test proving height -1088 twice, and the chase on map 15 measured at 600
# plans with 0 refusals). The row keeps the 2026-10-29 refusal history that found the bug - a fact that
# explains WHY the bug was visible - and adds what closed it, so it grew by 11 lines rather than a finding
# being cut. Detail: history\barrack-entry-points.md, "R74"; the settled call is DECISIONS.md 2026-10-30.
#
# 2026-10-30, later (R76 slice 2): ROADMAP 1110 -> 1190. Again ONE task row changed STATE, no new lane: Save,
# Rename and Delete are now SEEN IN GAME, the save-order fix (display_order, 00011) and 3639's MOVE are built
# and unit-tested but NOT yet seen in a running client, and 3637 is still open. R76 keeps the failed-memo-save
# history that found the bug and gains the new state, so the number went up rather than a finding being cut.
# Detail: history\zone-town-npcs.md, "R76 slice 2 landed"; the settled calls are DECISIONS.md 2026-10-30.
#
# 2026-10-30, latest (R76 slice 3): ROADMAP 1190 -> 1210. Again ONE task row changed STATE, no new lane: the
# basic Warp List holds TEN (a flat server number, since the original reached 10 through the "Warp Extend"
# cash item and this rebuild has no cash system), the entry burst states that cap in exactly one 46-byte 3748
# `Lock_Warp`, and a stale `commander_property` row never wins. The row keeps slices 1 and 2 intact and marks
# their "5" as superseded, carries the measured correction (the `warplist` window has no counter; the
# used/total text is the separate `teleportation` window and its total is the Teleportation skill level;
# `ge.exe` has no `Lock_Warp` string) and the types-1-3-stay-empty rule, so no finding was cut. Detail:
# history\zone-town-npcs.md, "R76 slice 3 landed"; the settled call is DECISIONS.md 2026-10-30.
#
# 2026-10-30, later still (R77): ROADMAP 1210 -> 1330. ONE new task row - the owner's call on whether a give
# that overflows the held stack should split the remainder into a second row or keep refusing (the rule lives
# in zone\items.go giveItem AND script\tx.go checkItemOp, and the refusal matches the shipped server). The
# guide had ALREADY fallen 100 lines behind the file from the day's slice rows, so this raise covers that too;
# no row was cut and no finding removed. Detail: history\database.md, "A give with Merge ADDS to the held
# stack".
#
# 2026-10-30, doc-headroom pass (two new files + one guide raise + one warning band). The pass SPLIT
# history\zone-town-npcs.md (1894 lines) into itself, history\town-travel.md and history\warp-list.md, and
# moved fourteen CLOSED section-1 rows out of ROADMAP.md verbatim into history\done-milestones.md, which
# took ROADMAP 1330 -> 1147 lines. Nothing was cut to fit: every section moved byte for byte and every id
# kept its exact status. HANDOFF 255 -> 300 on the owner's ruling - HANDOFF's one job is "what is true right
# now", it must never be trimmed, and the docs are optimised for the agent that reads them, not for the
# size guide. The 90% "near ceiling" band below is new for the same reason: a doc that is nearly full used
# to report a plain OK until the last line.
#
# 2026-11-02 (R110-R113b): ROADMAP 1330 -> 1500. The movement rebuild is FOUR task rows (R110 the route, R111 one
# clock per leg, R112 a finished leg stops deciding, R113 the client's own legs are routes) plus two attached rows
# for the pieces that came with them (R112b one planner process, R113b arrival by distance). Each row carries its
# own measured defect and the test names, so no row could be shortened without losing a fact. The file was ALREADY
# over the guide (1443) before this pass, from the day's earlier rows; this raise covers both and nothing was cut.
# Detail: history\movement.md, "R110-R113"; the settled call is DECISIONS.md 2026-11-02.
#
# 2026-11-03 (R114-R117): ROADMAP 1500 -> 1600. The move-cancel pass is FOUR task rows (R114 a move ends the
# engagement of the slot it names, R115 one walk per stand point, R116 the due swing before the move echo and the
# process-relative tick, R117 the OPEN finding - the swing that must not fire after a move). Each row has to carry
# its own measured defect, the log lines and the test names, and R117 has to state what would ACCEPT a fix so it is
# never claimed early. The file was at 1488 of 1500 before the pass, so this raise covers both and no row was cut.
# Detail: history\combat-attack-heal.md, "The move that ends a fight". RESUME.md gained five standing facts and is
# at 259 of its 350 guide, so it did not move. Same pass: README.md 240 -> 250, because the paragraph that records
# every raise had reached the last line of its own guide and needed this raise written into it.
#
# 2026-11-03 (Lua status instrument, R126): ROADMAP 1600 -> 1640. One new task row for the Character Info sheet
# being served by the game's own status script: the INSTRUMENT is built and deployed (item property export, the 16
# bindings, the shadow line) and the SWITCH is not started. The row has to carry the measured 14/15 line, the
# ATK-first order of the switch, and the THREE open gaps by name (max SP 695 vs 995 with no promotion level in a
# 3684, the two corpus-absent helpers, RPSY_BM / E_PsychicSnake) so none is "fixed" on the way - and the file was
# already at 1606 of 1600 from the day's rows, so this raise covers both. Nothing was cut. Same pass: README.md
# stays at 250 (a four-line dated raise was written into its paragraph of raises, 245 -> 249), and
# history\script-engine.md gained one section and is at 989 of its 1500 guide, so it did not move.
# Detail: history\script-engine.md, "The Character Info sheet runs the game's own script now".
#
# 2026-11-04 (HANDOFF, the rank line): HANDOFF 300 -> 320. The same session's live-login findings (the shadow on a real
# team, the two visible diffs, the rank line that was a never-sent 3093 packet, the deploy times and the next move) do
# not fit in 300 lines beside what was already there, and HANDOFF is never trimmed - its one job is what is true right
# now. Nothing was cut. Detail: history\zone-entry.md, "The rank line under a party card's name".
#
# 2026-11-04 (the switch, R126): HANDOFF 320 -> 340. The switch entry (what replaced the Go rules, the growing-item
# bug it exposed, the deployed log line and what his next login should show) is a handoff-defining fact and HANDOFF is
# never trimmed - its one job is what is true right now. Nothing was cut.
# Detail: history\script-engine.md, "The switch: the sheet is the game's script now".
#
# Going over a guide prints WARNING and does not fail the script.
#
# 2026-11-04 (the rank line, R128): ROADMAP 1640 -> 1660. A new task row for the promotion rank that the party card
# was drawing BLANK - the character sheet (3684) has no promotion field and START_INFO has none either, so the rank
# only travels in ZC_PROMOTION_INFO (3093), which this server never sent. The row has to carry the packet's measured
# layout, the reference's send site and the appearance-byte question that is still open; the file was already at
# 1641 of 1640 after the same session's R126 notes, so this raise covers both. Nothing was cut.
# Detail: history\zone-entry.md, "The rank line under a party card's name".
#
# 2026-11-04 (R131, and the raise log itself): README 265 -> 275 as well, because this paragraph of raises reached
# its own guide again - the same precedent as 240 -> 250 and 250 -> 265. Then: ROADMAP 1695 -> 1710 and
# HANDOFF 365 -> 380. His rule ("each of
# the character to be independent ... changing leader should not affect anything") turned one session's complaint
# into a structural fix: a per-character walk record, a tick that steps each of them, and a switch that touches
# only the control slot. The row carries the 17:12:15 measurement and the reference's own OnSelectChangePC.
# 2026-11-04 (the movement area CLOSED): HANDOFF 390 -> 405. His closing word on the whole movement/combat/NPC
# interaction area is exactly what a new session must read first, together with the list of what closed and
# the one thing this pass fixed on the way out (`wantsTick` counting a team walk). Nothing was cut.
#
# 2026-11-04 (R132 - the ring and the stop-on-fire): ROADMAP 1710 -> 1725 and HANDOFF 380 -> 390. Two of his own
# observations in one row: the 10% stand-off margin was our patch for the path engine's error (the pull-in
# already answers it, so the ring is the reference's own maxR + targetR), and the fire-on-arrival left the body
# walking to the ring while swinging (17:50:17: first hit at (1442,-821), second 138 units further on). Nothing
# was cut. Detail: history\attack-position.md, "The ring is the stance's, and the first hit stops the walk".
#
# 2026-11-04 (`-splock`, the SP lock he asked for): HANDOFF 405 -> 420, and `history\combat.md` gets its own
# guide (1500 -> 1700). HANDOFF is never trimmed - its one job is what is true right now - and the deploy (the
# flag, what it does to the 3654, the restart, the session it dropped) is what a new session must read first.
# combat.md was ALREADY at 1638 of its 1500 guide before this pass; §10.9a is the SP rule as this server carries
# it, the switch, and his stance's own costs - the measured content that file exists for - so the guide is raised
# rather than those facts split away from the cast oracle they belong to. Nothing was cut.
# Detail: progress\2026-11.md, 2026-11-04 19:08.
#
# Nothing was cut. Detail: history\movement.md, "Each character owns its walk (R131)".
#
# 2026-11-04 (R130 - the stopping point must be ground): ROADMAP 1675 -> 1695 and HANDOFF 355 -> 365. His question
# after R129 ("i dont want the character to suddenly get stuck due to collision") produced a real guard: the
# step-back stand point is now checked against the map's own mesh, the move-destination line exists, and the
# reference's own answer (GetOuterPosByTarget + IsCollision) is written down with file:line. Nothing was cut.
# Detail: history\movement.md, "What actually stops a bad position".
#
# 2026-11-04 (R129 built, gated and deployed): HANDOFF 340 -> 355. The fix, its two tests, the log line and the
# restart reached a file that is never trimmed, and this block is what a new session must read first - the
# rule it replaced is exactly the "press mid-walk jumps to the destination" the owner reported. Nothing was
# cut. Detail: history\attack-position.md, "A press mid-walk must fight where the body stands".
#
# 2026-11-04 (the owner's correction to R123): ROADMAP 1660 -> 1675. He questioned the row's premise - "a move will
# cancel keepmode though. a click to move and not the AI own chase" - and re-decoding captures\keepfast* with
# src\decode_zone_capture.py showed the row conflates two moves that arrive as the SAME packet (CZ_MOVE_PC): a
# player's click and the character's own AI step. So the row can no longer claim "the reference cancels on distance";
# it now carries the objection, the counters (2/18 CZ_ATTACK presses for 94/347 damages) and the one in-game test that
# settles it. Nothing was cut.
# Detail: history\combat-attack-heal.md, "A click cancels, an AI step does not".
#
# Also note: a line count is a weak proxy. What matters is ONE JOB PER DOC and
# that the next session can find a fact. Prefer a long, well-structured doc over
# a short one with facts missing.
#
# 2026-11-09: two raises were recorded ONLY in README.md's own copy of this log, and that copy is now a
# pointer to this file, so they are consolidated here: 2026-10-27, ROADMAP 600 -> 640 (R17's VPS target and
# its five axes); 2026-10-28, ROADMAP 690 -> 710 (R57 opened for the Waypoint structure's town-teleport
# window - the window itself landed, travelling did not, so the row had to be readable on its own).
CEILINGS = {
    'RESUME.md': 350,
    'ROADMAP.md': 1980,  # 2026-11-09 (the size-guide pass): 1910 -> 1980. The fourth batch moved FOURTEEN
                         # terminal rows (R64, R67, R71, R122, R123, R125, R126, R127, R128, R129, R130, R131,
                         # R132, R133) VERBATIM into `history\done-milestones.md` - a ROADMAP row is an OPEN
                         # task, not a record - taking the file 1909 -> 1759. The remaining mass is 66 OPEN
                         # rows carrying their own measured briefs (R76 alone is 209 lines) plus the
                         # fourth-batch pointer paragraph, so the guide moved too; 1759 of 1980 is 88.8%. Nothing was cut. 2026-11-07 (evening): 1900 -> 1910 - the `-respawn <duration>` DEV flag (`23fa8f58`)
                         # needed its housekeeping row beside `-dummycell`, naming what is live (:7002, the four-body
                         # command line) and what is still owed (the owner's own look). Nothing was cut.
                         # 2026-11-07 (second raise): 1850 -> 1900. R139 (a spawn's HP from SCR_Get_MON_MHP),
                         # R140 (respawn slice 1) and R98's drops slice 1 each needed a row or a state change; R98's
                         # row now names the PICKUP and the two group rules it still owes. Nothing was cut.
                         # Earlier the same day: 1780 -> 1850. The four overnight slices (R135e, R93, R137 slice 1,
                         # R138) each needed a task row or a state change, and R93's row carries the aimed
                         # 3557 story while R135 carries R135e's two open gaps. Nothing was cut.
    'INDEX.md': 460,     # 2026-11-11: 440 -> 460. One row was added for `archive\` (the client's crash
                         # dumps, kept as reference) and the `data\` row was rewritten to point at the new
                         # `data\README.md`; the file measured 441. Same reasoning as the 2026-11-09 raise:
                         # a lookup table grows with the tree and splitting it is how a row becomes
                         # unfindable. No row was cut or merged.
                         # 2026-11-09 (the size-guide pass): 360 -> 440. INDEX is a LOOKUP TABLE - one row per
                         # file or folder - so "is this still one job?" is YES by construction: an index grows
                         # with the tree, and splitting a table across two files is how a file becomes
                         # unfindable. At 385 lines it was over 360 and rising with every slice that lands; the
                         # guide is raised to the table's real size instead, and no row was cut or merged.
                         # 2026-11-07 (evening): 355 -> 360 - the row for `zone\respawn_override_test.go`, which
                         # pins the `-respawn` flag (2 s under it, 23.568 s without, nothing 1 ms early). Nothing cut.
                         # 2026-11-07: 320 -> 355 - rows for `sight_cull_test.go`, `action_broadcast_test.go`,
                         # `monster_swing.go` and `monster_swing_test.go` (the same four slices). Nothing was cut.
                         # 2026-11-06: 310 -> 320 - the knowledge-lane queue
                         # (research\QUESTIONS.md) and the lanes\ claims; 2026-11-05: 300 -> 310 - two rows for the load harness
                         # (`loadtest_seed.py`, `goserver\cmd\loadgen`). Nothing was cut.
    'HISTORY.md': 100,
    'GLOSSARY.md': 300,
    'OPERATIONS.md': 300,
    # 2026-11-07: HANDOFF.md is RETIRED (the owner's call) - the board is the queue
    # AND the handoff, and its export `QUEUE.md` is generated, so it has no guide.
    # The raise notes above that mention HANDOFF are the record of a page that is
    # no longer here; do not add a new entry for it.
}

ROOT_README_CEILING = 300  # 2026-11-07: 285 -> 300 - the paragraph of raises below grew by the four
                           # overnight slices' raise. 2026-11-05: 275 -> 285 - the
                           # ROADMAP/INDEX raise for the load-test harness. Nothing was cut.
HISTORY_CEILING = 1500
# combat.md has its own guide: it is the cast/combat wire oracle and keeps every measured layout in §8-§10, so its
# size is content, not sprawl (raised 1500 -> 1700 on 2026-11-04 with §10.9a, then 1700 -> 1740 the same day when the
# live R84 result table landed at 1715 - nothing was cut).
HIST_OVERRIDES = {'data-generation.md': 1500, 'combat.md': 2220,
                  # 2026-11-09 (the size-guide pass): combat.md 1800 -> 2220. It is ONE job - the cast/combat
                  # wire oracle - and §8-§10 plus the three 2026-11-08 monster sections (leash/home/lifetime,
                  # the kited body walking home, and the acquire radius being the AI row's SeekRange) are
                  # that same job measured through the same cast and aggro packets; `history\movement.md`
                  # owns the PLAYER's walk, so nothing in here is a second, movement topic. At 1982 of 1800
                  # the file was already over, and the alternative - splitting a wire oracle so a builder has
                  # to guess which half holds the layout - is the cost this guide exists to avoid. Nothing cut.
                  # 2026-11-07: combat.md 1740 -> 1800 - §10.12 (R138): the area cast's per-victim 3557 now reads
                  # the HP ledger. It is the cast/area wire oracle's own subject, the file was at 1740 of 1740
                  # before it, and nothing was cut.
                  # 2026-11-06 (R135): the sight ring added its own section to multiplayer.md - the per-state
                  # table, the map's ViewRange rows, the two RE-ENTER frames and the 50-bot numbers. The file was
                  # at 1493 of 1500 BEFORE it and the section is the same job (who sees whom), so the guide moved
                  # and no finding was cut. Dated line in README.md, same day.
                  # 2026-11-06, later (R135d): the ring now measures the BODY, not the destination, and
                  # its subsection (the Pos-is-a-destination trap, walkedRouteAt as the reader, the red proof
                  # and the four answers) went into the SAME one job. The file was at 1633 of 1640 before it,
                  # so the guide moved and no finding was cut. Dated line in README.md, same day.
                  # 2026-11-07 (R135e): the second radius - the update cull - joined the same section; the file
                  # was at 1719 of 1780, so 1780 -> 1800 keeps the one job and cuts nothing.
                  # 2026-11-09 (the size-guide pass): multiplayer.md 1800 -> 1980. Nothing was over: it stood
                  # at 1774 of 1800, i.e. 98%, and the near-ceiling band was flagging a doc that was still
                  # UNDER its guide. It is one job (who sees whom in one zone: the roster, the spawn, the
                  # sight ring and its two radii), R135e's 2026-11-07 section filled it, so the guide moves
                  # rather than 155 lines of measured findings being cut. Nothing was cut.
                  # 2026-11-11 (the night of the barrack login rule): 1980 -> 2100. Nothing was over: it stood
                  # at 1978 of 1980 (99%) because the barrack half of "one live login per account" needed a
                  # two-line pointer. The file is still one job (who sees whom in one zone: roster, spawn, the
                  # sight ring, one session per account) and its detail is all measured, so the guide moves and
                  # nothing is cut. A SPLIT is queued as its own card rather than done here at 02:40.
                  # 2026-11-11, later (card t_ce7d19c5): THE 2100 STOPGAP IS GONE - multiplayer.md is back on
                  # the default 1500, because 2100 was measuring THREE jobs in one file, not one big one. It
                  # was split VERBATIM into four files, one job each, and every one of them is under 1500:
                  #   history\multiplayer.md           who sees whom in one zone: the roster, the 3520 spawn
                  #                                    and its layout, the appearance and the worn equipment, the
                  #                                    walk on the other client, the client's own reader  (1242)
                  #   history\sight-ring.md            who is TOLD about whom: R135..R135e, the two radii, the
                  #                                    leave and the two re-enter forms, PcViewDist          (462)
                  #   history\multi-player-serving.md  several connections at once: the thread-per-connection
                  #                                    refactor, the golden test, the two-client setup        (273)
                  #   history\live-sessions.md         ONE live session per account and per character        (45)
                  # No finding was deleted, reworded or reordered; every cross-reference was repointed at the
                  # file that now owns the section. The three new files take the 1500 default - no override.
                  }


# A doc at 90% or more of its guide, but not over it, is "near ceiling": it gets
# a warning of its own. It does NOT fail anything and the exit code is unchanged.
NEAR_BAND = 90


def _flag(n, ceiling):
    """'over', 'near' or '' for a line count against a guide size."""
    if n > ceiling:
        return 'over'
    if ceiling and n * 100 >= ceiling * NEAR_BAND:
        return 'near'
    return ''


def _lines(path):
    try:
        with open(path, encoding='utf-8', errors='replace') as f:
            return sum(1 for _ in f)
    except FileNotFoundError:
        return None


def main():
    problems = []
    warnings = []
    near = []

    print('Project-root docs:')
    n = _lines(os.path.join(PROJECT_ROOT, 'README.md'))
    if n is None:
        print('  MISSING  README.md')
        problems.append('missing doc: README.md')
    else:
        kind = _flag(n, ROOT_README_CEILING)
        flag = ''
        if kind == 'over':
            flag = '  OVER'
            warnings.append(f'README.md is {n} lines (guide {ROOT_README_CEILING})')
        elif kind == 'near':
            flag = '  near ceiling (%d%%)' % (n * 100 // ROOT_README_CEILING)
            near.append(f'README.md is {n} lines, near ceiling (guide {ROOT_README_CEILING}, %d%%)'
                        % (n * 100 // ROOT_README_CEILING))
        print(f'  {n:4d}/{ROOT_README_CEILING:<4d} README.md (project root){flag}')

    print('\nTop-level docs (docker-port):')
    for name, ceiling in sorted(CEILINGS.items()):
        p = os.path.join(ROOT, name)
        n = _lines(p)
        if n is None:
            print(f'  MISSING  {name}')
            problems.append(f'missing doc: {name}')
            continue
        kind = _flag(n, ceiling)
        flag = ''
        if kind == 'over':
            flag = '  OVER'
            warnings.append(f'{name} is {n} lines (guide {ceiling})')
        elif kind == 'near':
            flag = '  near ceiling (%d%%)' % (n * 100 // ceiling)
            near.append(f'{name} is {n} lines, near ceiling (guide {ceiling}, %d%%)'
                        % (n * 100 // ceiling))
        print(f'  {n:4d}/{ceiling:<4d} {name}{flag}')

    hist_dir = os.path.join(ROOT, 'history')
    history_index = os.path.join(ROOT, 'HISTORY.md')
    index_text = open(history_index, encoding='utf-8', errors='replace').read() if os.path.exists(history_index) else ''

    print('\nHistory files:')
    if os.path.isdir(hist_dir):
        for name in sorted(os.listdir(hist_dir)):
            if not name.endswith('.md'):
                continue
            p = os.path.join(hist_dir, name)
            n = _lines(p)
            ceiling = HIST_OVERRIDES.get(name, HISTORY_CEILING)
            kind = _flag(n, ceiling)
            flag = ''
            if kind == 'over':
                flag = '  OVER'
                warnings.append(f'history\\{name} is {n} lines (guide {ceiling})')
            elif kind == 'near':
                flag = '  near ceiling (%d%%)' % (n * 100 // ceiling)
                near.append(f'history\\{name} is {n} lines, near ceiling (guide {ceiling}, %d%%)'
                            % (n * 100 // ceiling))
            indexed = name in index_text
            idx_flag = '' if indexed else '  NOT INDEXED'
            print(f'  {n:4d}/{ceiling:<4d} history\\{name}{flag}{idx_flag}')
            if not indexed:
                problems.append(f'history\\{name} is not listed in HISTORY.md')

    # ---------------------------------------------------------------- structure
    # Two checks a line count cannot see. Both are WARNINGS: a doc is a judgement
    # call, and the project's rule is that no checker deletes a true finding.
    #
    # 1. SAME TOPIC IN TWO FILES. Two `history\` files whose H1 subject starts
    #    with the same three words are one job wearing two names - that is how
    #    three zone-npc-click pages happened (merged 2026-11-11). The key drops
    #    the parenthetical, so "Zone NPC click-to-move (first pass)" and
    #    "... (live server analysis)" collide on purpose.
    # 2. A FILE THAT CALLS ITSELF AN INDEX. An index is a table of rows; once
    #    it carries hundreds of lines of narrative it is a history file lying
    #    about its job (`history\zone.md` was 666 lines titled "(index)").
    if os.path.isdir(hist_dir):
        topics, self_index = {}, []
        for name in sorted(os.listdir(hist_dir)):
            if not name.endswith('.md'):
                continue
            p = os.path.join(hist_dir, name)
            first = ''
            for line in open(p, encoding='utf-8', errors='replace'):
                if line.startswith('# '):
                    first = line[2:].strip()
                    break
            subject = re.sub(r'^HISTORY\s*-\s*', '', first, flags=re.I)
            subject = re.sub(r'\(.*?\)', ' ', subject)
            words = re.findall(r'[a-z0-9]+', subject.lower())
            if len(words) >= 3:
                topics.setdefault(' '.join(words[:3]), []).append(name)
            if 'index' in first.lower() and (_lines(p) or 0) > 400:
                self_index.append((name, _lines(p) or 0))
        for key, names in sorted(topics.items()):
            if len(names) > 1:
                warnings.append('SAME TOPIC in %d files (%r): %s - one job, one file?'
                                % (len(names), key, ', '.join(names)))
        for name, n in self_index:
            warnings.append('%s is titled an INDEX but is %d lines of narrative - retitle it '
                            'or move the detail to its owner' % (name, n))

    if near:
        print('\nWarnings (NEAR CEILING - 90% or more of the guide, not over it - not failures):')
        for w in near:
            print(f'  - {w}')
        print('  These are the docs that go over next. Split one, or raise its guide'
              ' in README.md + check_docs.py with a dated line.')

    if warnings:
        print('\nWarnings (guide sizes - not failures):')
        for w in warnings:
            print(f'  - {w}')
        print('  Ask: does this doc still have ONE job? Split it, or raise the guide'
              ' in README.md + check_docs.py with a dated line.')

    if problems:
        print('\nProblems:')
        for p in problems:
            print(f'  - {p}')
        return 1
    print('\nDocs OK.' if not (warnings or near) else '\nDocs OK (with guide warnings).')
    return 0


if __name__ == '__main__':
    sys.exit(main())
