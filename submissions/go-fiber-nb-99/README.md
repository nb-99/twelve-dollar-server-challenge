# Go and Fiber submission

| | |
| --- | --- |
| Language | Go 1.27.2 from the official tarball, SHA-256 checked |
| Framework | Fiber v3.6.1 on fasthttp 1.75.0 |
| SQLite driver | `database/sql` with mattn/go-sqlite3 v1.14.52 (cgo, bundles SQLite) |
| JWT | golang-jwt/jwt v5.3.1 |
| Build | Go modules; `go.sum` pins every dependency |
| Nginx or direct | Direct. The app listens on `HOST:PORT`, `0.0.0.0:80` on the benchmark VM |

## Running on the benchmark VM

1. `sudo bash install.sh` installs Go under `/opt/go-challenge-1.27.2` and
   `build-essential` for the cgo SQLite driver.
2. `bash build.sh` runs `go vet` and the Go tests, then builds `bin/server`.
3. Set `SQLITE_PATH`, `JWT_SECRET`, `HOST`, and `PORT` as in `SPEC.md`.
4. `bash start.sh` replaces the shell with the server. It answers `/health`
   within a second.

Port 80 needs the `CAP_NET_BIND_SERVICE` capability that the challenge's service
grants. `SQLITE_PATH` must name an existing copy of the seed; the app opens it
with `mode=rw` and never creates or migrates a database.

## Implementation

The Go code follows the [module layout](https://go.dev/doc/modules/layout) for
a server: a command in `cmd/` and packages in `internal/`, where other modules
cannot import them.

| Package | Responsibility |
| --- | --- |
| `cmd/server` | Reads the configuration, sets garbage-collector defaults, wires the packages, and serves until `SIGINT` or `SIGTERM` |
| `internal/config` | The runtime contract: `SQLITE_PATH`, `JWT_SECRET`, `HOST`, `PORT`, and the optional `PPROF_ADDR` |
| `internal/api` | Fiber routes, request validation, and the like, health, and error bodies |
| `internal/auth` | HS256 bearer-token verification with golang-jwt, and the `sub` and `username` claim checks |
| `internal/store` | SQLite access: connection pools, pragmas, prepared statements, and the post bodies as JSON |
| `internal/testdb` | A small test database with the challenge schema, for the `api` and `store` tests |

Fasthttp keeps idle keep-alive connections open without a timeout, and Fiber
starts one goroutine per connection.

### Optimizations

Only the memory settings below come from a measured benchmark so far;
`PROFILE=true` benchmarks (see below) are meant to show which of the others
matter.

- **SQLite builds the post JSON.** The reads wrap the reference query from
  `SPEC.md` in `json_object` and `json_group_array`, so a feed crosses from C to
  Go as one value instead of 100 columns, and Go sends it without decoding or
  encoding it. `INSERT … RETURNING json_object(…)` does the same for a new post.
  Keys keep the spec's order, and the feed aggregate sorts its 20 posts with
  `ORDER BY` inside `json_group_array`, so the order does not depend on the
  query plan.
- **One writer, four readers.** Inserts use a pool with one connection, so
  concurrent writes queue in Go instead of in SQLite's busy handler. Reads use
  up to four `query_only` connections; WAL lets them run while a write commits.
  Neither size is tuned.
- **Prepared statements.** Each statement is prepared once per connection and
  reused.
- **SQLite settings.** `mmap_size` maps the about 240 MB database file into
  memory, so reads do not copy pages into each connection's cache. `_mutex=no`
  drops SQLite's per-connection mutex, because `database/sql` never shares a
  connection between goroutines.
- **One statement per like.** `INSERT … SELECT … FROM posts WHERE id = ? ON
  CONFLICT DO NOTHING` inserts nothing for a missing post. Only when it inserts
  nothing does a second query tell an existing like (200) from a missing post
  (404).
- **Typed JWT claims.** Claims decode into a struct instead of a map. `sub` and
  `username` stay untyped, so a wrong type is still `invalid token payload`.
- **Garbage collector.** Unless `GOGC` or `GOMEMLIMIT` is set, the server uses
  `GOGC=400` and a 1.5 GiB soft memory limit. The live heap is small, so
  collections run about a quarter as often as with the default. The limit
  includes goroutine stacks, about 8 KiB per open connection, and makes
  collections run earlier before Go could crowd out the OS and SQLite, whose
  memory Go does not count. A k8s benchmark with a 1 GiB limit reached it at
  about 25,000 connections, and GC took over half the CPU at 40,000.
- **Idle connections release their buffers.** Fiber's `ReduceMemoryUsage`
  frees each keep-alive connection's read and write buffers between requests.
  Load-test users are idle most of the time, and with 25,000 connections the
  read buffers alone held about 190 MB. It costs some CPU per request.

### Rules this keeps

- Every request reads SQLite while it is served. There is no response cache,
  query-result cache, table copy, or remembered JWT. Prepared statements cache
  query plans only.
- The writer sets WAL, `synchronous=NORMAL`, foreign keys, and a 5-second busy
  timeout. Inserts run in auto-commit mode, and a response is sent only after
  the commit succeeds.
- The schema is unchanged. Likes stay idempotent through the existing primary
  key and `ON CONFLICT DO NOTHING`.

## Behavior details

- Post lengths count Unicode code points, like SQLite's `length` check.
  `strings.TrimSpace` removes surrounding Unicode whitespace.
- SQLite's JSON functions escape quotes, backslashes, and control characters,
  and leave `<`, `>`, `&`, `/`, and non-ASCII characters as they are. The spec
  allows either.
- Fiber errors such as an unknown path keep their status, with the lowercase
  status text as the message: `{"error":"not found"}`. Wrong methods on known
  paths return 405, which the challenge leaves out of scope.
- A token whose `sub` names a user who does not exist cannot post. The foreign
  key rejects the insert, and the API returns 500.
- With `PPROF_ADDR` set, for example to `127.0.0.1:6060`, the server also serves
  `net/http/pprof` there. The challenge VM does not set it.

## Verification

- `bash test/run.sh submissions/go-fiber-nb-99` from the repository root runs the
  42-check suite as the challenge's CI does, after `install.sh`.
- `make test-suite` runs the same suite in Docker, or on this host with
  `MODE=direct`.
- The Go tests, which `build.sh` runs, cover exact response bytes, feed order
  with tied timestamps, an empty feed, special characters, id and token
  validation, body validation, commits that another connection can read,
  concurrent duplicate likes, internal errors, and a degraded health check.

## Development tooling

None of this runs on the benchmark VM. The Makefile and its scripts follow the
targets and settings of the separate `java-spring-boot-nb-99` submission, so
both run and compare the same way. `make help` lists every target and setting.

| Path | Contents |
| --- | --- |
| `Makefile` | Entry point for every target below |
| `scripts/` | One script per `MODE` (`direct.sh`, `docker.sh`, `k8s.sh`), their shared `lib.sh`, and `seed-local.sh`, which seeds a container's database |
| `deployments/docker/` | The multi-stage `Dockerfile` (app, seed, and test images) and `compose.yaml` |
| `deployments/k8s/` | The Kustomize base for the app and k6 pods |
| `tools/benchmark/` | `benchmark.py`, the load-limit search, and `report.py`, which renders its reports |

| Target | Action |
| --- | --- |
| `make build` | `gofmt` check, then `build.sh` with the installed Go (`GO=...` overrides it) |
| `make test-suite` | The unchanged 42-check `test/test.sh` against a fresh database |
| `make benchmark` | Warm-up, load-limit search, and reports |
| `make benchmark-smoke` | A short run that checks the tooling, not a score |
| `make up`, `make down`, `make logs` | A Compose stack on localhost port 3000 |
| `make run` | Run `bin/server`; needs `SQLITE_PATH` and `JWT_SECRET` |

`MODE` picks where the suite and benchmark run:

- `MODE=docker` (default) runs the app in one container limited to 1 vCPU, 2 GiB
  without swap, and 65,535 open files. k6 runs in its own container, or on the
  host with `BENCH_K6=local` when the Docker VM lacks memory for k6.
- `MODE=direct` runs `bin/server` and k6 on this host without limits. It
  downloads a missing k6 to a temporary directory and checks its SHA-256.
- `MODE=k8s` pushes images to `K8S_REGISTRY` and runs the app pod and the k6
  pods in `K8S_NAMESPACE`. The app's pod-level limit is 1 vCPU and 2 GiB. The cgo
  build runs on each node architecture, under emulation when it differs from the
  build host. Scheduling rules keep the app pod off nodes with other benchmark
  pods, including those of `java-spring-boot-nb-99` runs on the same cluster.
  A StatefulSet runs `K8S_K6_PODS` k6 pods (default 3), and each run gives every
  pod an equal share of the users through k6 execution segments. One pod cannot
  go much past 64,000 users: each user holds a connection to the server, and
  connections from one IP address need distinct local ports. More pods also
  spread k6's memory, about 310 KiB per user, over several nodes. Each pod
  requests and is limited to 10 GiB, about 30,000 users, so the scheduler places
  it only where that memory is free; with a smaller request, k6 once exhausted
  a node's memory instead of its own limit. `K8S_K6_MEMORY` sets the memory,
  and `K8S_K6_CPU_REQUEST` and `K8S_K6_CPU` the CPU request and limit
  (defaults 500m and 6). Past its memory limit, k6 is OOM-killed and the
  benchmark stops with exit 137. A pod that no node has room for stops the run
  with the scheduler's reason.
  The report combines the pods' k6 summaries. Requests, errors, and checks are
  totals; the error rate is exact. p95 and p99 are the highest pod's value: the
  whole run's percentile lies between the pods' values, so a run passes the
  latency thresholds only if every pod did. k6 numbers each pod's users from 1,
  so each pod's token list starts at a different offset, and the pods together
  use as many distinct users as one k6 process would.
  `K8S_NODE=<node name>` runs all pods on that node instead of separate ones;
  load then never crosses the network, and parallel runs share the node.
  `K8S_APP_NODE=<node name>` pins only the app pod, for example to the node
  with the least free memory, and the k6 pods take the others.
  Copy `.env-sample` to `.env` for these settings; `.envrc` loads it with direnv.

The benchmark follows the challenge's scoring: a 2-minute warm-up with 1,000
users, 1-minute probes that double from `BENCH_START_VUS` until one fails,
refinement to within 250 users, and a 5-minute confirmation. A run passes with
p95 below 500 ms, p99 below 1,000 ms, and fewer than 1% failed requests.

Results go to `benchmark-results/<run-id>/`: `report.md`, `report.html`,
`results.json`, raw k6 output, and charts. A local result is not a challenge
score.

### Profiling

`PROFILE=true` works in every mode, for example
`make benchmark MODE=k8s PROFILE=true`. The Docker and Kubernetes deployments
set `PPROF_ADDR=127.0.0.1:6060` inside the app container, and direct mode picks a
free port. For each run, `curl` (on the host, or through `docker exec` or
`kubectl exec`) saves a CPU profile of the hold, after the ramp-up, and a heap
profile after the run to `pprof/run-NNN.{cpu,heap}.pb.gz`. The report links
them. Open one with:

```sh
go tool pprof -http=: benchmark-results/<run-id>/pprof/run-003.cpu.pb.gz
```

The Go profiler cannot see into C, so SQLite's time appears under
`runtime.cgocall` below the `store` call that started it.
