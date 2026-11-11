#!/bin/sh
# build.sh - build BOTH targets from this one tree: the Windows binary the owner
# plays with and the Linux binary the VPS will run. There is no cgo and no
# runtime.GOOS branch, so one source builds both; if that ever stops being true
# this script is where it shows.
#
#   sh build.sh
#
# The binaries land in ../out/ under distinct names ON PURPOSE: ../out/goserver.exe
# is the owner's LIVE barrack binary, and overwriting a running .exe fails on
# Windows. Copy/rename when deploying.
set -eu
cd "$(dirname "$0")"
mkdir -p ../out

echo "building windows/amd64..."
GOOS=windows GOARCH=amd64 go build -o ../out/goserver_win_amd64.exe .

echo "building linux/amd64..."
GOOS=linux GOARCH=amd64 go build -o ../out/goserver_linux_amd64 .

# The zone manager (cmd/zoneman) is the on-demand starter: it watches the registry for
# request_<map>.json and brings a map up, so a door never has to wait for the operator.
echo "building zoneman..."
GOOS=windows GOARCH=amd64 go build -o ../out/goserver_zoneman.exe ./cmd/zoneman

# The account tool: one account on the PostgreSQL stack, because nothing else writes
# account_info and a fresh deployment could not log in without it.
echo "building mkaccount..."
GOOS=windows GOARCH=amd64 go build -o ../out/goserver_mkaccount.exe ./cmd/mkaccount

# Seed the deployed config WITHOUT overwriting an operator's edit: server.json
# beside the binary is the file a no-flag start reads.
if [ ! -f ../out/server.json ]; then
    cp server.json ../out/server.json
fi

echo "done:"
ls -l ../out/goserver_win_amd64.exe ../out/goserver_linux_amd64 ../out/server.json
