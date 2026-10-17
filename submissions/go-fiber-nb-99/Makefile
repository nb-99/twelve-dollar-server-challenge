SHELL := /bin/bash
.DEFAULT_GOAL := help
.NOTPARALLEL:

DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
BIN := $(DIR)/bin/server
SOURCES := $(DIR)/go.mod $(DIR)/go.sum $(shell find "$(DIR)/cmd" "$(DIR)/internal" -name "*.go")
COMPOSE := docker compose -f "$(DIR)/deployments/docker/compose.yaml"
NATIVE_DOCKER := export DOCKER_DEFAULT_PLATFORM="$$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')" &&

# Where test-suite and benchmark run: docker (default), direct, or k8s.
MODE ?= docker
GO ?= go
BUILD_IMAGES ?= true
WAIT_TIMEOUT ?= 120
# Where k6 runs in MODE=docker: docker (default) or local (this host).
BENCH_K6 ?= docker
# Image repository prefix that MODE=k8s pushes to, e.g. registry.example.com/owner.
K8S_REGISTRY ?=
# Existing namespace for MODE=k8s, and an optional image pull secret in it.
K8S_NAMESPACE ?= twelve-dollar-challenge
K8S_PULL_SECRET ?=
# Number of k6 pods in MODE=k8s benchmarks (default 3), each running an equal share
# of the users, and overrides of each pod's CPU request (default 500m), CPU limit
# (default 6), and memory request and limit (default 10Gi, about 30,000 users).
K8S_K6_PODS ?=
K8S_K6_CPU_REQUEST ?=
K8S_K6_CPU ?=
K8S_K6_MEMORY ?=
# Node that runs the app and k6 pods in MODE=k8s; empty puts them on separate nodes.
K8S_NODE ?=
# Node for only the app pod in MODE=k8s; the k6 pods then avoid it.
K8S_APP_NODE ?=
# Save CPU and heap profiles of each benchmark run.
PROFILE ?= false
export BUILD_IMAGES WAIT_TIMEOUT BENCH_K6 K8S_REGISTRY K8S_NAMESPACE K8S_PULL_SECRET \
  K8S_K6_PODS K8S_K6_CPU_REQUEST K8S_K6_CPU K8S_K6_MEMORY K8S_NODE K8S_APP_NODE PROFILE

BENCH_WARMUP_VUS ?= 1000
BENCH_WARMUP_SECONDS ?= 120
BENCH_START_VUS ?= 2500
BENCH_MAX_VUS ?= 80000
BENCH_STEP ?= 250
BENCH_PROBE_SECONDS ?= 60
BENCH_CONFIRM_SECONDS ?= 300
BENCH_CONFIRM_RUNS ?= 1
BENCH_RAMP_SECONDS ?= 60
BENCH_BUDGET_SECONDS ?= 3600
BENCH_OUTPUT ?=
BENCH_ARGS ?=
REPORT_DIR ?= $(BENCH_OUTPUT)

ifeq ($(filter $(MODE),direct docker k8s),)
$(error MODE must be direct, docker, or k8s)
endif
# Direct mode runs the locally built binary; Docker builds its own image.
MODE_DEPS := $(if $(filter direct,$(MODE)),$(BIN))

.PHONY: help all build test test-suite image run up down logs benchmark benchmark-smoke report

help:
	@printf '%s\n' \
	  'make build            Format check, go vet, Go tests, and bin/server with installed Go' \
	  'make test             build, then the 42-check challenge suite' \
	  'make test-suite       Challenge suite on a fresh database (MODE=docker|direct|k8s)' \
	  'make benchmark        Warm-up, limit search, and reports (MODE=docker|direct|k8s)' \
	  'make benchmark-smoke  Short benchmark run that checks the tooling, not a score' \
	  'make report           Regenerate the latest report, or REPORT_DIR=<run directory>' \
	  'make all              build, test-suite, and benchmark' \
	  'make image            Build native app, seed, and test images' \
	  'make up / down / logs Regular Docker stack on LOCAL_PORT (default 3000)' \
	  'make run              Run bin/server; requires SQLITE_PATH and JWT_SECRET' \
	  '' \
	  'The app serves clients directly, without Nginx.' \
	  'MODE=docker runs the app in one container limited to 1 vCPU and 2 GiB,' \
	  'like the challenge VPS. BENCH_K6=local runs k6 on this host instead of in Docker.' \
	  'MODE=direct runs bin/server and k6 on this host and downloads a missing k6' \
	  'to a temporary directory.' \
	  'MODE=k8s pushes images to K8S_REGISTRY and runs the app in a pod limited to' \
	  '1 vCPU and 2 GiB in namespace K8S_NAMESPACE of the current kubectl context,' \
	  'pulling with K8S_PULL_SECRET if set. k6 runs in K8S_K6_PODS=3 pods on other' \
	  'nodes, each with 10 GiB for about 30,000 users and at most 64,000 users' \
	  '(K8S_K6_CPU_REQUEST, K8S_K6_CPU, and K8S_K6_MEMORY change the resources).' \
	  'K8S_NODE=<node name> runs all pods on that node; K8S_APP_NODE=<node name>' \
	  'runs only the app there and k6 elsewhere.' \
	  'PROFILE=true saves a pprof CPU profile of each benchmark hold and a heap' \
	  'profile after each run in the report directory.' \
	  'Benchmark settings: BENCH_WARMUP_VUS=1000 BENCH_WARMUP_SECONDS=120' \
	  'BENCH_START_VUS=2500 BENCH_MAX_VUS=80000 BENCH_STEP=250 BENCH_PROBE_SECONDS=60' \
	  'BENCH_CONFIRM_SECONDS=300 BENCH_CONFIRM_RUNS=1 BENCH_RAMP_SECONDS=60' \
	  'BENCH_BUDGET_SECONDS=3600 BENCH_OUTPUT=<new directory> BENCH_ARGS="..."'

all:
	$(MAKE) -C "$(DIR)" build
	$(if $(filter docker,$(MODE)),$(MAKE) -C "$(DIR)" image)
	$(MAKE) -C "$(DIR)" test-suite BUILD_IMAGES=false
	$(MAKE) -C "$(DIR)" benchmark BUILD_IMAGES=false

build:
	@unformatted=$$("$$("$(GO)" env GOROOT)/bin/gofmt" -l "$(DIR)"); \
	  if [ -n "$$unformatted" ]; then echo "gofmt needed: $$unformatted" >&2; exit 1; fi
	GO="$(GO)" bash "$(DIR)/build.sh"

$(BIN): $(SOURCES)
	GO="$(GO)" bash "$(DIR)/build.sh"

test: build
	$(MAKE) -C "$(DIR)" test-suite

test-suite: $(MODE_DEPS)
	bash "$(DIR)/scripts/$(MODE).sh" test

benchmark: $(MODE_DEPS)
	bash "$(DIR)/scripts/$(MODE).sh" benchmark \
	  --warmup-vus "$(BENCH_WARMUP_VUS)" --warmup-seconds "$(BENCH_WARMUP_SECONDS)" \
	  --start-vus "$(BENCH_START_VUS)" --max-vus "$(BENCH_MAX_VUS)" --step "$(BENCH_STEP)" \
	  --probe-seconds "$(BENCH_PROBE_SECONDS)" --confirm-seconds "$(BENCH_CONFIRM_SECONDS)" \
	  --confirm-runs "$(BENCH_CONFIRM_RUNS)" --ramp-seconds "$(BENCH_RAMP_SECONDS)" \
	  --budget-seconds "$(BENCH_BUDGET_SECONDS)" \
	  $(if $(BENCH_OUTPUT),--output "$(BENCH_OUTPUT)") $(BENCH_ARGS)

benchmark-smoke:
	$(MAKE) -C "$(DIR)" benchmark BENCH_WARMUP_VUS=50 BENCH_WARMUP_SECONDS=8 \
	  BENCH_START_VUS=50 BENCH_MAX_VUS=100 BENCH_STEP=50 BENCH_PROBE_SECONDS=8 \
	  BENCH_CONFIRM_SECONDS=10 BENCH_RAMP_SECONDS=1 BENCH_BUDGET_SECONDS=600

report:
	python3 "$(DIR)/tools/benchmark/report.py" $(if $(REPORT_DIR),"$(REPORT_DIR)")

image:
	$(NATIVE_DOCKER) $(COMPOSE) --profile test build server seed test

up:
	$(NATIVE_DOCKER) $(COMPOSE) up --build -d --wait --wait-timeout "$(WAIT_TIMEOUT)"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs --follow server seed

run: $(BIN)
	exec bash "$(DIR)/start.sh"
