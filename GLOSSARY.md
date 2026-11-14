# gocached

A self-hosted Go build cache for intranet CI: a client that `go` talks to through `GOCACHEPROG`, and a server that shares its host's own Go build cache with those clients.

## Language

**Action ID**:
The key `go` computes for one build step. Every cache lookup and store is addressed by an action ID.

**Output ID**:
The content hash of the output a build step produced. It lets anyone holding the content check that it is what the cache claims.

**Object**:
What the cache stores for one action ID: the output content plus its output ID, size, and the time it entered the cache.

**Kind**:
What sort of entry an object is, such as a package index or a compiled package, recognized from the first bytes of its content when it is stored. It decides whether an object is worth sharing with other machines.

**Backend**:
One place objects can be stored and looked up, such as the local disk or a remote server.

**Chain**:
An ordered list of backends consulted in turn. The first backend is always the local disk, because `go` needs every hit as a file on disk.

**Hit**:
The first backend in a chain that holds the requested action ID, together with the object it holds.

**Fill**:
Copying a hit back into the backends ahead of it in the chain, so the next lookup finds it sooner.

**Host GOCACHE**:
The server machine's own Go build cache, which the server shares with clients instead of keeping a separate store.

**Cache dir**:
The directory that holds the local backend, used by both client and server. It is private to gocached by default.

**Shared mode**:
Running with the host GOCACHE as the cache dir, so that `go` and gocached read and write the same cache. In this mode the dir must already have been initialized by `go`.
