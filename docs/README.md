# ge_server docs

Four pages, one job each. Read them in this order if you are new.

| Page | What it answers |
|---|---|
| [DEVELOPING](DEVELOPING.md) | What you need installed, how to build it, how to run it, how to test it |
| [ARCHITECTURE](ARCHITECTURE.md) | What the three programs are, how they talk, and the map of the Go packages |
| [DATA](DATA.md) | Why the server needs the game's own tables, how they are produced, and what you bring |
| [CLIENT-VERSIONS](CLIENT-VERSIONS.md) | **This tree is pinned to one client build** - what that locks, and how to adapt it to yours |
| [LEGAL](LEGAL.md) | No game files are included, and what that means for you |

## The shortest possible orientation

```
your client  ->  barrack :7001  ->  zone manager :7010  ->  zone :7010+  ->  PostgreSQL
```

- `docker-port\goserver\` - the server (Go). Start at `cmd\`: one folder, one program.
- `docker-port\src\` - the Python tools. They read **your** client and produce the tables; they
  do not serve.
- `docker-port\deploy\` - the Docker image and the deployment guide.

## The discipline that makes the code readable

A behaviour is only "known" when it is measured - a capture, a log line, a binary, a database
row, or a byte-for-byte comparison - and the code comment says which. When something is a guess,
it is marked as one. That is why comments in `zone\` cite a source file and a line number instead
of asserting a rule.

If you change a behaviour, change the comment with it: a stale explanation is the most expensive
kind of debt in a project like this.
