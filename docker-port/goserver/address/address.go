// Package address answers ONE question for every role of this server: which ip
// should we tell the game client to dial?
//
// It is not compiled in any more. The original (v28) server kept an address book
// in its RESOURCE database - MACHINE (MACHINE_ID, GROUP_ID, MACHINE_NAME,
// MACHINE_IP), beside PVP_MACHINE and SERVER_INFO - so a machine could be moved
// or added without a rebuild (docker-port\history\shipped-binaries.md:364-379).
// This package is that idea, one row: `machine` in PostgreSQL
// (db\migrations\00013_machine.sql), read once at startup by the barrack and by
// every zone.
//
// THE ORDER OF PRECEDENCE, and nothing else may answer:
//
//  1. the -advertise flag - a test or a one-off run; it WINS over the row;
//  2. the machine row's ip - what the owner asked for: configurable;
//  3. DefaultIP - the documented default, and the only place in this tree where
//     a loopback address literal lives.
//
// A missing row, an unreachable database or a blank ip is NOT fatal: the address
// falls back to DefaultIP and Resolve logs exactly ONE line saying which value
// was used and why. A server that cannot read its config must still serve.
//
// WHAT THIS PACKAGE DOES NOT ANSWER: a PORT. A town's port is the one its own
// listener BOUND, so the zone reports the port it opened and no row can
// contradict it.
package address

import (
	"context"
	"fmt"
	"strings"
)

// DefaultIP is the ONE compiled-in address in this tree, and it is reached only
// when nothing else answers: no flag, no store, no row, no ip. Every other place
// that must guess an address goes through Fill, so a second literal cannot grow
// back into the code.
const DefaultIP = "127.0.0.1"

// DefaultMachine is the address-book row the servers read when -machine is not
// given. The migration seeds it with the address this project ran on before the
// address book existed.
const DefaultMachine = "local"

// Source says where the address came from, so a startup log can name it.
type Source string

const (
	// FromFlag is the -advertise value: it beats the database on purpose.
	FromFlag Source = "the -advertise flag"
	// FromStore is the machine row in PostgreSQL.
	FromStore Source = "the machine row in PostgreSQL"
	// FromDefault is DefaultIP, used because nothing else answered.
	FromDefault Source = "the built-in default"
)

// Fill returns ip, or DefaultIP when ip is blank. It is the single conversion
// from "we have no address" to the documented default: the registry's empty-ip
// rule and this package's own fallback both call it, so they cannot drift.
func Fill(ip string) string {
	if v := strings.TrimSpace(ip); v != "" {
		return v
	}
	return DefaultIP
}

// Store is the address book. It is an interface so the resolution rules can be
// tested without a database, and so a future source (a config file, the zone
// registry) can answer without touching a caller.
type Store interface {
	MachineIP(ctx context.Context, machine string) (string, error)
}

// StoreFunc adapts a plain function to Store, so a caller can wire db.MachineIP
// in one line and a test can answer with a constant.
type StoreFunc func(ctx context.Context, machine string) (string, error)

// MachineIP implements Store.
func (f StoreFunc) MachineIP(ctx context.Context, machine string) (string, error) {
	return f(ctx, machine)
}

// Config is everything Resolve is allowed to look at. Every field has a safe
// zero value: an empty Config resolves to DefaultIP.
type Config struct {
	// Flag is the -advertise value. Empty means "not given".
	Flag string
	// Machine is the address-book row to read. Empty means DefaultMachine.
	Machine string
	// Store reads the row. Nil means "no address book is wired in", which is a
	// documented fallback, not a failure.
	Store Store
	// Log receives the ONE resolution line. Nil logs nothing.
	Log func(format string, args ...any)
}

// Answer is the resolved address plus the story of how it was chosen.
type Answer struct {
	// IP is never empty: it is the flag, the row, or DefaultIP.
	IP string
	// Source is which of those three answered.
	Source Source
	// Detail is the rest of the log line: why THIS source answered, or why the
	// one above it could not.
	Detail string
}

// String is the line that is logged, in a shape the owner can read.
func (a Answer) String() string {
	return fmt.Sprintf("advertise: %s (from %s) - %s", a.IP, a.Source, a.Detail)
}

// Resolve picks the address AND logs exactly one line saying which value was
// used and where it came from. Call it once per process, at startup, and keep the
// Answer: a running role keeps its startup state, so nothing re-reads the row.
func Resolve(ctx context.Context, c Config) Answer {
	a := resolve(ctx, c)
	if c.Log != nil {
		c.Log("%s", a.String())
	}
	return a
}

// resolve is the rules without the logging, so the logging above happens exactly
// once whatever the branch.
func resolve(ctx context.Context, c Config) Answer {
	machine := strings.TrimSpace(c.Machine)
	if machine == "" {
		machine = DefaultMachine
	}

	// 1. THE FLAG WINS. A test or a one-off run must be able to point the client
	// somewhere without editing the database, and a value that silently lost to a
	// stored row would be a trap.
	if flagIP := strings.TrimSpace(c.Flag); flagIP != "" {
		return Answer{IP: flagIP, Source: FromFlag,
			Detail: fmt.Sprintf("it overrides machine row %q", machine)}
	}

	// 2. THE DATABASE. Nil is "no address book wired in", which is what a test or
	// an older caller has; it is a fallback, not an error.
	if c.Store == nil {
		return Answer{IP: DefaultIP, Source: FromDefault,
			Detail: "no address book is wired in"}
	}
	ip, err := c.Store.MachineIP(ctx, machine)
	if err != nil {
		return Answer{IP: DefaultIP, Source: FromDefault,
			Detail: fmt.Sprintf("machine row %q could not be read: %v", machine, err)}
	}
	if strings.TrimSpace(ip) == "" {
		return Answer{IP: DefaultIP, Source: FromDefault,
			Detail: fmt.Sprintf("machine row %q has an empty ip", machine)}
	}

	return Answer{IP: strings.TrimSpace(ip), Source: FromStore,
		Detail: fmt.Sprintf("machine row %q", machine)}
}
