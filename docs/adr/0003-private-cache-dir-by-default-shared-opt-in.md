# Cache dir is private by default; sharing the host GOCACHE is opt-in

`gocached prog` and `gocached serve` read their cache dir from the same variable, `GOCACHED_DIR`, and resolve it by the same rules. By default the cache dir is private: `<user cache dir>/gocached`, created if missing, with no further checks. `GOCACHED_SHARE=1` turns on shared mode, where the cache dir is the host GOCACHE. In shared mode the default is resolved in this order:

1. `GOCACHED_DIR`, if set.
2. `go env GOCACHE`, which also honours `go env -w` settings.
3. If the `go` command is missing or fails, go's own rule: `$GOCACHE`, otherwise `<user cache dir>/go-build`. gocached logs that it fell back.

Shared mode also requires the README that go writes into every cache it initializes.

The README check only makes sense when the dir is claimed to be go's own cache, so it lives behind the switch. A private dir gets no check, which means a mistyped or unmounted path is silently created and served. The old server-only `GOCACHED_CACHE_DIR` is removed without an alias: 0.x makes no stability promise.

`gocached trace go` is the one command that turns shared mode on by itself: it sets `GOCACHED_SHARE=1` for the go command it runs when neither `GOCACHED_SHARE` nor `GOCACHED_DIR` is in the environment, and nothing cache-related with `GOCACHE=off`. Running it is the opt-in. A fresh GOCACHE passes the README check there, because go writes the README in `initDefaultCache` (cmd/go/internal/cache/default.go) before it starts `GOCACHEPROG`; gocached still never writes it.

The Docker image defaults to `GOCACHED_DIR=/cache` and `GOCACHED_SHARE=1`, because it exists to serve a mounted host GOCACHE.
