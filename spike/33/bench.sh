#!/usr/bin/env bash
# Spike for #33: startup cost of exe size and of the gocached toolexec wrapper.
# Run from the repo root in bash (Git Bash on Windows).
set -euo pipefail
B=$(mktemp -d); X=""
case "$(go env GOOS)" in windows) X=.exe; B=$(cygpath -m "$B") ;; esac
go build -o "$B/drive$X" ./spike/33/drive
go build -o "$B/nop$X" ./spike/33/nop
go build -trimpath -ldflags="-s -w" -o "$B/mini$X" ./spike/33/mini
go build -trimpath -ldflags="-s -w" -o "$B/txonly$X" ./spike/33/txonly
go build -trimpath -ldflags="-s -w" -o "$B/gocached$X" .          # as released
for mb in 4 12; do cp "$B/nop$X" "$B/nop$mb$X"; head -c $((mb*1000000)) /dev/zero >> "$B/nop$mb$X"; done
T=$(go env GOTOOLDIR); cp "$T/compile$X" "$B/compile$X"
cp "$B/compile$X" "$B/compile12$X"; head -c 12000000 /dev/zero >> "$B/compile12$X"
cd "$B"; ls -l
echo "go: $(go version)  os: $(uname -a)"
"./drive$X" 7 40 \
  "nop=./nop$X" "nop+4MB=./nop4$X" "nop+12MB=./nop12$X" \
  "compile=./compile$X|-V=full" "compile+12MB=./compile12$X|-V=full" \
  "mini nop=./mini$X|./nop$X" "txonly nop=./txonly$X|./nop$X" "gocached toolexec nop=./gocached$X|toolexec|./nop$X" \
  "mini compile=./mini$X|./compile$X|-V=full" "gocached toolexec compile=./gocached$X|toolexec|./compile$X|-V=full"

# Cold build of this repo: fresh GOCACHE each run, modes interleaved, run 0 discarded.
cd "$OLDPWD"
declare -A R
for i in 0 1 2 3; do
  for m in plain mini txonly gocached; do
    case $m in
      plain) f="" ;; mini) f="-toolexec=$B/mini$X" ;; txonly) f="-toolexec=$B/txonly$X" ;;
      gocached) f="'-toolexec=$B/gocached$X toolexec'" ;;
    esac
    rm -rf "$B/cache"; t=$(date +%s%N)
    GOCACHE="$B/cache" GOFLAGS="$f" go build ./...
    [ $i = 0 ] || R[$m]+="$(( ($(date +%s%N) - t) / 1000000 )) "
  done
done
for m in plain mini txonly gocached; do echo "cold build $m: ${R[$m]}ms"; done
