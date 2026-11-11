// Command mkaccount creates ONE account on the PostgreSQL stack: the login row the
// barrack reads at CB_LOGIN, and the password row beside it.
//
//	mkaccount -db "postgres://..." <member_id>
//
// WHY A PROGRAM AND NOT A SQL SNIPPET: a fresh deployment cannot log in at all - the
// barrack only READS account_info, nothing on this stack writes it, and the old
// src\make_account.py writes through the ORIGINAL SQL Server connection. This is the
// smallest tool that makes "rent a box, bring it up, make an account, log in" true, and
// it lives INSIDE the image so a deployment never needs Python or the repo.
//
// WHAT IT DOES NOT DO: it does not create a family (commander) or a character. The
// CLIENT does both - CB_CREATE_COMMANDER (11) and CB_CREATE_CHAR (18) in the barrack's
// own command table - so an account made here logs in to an EMPTY barrack, which is the
// screen where the player types their family name. A tool that pre-made a family would
// skip the game's own flow (and disagree with it the day the barrack learns cmd 11).
//
// THE PASSWORD ROW IS MD5 HEX, and that is PROVEN now: the original engine keeps it in
// `RESOURCE.ACCOUNT_INFO.PASS`, and 504 of its 507 rows are exactly 32 lowercase hex
// characters - `e10adc3949ba59abbe56e057f20f883e` among them, which is md5("123456").
// No salt, no prefix, no digest of anything but the plaintext. `-password` writes that
// md5; without it the row gets '-' (no password set), which is what the load-test bots
// carry.
//
// What still does NOT happen is VERIFICATION: the barrack's login does not compare the
// client's answer to this column yet ("PASSWORD NOT VERIFIED: the client's digest cannot
// be checked"), and the launcher's own password is a local check in accounts.ini. So the
// row is correct data waiting for the check, not a lock.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"geserver/account"
	"geserver/db"
)

func main() {
	dbDSN := flag.String("db", db.DSN(),
		"PostgreSQL DSN (empty = the environment, then the local default)")
	password := flag.String("password", "",
		"the login password; stored as MD5 hex, the original engine's own scheme. "+
			"Empty = no password row value ('-'), which nothing verifies yet")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mkaccount [-db DSN] [-password PW] <member_id>\n\n"+
			"Creates one account (account_info + account_pass) with commander_id 0: the\n"+
			"player creates the family in the game's barrack, where the client asks for it.\n"+
			"<member_id> is what the client sends as its account name - the launcher's\n"+
			"accounts.ini left-hand id. -password is stored as MD5 hex (the original\n"+
			"engine's scheme); without it the row is '-'.\n")
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	member := flag.Arg(0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, *dbDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkaccount: no database: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	// A BRAND-NEW deployment has no tables; the manager migrates on boot too, but this
	// tool must also work against a database that has never run the server.
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "mkaccount: migrations failed: %v\n", err)
		os.Exit(1)
	}

	// The id's own rules live in geserver/account, so the console's registration page and
	// this command cannot drift apart.
	id, err := account.Create(ctx, pool, member, *password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkaccount: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created account %q: ACCOUNT_ID=%d COMMANDER_ID=0 (the family is created in the game)\n", member, id)
	if *password == "" {
		fmt.Printf("password: none set ('-'). Nothing verifies it yet.\n")
	} else {
		fmt.Printf("password: stored as MD5 hex (%s), the original engine's scheme. NOT verified at login yet.\n", account.PassMD5(*password))
	}
	fmt.Printf("log in with the account name %q\n", member)
}
