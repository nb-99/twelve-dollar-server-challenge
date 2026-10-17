# Build context: the repository root (see compose.yaml and scripts/k8s.sh).
# The cgo SQLite driver compiles C, so the build stage runs on the target
# platform; a cross-architecture build for Kubernetes runs under emulation.
FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55 AS build
COPY submissions/go-fiber-nb-99/install.sh /tmp/install.sh
RUN bash /tmp/install.sh
ENV PATH="/opt/go-challenge-1.27.2/bin:${PATH}"
# The repository layout is kept, because the tests read /src/schema.sql.
WORKDIR /src/submissions/go-fiber-nb-99
COPY submissions/go-fiber-nb-99/go.mod submissions/go-fiber-nb-99/go.sum ./
RUN go mod download
COPY schema.sql /src/schema.sql
COPY submissions/go-fiber-nb-99/build.sh ./
COPY submissions/go-fiber-nb-99/cmd ./cmd
COPY submissions/go-fiber-nb-99/internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build bash build.sh

FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55 AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home app
WORKDIR /app
COPY --from=build /src/submissions/go-fiber-nb-99/bin/server ./bin/server
COPY submissions/go-fiber-nb-99/start.sh ./
# The app serves clients directly on port 80, as on the challenge VPS. Docker and
# the Kubernetes pod allow unprivileged processes to bind it.
ENV HOST=0.0.0.0 PORT=80
USER 10001
EXPOSE 80
ENTRYPOINT ["bash", "/app/start.sh"]

FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55 AS checks
RUN apt-get update \
    && apt-get install -y --no-install-recommends nodejs sqlite3 curl jq openssl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /challenge
COPY schema.sql ./schema.sql
# make-seed.sh writes beside itself; Kubernetes seeds as the app user.
COPY --chown=10001:10001 seed ./seed
COPY test ./test
COPY bench/load.js ./bench/load.js
COPY submissions/go-fiber-nb-99/scripts/seed-local.sh ./seed-local.sh
ENTRYPOINT ["bash"]
