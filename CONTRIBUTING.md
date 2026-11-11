# Contributing

The value of this project is in **measurements that can be checked**. A small, proved change is
worth more than a large plausible one.

## What helps most

1. **A measurement with its evidence.** A packet layout, a table row, a behaviour - with the
   method named: the client build, a recorded capture, a log line, a byte-for-byte comparison.
2. **A test that fails without the fix.** A change is not done until a test proves it was needed:
   break the code again, watch the test go red, restore it, watch it go green.
3. **A tool that runs anywhere.** A tool that takes its paths as arguments and works on someone
   else's machine is a real contribution; one that assumes a particular disk is not.
4. **A correction.** If the code and a description disagree, that is a bug in one of them and an
   easy change to accept.

## What does not help

- A change carrying game files, client archives, databases or captured traffic. **Do not send
  those.** See [docs/LEGAL.md](docs/LEGAL.md).
- "It works on my machine" without the log line, the capture, or the test that shows it.
- A second file for a subject that already has one. Extend what exists.

## Where a finding belongs

There is no separate research folder in this repository on purpose: a finding lives where it is
used.

| You have | Put it in |
|---|---|
| a behaviour, a byte layout, a rule | the comment above the code that implements it, naming the evidence - a source file and line, a capture, a timestamped log line |
| proof that a bug existed | the test, in its name and its failure message |
| a machine-specific assumption | a change that takes the value as an argument, or a line in the tool's help text saying what it needs |

A comment that names its evidence is worth more than a page nobody updates.

## Before you send

Build, vet and test everything you touched; the exact commands are in
[docs/DEVELOPING.md](docs/DEVELOPING.md). Format Go code with `gofmt`.

## Style

- Small packages, one program per folder, no clever tricks where a plain loop reads better.
- A comment explains **why** and cites the evidence. A comment that restates the code is noise.
- **Never commit a secret.** Passwords and connection strings belong in the environment, not in a
  file that gets committed.
- Keep the diff about one thing. A refactor mixed with a fix cannot be reviewed.
