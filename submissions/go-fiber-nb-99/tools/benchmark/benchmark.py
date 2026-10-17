#!/usr/bin/env python3
"""Warm up a running feed server, search its k6 load limit, and write reports.

The caller deploys the app (directly, in Docker, or in Kubernetes) and
passes its URL. This tool never starts, resets, or stops the app.
"""

import argparse
import datetime
import hashlib
import json
import math
import os
import platform
import resource
import shlex
import signal
import subprocess
import sys
import threading
import time
import traceback
import urllib.request
import uuid
from fractions import Fraction
from pathlib import Path

from report import RESULTS, report

# The challenge's k6 workload, in the repository's bench/ directory.
WORKLOAD = Path(__file__).resolve().parents[4] / "bench" / "load.js"
K6_IMAGE = "grafana/k6:2.2.0@sha256:9bd01d6941fca969cb61bb57d2da5ee9b385fe2aa8881df3798c196564d6ace6"
# --k6 exec contract: the k6 container holds load.js and tokens.json here, and
# a writable results/ directory.
REMOTE = Path("/work")
CHECKS = ("feed 200", "post 200", "like 200/201", "create 201")
THRESHOLDS = (
    ("http_req_duration", "p(95)<500", "p(95)", 500),
    ("http_req_duration", "p(99)<1000", "p(99)", 1000),
    ("http_req_failed", "rate<0.01", "value", 0.01),
)
RAMP_DOWN_SECONDS = 30
# Covers k6's graceful stop plus process or container startup.
RUN_ALLOWANCE_SECONDS = 60
SAMPLE_SECONDS = 2
# k6 closes its connections when a run ends, and Linux keeps each closed
# connection's local port in TIME_WAIT for 60 seconds. A persistent k6 container
# that starts the next run sooner can run out of ports and fail to connect.
TIME_WAIT_SCRIPT = (
    "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk '$4 == \"06\"' | wc -l"
)
TIME_WAIT_LIMIT = 100
TIME_WAIT_TIMEOUT_SECONDS = 120
# Connections from one address to the server need distinct local ports; the k6
# containers in deployments/k8s/k6.yaml and Docker mode allow 1024-65535.
PORTS_PER_K6 = 64000


class ExecutionError(Exception):
    pass


class AppStopped(ExecutionError):
    pass


class BudgetExpired(Exception):
    pass


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def number(value):
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(value)
        or value < 0
    ):
        raise ExecutionError("Invalid numeric value in k6 summary")
    return value


def parse_summary(summary, exit_code, minimum_seconds=None):
    """Validate k6's pinned legacy JSON contract, not its human-readable output."""
    if exit_code not in (0, 99):
        raise ExecutionError(f"k6 execution failed with exit code {exit_code}")
    try:
        metrics = summary["metrics"]
        requests = number(metrics["http_reqs"]["count"])
        if requests == 0:
            raise ExecutionError("k6 issued no requests")
        vus = number(metrics["vus_max"]["value"])
        if number(metrics["vus"]["max"]) != vus:
            raise ExecutionError("k6 did not reach its virtual-user count")
        rate = number(metrics["http_reqs"]["rate"])
        if rate == 0 or (
            minimum_seconds is not None and requests / rate < minimum_seconds
        ):
            raise ExecutionError("k6 did not complete the requested stage duration")
        thresholds = {}
        for metric, expression, key, limit in THRESHOLDS:
            value = number(metrics[metric][key])
            failed = metrics[metric]["thresholds"][expression]
            # In --summary-export, the boolean is 'failed', not 'ok'.
            if type(failed) is not bool or failed != (value >= limit):
                raise ExecutionError(f"Inconsistent threshold result: {expression}")
            thresholds[expression] = not failed
        passed = all(thresholds.values())
        if (exit_code == 0) != passed:
            raise ExecutionError("k6 exit code disagrees with threshold outcomes")
        groups = summary["root_group"]["checks"]
        checks = {}
        for name in CHECKS:
            check = groups.get(name, {})
            checks[name] = {
                "passed": number(check.get("passes", 0)),
                "failed": number(check.get("fails", 0)),
            }
        if checks["feed 200"]["passed"] + checks["feed 200"]["failed"] == 0:
            raise ExecutionError("The expected feed workload was not exercised")
        return {
            "status": "passed" if passed else "threshold failed",
            "reached_vus": vus,
            "thresholds": thresholds,
            "checks": checks,
            "metrics": {
                "requests": requests,
                "rps": rate,
                "avg_ms": number(metrics["http_req_duration"]["avg"]),
                "p95_ms": number(metrics["http_req_duration"]["p(95)"]),
                "p99_ms": number(metrics["http_req_duration"]["p(99)"]),
                "max_ms": number(metrics["http_req_duration"]["max"]),
                "error_percent": number(metrics["http_req_failed"]["value"]) * 100,
                # A Rate metric's passes are its true samples: failed requests.
                "failed_requests": number(metrics["http_req_failed"]["passes"]),
            },
        }
    except (KeyError, TypeError, AttributeError) as error:
        raise ExecutionError(f"Missing or invalid k6 summary field: {error}") from error


def merge(parts):
    """Combine the parsed summaries of k6 processes that shared one test.

    Counts and the error rate combine exactly. The whole test's p95 and p99 lie
    between the processes' values, so the highest stands in for them: a test
    passes the latency thresholds only if every process did.
    """
    if len(parts) == 1:
        return parts[0]
    metrics = [part["metrics"] for part in parts]
    requests = sum(m["requests"] for m in metrics)
    failed = sum(m["failed_requests"] for m in metrics)
    merged = {
        "requests": requests,
        "rps": sum(m["rps"] for m in metrics),
        "avg_ms": sum(m["avg_ms"] * m["requests"] for m in metrics) / requests,
        "p95_ms": max(m["p95_ms"] for m in metrics),
        "p99_ms": max(m["p99_ms"] for m in metrics),
        "max_ms": max(m["max_ms"] for m in metrics),
        "error_percent": failed / requests * 100,
        "failed_requests": failed,
    }
    values = {
        "p(95)": merged["p95_ms"],
        "p(99)": merged["p99_ms"],
        "value": failed / requests,
    }
    thresholds = {
        expression: values[key] < limit for _, expression, key, limit in THRESHOLDS
    }
    return {
        "status": "passed" if all(thresholds.values()) else "threshold failed",
        "reached_vus": sum(part["reached_vus"] for part in parts),
        "thresholds": thresholds,
        "checks": {
            name: {
                outcome: sum(part["checks"][name][outcome] for part in parts)
                for outcome in ("passed", "failed")
            }
            for name in CHECKS
        },
        "metrics": merged,
    }


def validate_events(path, exit_code):
    """Iteration exceptions can be logged even when k6 exits successfully."""
    if not path.is_file():
        raise ExecutionError("k6 did not produce its structured event log")
    for line in path.read_text(encoding="utf-8").splitlines():
        event = json.loads(line)
        message = event.get("msg", "")
        threshold_error = (
            exit_code == 99
            and message.startswith("thresholds on metrics ")
            and message.endswith(" have been crossed")
        )
        if event.get("level") == "error" and not threshold_error:
            raise ExecutionError(f"k6 logged an execution error: {message}")


def search(run, start, maximum, step, confirmations):
    """Only genuine threshold failures shrink the passing/failing interval."""
    low, high = 0, None
    candidate = start
    while True:
        if run("search", candidate):
            low = candidate
            if candidate == maximum:
                break
            candidate = min(candidate * 2, maximum)
        else:
            high = candidate
            break
    if high is not None:
        while high - low > step:
            candidate = ((low + high) // (2 * step)) * step
            if run("refine", candidate):
                low = candidate
            else:
                high = candidate
    candidate = low
    while candidate >= step:
        if all(run("confirm", candidate) for _ in range(confirmations)):
            return candidate, high is None and candidate == maximum
        candidate -= step
    return None, False


def execute(args, *, timeout=60):
    """Return stdout."""
    result = subprocess.run(
        args,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )
    if result.returncode:
        raise ExecutionError(
            f"{shlex.join(args)} exited {result.returncode}: {result.stderr.strip()}"
        )
    return result.stdout


def execute_all(commands, log, timeout, env):
    """Run commands concurrently, all writing to one log; return their exit codes."""
    deadline = time.monotonic() + timeout
    processes = []
    with open(log, "w", encoding="utf-8") as stream:
        try:
            for command in commands:
                processes.append(
                    subprocess.Popen(
                        command,
                        stdin=subprocess.DEVNULL,
                        stdout=stream,
                        stderr=subprocess.STDOUT,
                        env=env,
                        start_new_session=True,
                    )
                )
            return [
                process.wait(timeout=max(0, deadline - time.monotonic()))
                for process in processes
            ]
        finally:
            for process in processes:
                if process.poll() is None:
                    process.kill()
                    process.wait()


def healthy(url):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(f"{url}/health", timeout=5) as response:
            data = json.load(response)
            return (
                response.status == 200
                and data.get("status") == "ok"
                and data.get("db") == "ok"
            )
    except (OSError, ValueError):
        return False


def wait_healthy(url, seconds):
    deadline = time.monotonic() + seconds
    while not healthy(url):
        if time.monotonic() > deadline:
            raise ExecutionError(f"{url}/health was not healthy within {seconds}s")
        time.sleep(1)


def wait_for_ports(remote):
    """Wait until the previous run's closed k6 connections release their ports."""
    deadline = time.monotonic() + TIME_WAIT_TIMEOUT_SECONDS
    announced = False
    while (
        count := int(execute([*remote, "sh", "-c", TIME_WAIT_SCRIPT]))
    ) > TIME_WAIT_LIMIT:
        if time.monotonic() > deadline:
            raise ExecutionError(f"k6 still holds {count} ports in TIME_WAIT")
        if not announced:
            print(
                f"  Waiting for {count} closed k6 connections to release their ports",
                flush=True,
            )
            announced = True
        time.sleep(5)


def cpu_seconds(text):
    """Parse ps TIME values such as 1-02:03:04, 01:02:03, or 12:34.56."""
    days, _, clock = text.rpartition("-")
    seconds = 0.0
    for part in clock.split(":"):
        seconds = seconds * 60 + float(part)
    return seconds + int(days or 0) * 86400


class ProcessProbe:
    """Direct mode: one host process, read from /proc on Linux or ps on macOS."""

    source = "app process CPU time and resident memory"

    def __init__(self, pid):
        self.pid = pid

    def sample(self):
        try:
            os.kill(self.pid, 0)
        except ProcessLookupError as error:
            raise AppStopped(f"App process {self.pid} exited") from error
        except PermissionError:
            pass
        proc = Path(f"/proc/{self.pid}")
        if proc.is_dir():
            fields = (proc / "stat").read_text().rpartition(")")[2].split()
            cpu = (int(fields[11]) + int(fields[12])) / os.sysconf("SC_CLK_TCK")
            status = (proc / "status").read_text().splitlines()
            rss_kib = next(int(l.split()[1]) for l in status if l.startswith("VmRSS:"))
        else:
            used, rss = execute(["ps", "-o", "time=,rss=", "-p", str(self.pid)]).split()
            cpu, rss_kib = cpu_seconds(used), int(rss)
        return {"cpu_seconds": cpu, "memory_mib": rss_kib / 1024, "oom_kills": None}


class CgroupProbe:
    """Container modes: cgroup v2 counters read through exec command prefixes.

    Several prefixes, such as the containers of one pod, are summed.
    """

    source = "container cgroup CPU, memory.current including page cache, and OOM kills"
    SCRIPT = (
        "cd /sys/fs/cgroup && grep '^usage_usec ' cpu.stat"
        " && echo memory_current $(cat memory.current)"
        " && grep '^oom_kill ' memory.events"
    )

    def __init__(self, prefixes):
        self.prefixes = [shlex.split(prefix) for prefix in prefixes]
        self.cpu = None

    def limits(self):
        """Record the enforced limits, e.g. the challenge VPS budget."""
        script = "cd /sys/fs/cgroup && cat cpu.max memory.max && (cat memory.swap.max || echo n/a)"
        cpu, memory, swap = execute(
            [*self.prefixes[0], "sh", "-c", script]
        ).splitlines()
        quota, period = cpu.split()

        def mib(value):
            return None if value in ("max", "n/a") else int(value) / 1024**2

        return {
            "cpus": None if quota == "max" else int(quota) / int(period),
            "memory_mib": mib(memory),
            "swap_mib": mib(swap),
        }

    def sample(self):
        parts = [
            dict(
                line.split()
                for line in execute([*prefix, "sh", "-c", self.SCRIPT]).splitlines()
            )
            for prefix in self.prefixes
        ]
        cpu = [int(part["usage_usec"]) / 1e6 for part in parts]
        if self.cpu and any(now < before for now, before in zip(cpu, self.cpu)):
            raise AppStopped("A container restarted")
        self.cpu = cpu
        return {
            "cpu_seconds": sum(cpu),
            "memory_mib": sum(int(part["memory_current"]) for part in parts) / 1024**2,
            "oom_kills": sum(int(part["oom_kill"]) for part in parts),
        }


class Sampler:
    """Samples the app in the background while k6 runs."""

    def __init__(self, probe):
        self.probe = probe
        self.samples = []
        self.error = None
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self._loop, daemon=True)
        self.thread.start()

    def _loop(self):
        while not self.error:
            self._take()
            if self.stopped.wait(SAMPLE_SECONDS):
                return

    def _take(self):
        try:
            self.samples.append((time.monotonic(), self.probe.sample()))
        except (
            ExecutionError,
            OSError,
            ValueError,
            KeyError,
            IndexError,
            StopIteration,
            subprocess.SubprocessError,
        ) as error:
            self.error = error

    def finish(self):
        self.stopped.set()
        self.thread.join()
        if not self.error:
            self._take()
        if isinstance(self.error, AppStopped):
            raise self.error
        if self.error or len(self.samples) < 2:
            reason = str(self.error or "fewer than two samples")
            return {"source": self.probe.source, "unavailable": reason}
        (start, first), (end, last) = self.samples[0], self.samples[-1]
        if last["cpu_seconds"] < first["cpu_seconds"]:
            raise ExecutionError("The app restarted during the run")
        oom_kills = (
            None
            if last["oom_kills"] is None
            else last["oom_kills"] - first["oom_kills"]
        )
        if oom_kills:
            raise ExecutionError(f"The app was OOM-killed {oom_kills} time(s)")
        return {
            "source": self.probe.source,
            "samples": len(self.samples),
            "cpu_percent": (last["cpu_seconds"] - first["cpu_seconds"])
            / (end - start)
            * 100,
            "peak_memory_mib": max(sample["memory_mib"] for _, sample in self.samples),
            "oom_kills": oom_kills,
        }


class Profiler:
    """Saves Go pprof profiles of each run from the app's PPROF_ADDR listener.

    A CPU profile starts when the ramp-up should end and lasts the hold; a heap
    profile follows the run. curl runs on this host, or in the app container
    through a prefix. A run that ends early stops only the local process, so a
    remote curl finishes its profile; such runs end the search anyway.
    """

    def __init__(self, address, prefix):
        self.command = [*shlex.split(prefix or ""), "curl", "--noproxy", "*"]
        self.command += ["--fail", "--silent", "--show-error"]
        self.base = f"http://{address}/debug/pprof"
        self.lock = threading.Lock()

    def start(self, delay, seconds, destination):
        destination.parent.mkdir(exist_ok=True)
        self.cpu = destination
        self.process = None
        self.cancelled = threading.Event()
        self.thread = threading.Thread(
            target=self._cpu, args=(delay, seconds), daemon=True
        )
        self.thread.start()

    def _cpu(self, delay, seconds):
        if self.cancelled.wait(delay):
            return
        with self.lock:
            if self.cancelled.is_set():
                return
            with open(self.cpu, "wb") as stream:
                try:
                    self.process = subprocess.Popen(
                        [*self.command, f"{self.base}/profile?seconds={seconds}"],
                        stdin=subprocess.DEVNULL,
                        stdout=stream,
                        stderr=subprocess.DEVNULL,
                    )
                except OSError:
                    return  # stop() reports the missing profile.
        self.process.wait()

    def stop(self, heap):
        """Returns whether the CPU and heap profiles were saved."""
        with self.lock:
            self.cancelled.set()
            if self.process and self.process.poll() is None:
                self.process.terminate()
        self.thread.join()
        cpu = bool(self.process and self.process.returncode == 0)
        if not cpu:
            self.cpu.unlink(missing_ok=True)
        try:
            with open(heap, "wb") as stream:
                saved = (
                    subprocess.run(
                        [*self.command, "--max-time", "60", f"{self.base}/heap"],
                        stdin=subprocess.DEVNULL,
                        stdout=stream,
                        stderr=subprocess.DEVNULL,
                        timeout=120,
                        check=False,
                    ).returncode
                    == 0
                )
        except (OSError, subprocess.SubprocessError):
            saved = False
        if not saved:
            heap.unlink(missing_ok=True)
        return cpu, saved


def segments(count):
    """k6 options that give each of count processes an equal share of one test."""
    bounds = [str(Fraction(i, count)) for i in range(count + 1)]
    sequence = f"--execution-segment-sequence={','.join(bounds)}"
    return [
        [f"--execution-segment={start}:{end}", sequence]
        for start, end in zip(bounds, bounds[1:])
    ]


def k6_command(args, output, container, vus, hold, stem, shard=0):
    variables = {
        "BASE_URL": args.k6_url or args.url,
        "VUS": vus,
        "DURATION": f"{hold}s",
        "RAMP_UP": f"{args.ramp_seconds}s",
    }
    segment = []
    if args.k6 == "local":
        prefix = [args.k6_bin]
        results, workload = output / "k6", WORKLOAD
        variables["TOKENS"] = args.tokens.resolve()
    elif args.k6 == "exec":
        prefix = shlex.split(args.k6_exec[shard]) + ["k6"]
        results, workload = REMOTE / "results", REMOTE / "load.js"
        variables["TOKENS"] = REMOTE / "tokens.json"
        if len(args.k6_exec) > 1:
            segment = segments(len(args.k6_exec))[shard]
    else:
        prefix = ["docker", "run", "--rm", "--name", container]
        prefix += ["--user", f"{os.getuid()}:{os.getgid()}", "-e", "NO_PROXY=*"]
        # One local port per user; Docker's default range has about 28,000.
        prefix += ["--sysctl", "net.ipv4.ip_local_port_range=1024 65535"]
        prefix += ["-v", f"{WORKLOAD.parent}:/bench:ro"]
        prefix += ["-v", f"{args.tokens.resolve()}:/tokens.json:ro"]
        prefix += ["-v", f"{output / 'k6'}:/results"]
        if args.docker_network:
            prefix += ["--network", args.docker_network]
        prefix.append(K6_IMAGE)
        results, workload = Path("/results"), Path("/bench/load.js")
        variables["TOKENS"] = "/tokens.json"
    return [
        *prefix,
        "run",
        "--quiet",
        "--no-usage-report",
        "--new-machine-readable-summary=false",
        "--log-format=json",
        f"--log-output=file={results / (stem + '.events.jsonl')}",
        f"--summary-export={results / (stem + '.json')}",
        *segment,
        *(f"--env={key}={value}" for key, value in variables.items()),
        str(workload),
    ]


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="Base URL checked by this tool")
    parser.add_argument(
        "--tokens", type=Path, help="Seed tokens.json; not used with --k6 exec"
    )
    parser.add_argument("--mode", default="external", help="Deployment label")
    parser.add_argument("--k6", choices=("local", "docker", "exec"), default="local")
    parser.add_argument("--k6-bin", default="k6", help="Local k6 executable")
    parser.add_argument(
        "--k6-exec",
        action="append",
        help="With --k6 exec, the prefix that runs commands in a k6 container, "
        'e.g. "kubectl exec -n NS k6-0 -c k6 --". The container must hold /work/load.js, '
        "/work/tokens.json, and a writable /work/results. Repeat it to split each "
        "run's users evenly over several containers.",
    )
    parser.add_argument("--k6-url", help="Base URL seen by k6; defaults to --url")
    parser.add_argument("--docker-network", help="Network for the k6 container")
    probe = parser.add_mutually_exclusive_group()
    probe.add_argument("--app-pid", type=int, help="Sample this host process")
    probe.add_argument(
        "--app-exec",
        action="append",
        help='Sample cgroup counters through a command prefix, e.g. "docker exec ID"; '
        "repeat it to sum several containers",
    )
    parser.add_argument(
        "--app-limits",
        type=json.loads,
        help="Enforced limits as JSON, when the probe cannot read them",
    )
    parser.add_argument(
        "--pprof",
        metavar="HOST:PORT",
        help="Save CPU and heap profiles of each run from the app's PPROF_ADDR",
    )
    parser.add_argument(
        "--pprof-exec",
        help="Reach --pprof through this command prefix, which must provide curl, "
        'e.g. "kubectl exec -n NS server -c app --"; defaults to this host',
    )
    parser.add_argument("--output", type=Path, help="New report directory")
    parser.add_argument("--wait-seconds", type=int, default=120)
    parser.add_argument("--warmup-vus", type=int, default=1000)
    parser.add_argument("--warmup-seconds", type=int, default=120)
    parser.add_argument("--start-vus", type=int, default=2500)
    parser.add_argument("--max-vus", type=int, default=20000)
    parser.add_argument("--step", type=int, default=250)
    parser.add_argument("--probe-seconds", type=int, default=60)
    parser.add_argument("--confirm-seconds", type=int, default=300)
    parser.add_argument("--confirm-runs", type=int, default=1)
    parser.add_argument("--ramp-seconds", type=int, default=60)
    parser.add_argument(
        "--budget-seconds",
        type=int,
        default=3600,
        help="Load-search budget after the app is healthy",
    )
    args = parser.parse_args(argv)
    for key, value in vars(args).items():
        if isinstance(value, int) and value <= 0:
            parser.error(f"--{key.replace('_', '-')} must be positive")
    if (
        args.start_vus > args.max_vus
        or args.start_vus % args.step
        or args.max_vus % args.step
    ):
        parser.error("start/max users must be step multiples, with start <= max")
    if args.k6 != "exec" and not (args.tokens and args.tokens.is_file()):
        parser.error(f"--tokens {args.tokens} does not exist")
    if (args.k6 == "exec") != bool(args.k6_exec):
        parser.error("--k6-exec is required with, and only valid for, --k6 exec")
    if args.k6_exec and min(args.warmup_vus, args.step) < len(args.k6_exec):
        parser.error("Every k6 container needs at least one user in each run")
    if args.k6_exec and (
        math.ceil(max(args.warmup_vus, args.max_vus) / len(args.k6_exec)) > PORTS_PER_K6
    ):
        parser.error(
            f"Each k6 container can hold about {PORTS_PER_K6:,} connections to the "
            "server, one per user; add containers or lower the user cap"
        )
    if args.pprof_exec and not args.pprof:
        parser.error("--pprof-exec needs --pprof")
    return args


def main(argv=None):
    args = arguments(argv)
    suffix = uuid.uuid4().hex[:10]
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output = (args.output or RESULTS / f"{stamp}-{args.mode}-{suffix}").resolve()
    try:
        output.mkdir(parents=True, exist_ok=False)
        (output / "k6").mkdir()
    except OSError as error:
        print(f"Cannot create a new output directory: {error}", file=sys.stderr)
        return 2
    container = f"twelve-dollar-k6-{suffix}"
    probe = (
        ProcessProbe(args.app_pid)
        if args.app_pid
        else (CgroupProbe(args.app_exec) if args.app_exec else None)
    )
    profiler = Profiler(args.pprof, args.pprof_exec) if args.pprof else None
    # Exec mode: one k6 container per prefix, each running a share of the users.
    remotes = [shlex.split(prefix) for prefix in args.k6_exec or []]
    workload_sha = sha256(WORKLOAD)
    state = {
        "status": "starting",
        "runs": [],
        "confirmed_vus": None,
        "environment": {
            "mode": args.mode,
            "url": args.url,
            "k6_url": args.k6_url or args.url,
            "k6": {"docker": K6_IMAGE, "exec": args.k6_exec}.get(args.k6, args.k6_bin),
            "k6_instances": max(1, len(remotes)),
            "app_probe": probe.source if probe else None,
            "host": platform.platform(),
            "host_logical_cpus": os.cpu_count(),
            "workload_sha256": workload_sha,
            "settings": {
                key: str(value) if isinstance(value, Path) else value
                for key, value in vars(args).items()
            },
        },
    }
    report(output, state)
    k6_env = {**os.environ, "NO_PROXY": "*", "no_proxy": "*"}
    exit_code = 0

    def interrupted(_signum, _frame):
        raise KeyboardInterrupt

    previous_handlers = {
        sig: signal.signal(sig, interrupted)
        for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)
    }
    try:
        if args.k6 == "local":
            state["environment"]["k6"] = execute([args.k6_bin, "version"]).strip()
            # k6 holds about one connection per user; too few descriptors would look like a capacity limit.
            files = resource.getrlimit(resource.RLIMIT_NOFILE)[0]
            needed = max(args.warmup_vus, args.max_vus) + 1024
            if files != resource.RLIM_INFINITY and files < needed:
                raise ExecutionError(
                    f"Open-file limit {files} is below {needed}; raise it or lower the user cap"
                )
        elif args.k6 == "exec":
            # Each container brings its own copy of the workload; it must be this one.
            for remote in remotes:
                remote_sha = execute(
                    [*remote, "sha256sum", str(REMOTE / "load.js")]
                ).split()
                if remote_sha[:1] != [workload_sha]:
                    raise ExecutionError(
                        "A k6 container's load.js differs from bench/load.js"
                    )
            state["environment"]["k6"] = execute([*remotes[0], "k6", "version"]).strip()
        else:
            try:
                execute(["docker", "image", "inspect", K6_IMAGE])
            except ExecutionError:
                execute(["docker", "pull", "--quiet", K6_IMAGE], timeout=600)
        print(f"Waiting for {args.url}/health", flush=True)
        wait_healthy(args.url, args.wait_seconds)
        if args.app_limits is not None:
            state["environment"]["app_limits"] = args.app_limits
        elif isinstance(probe, CgroupProbe):
            state["environment"]["app_limits"] = probe.limits()
        deadline = time.monotonic() + args.budget_seconds
        state["status"] = "running"

        def run(phase, vus):
            hold = {"warmup": args.warmup_seconds, "confirm": args.confirm_seconds}.get(
                phase, args.probe_seconds
            )
            # A k6 whose kubectl exec stream dropped may still be sending load;
            # stopping it closes its connections, so wait for their ports after.
            for remote in remotes:
                execute([*remote, "sh", "-c", "pkill -x k6; true"])
                wait_for_ports(remote)
            needed = (
                args.ramp_seconds + hold + RAMP_DOWN_SECONDS + RUN_ALLOWANCE_SECONDS
            )
            if deadline - time.monotonic() < needed:
                raise BudgetExpired("The time budget cannot fit the next complete run")
            identifier = len(state["runs"]) + 1
            stem = f"run-{identifier:03d}"
            # One k6 process per container, each with its own output files.
            stems = (
                [f"{stem}.instance-{shard}" for shard in range(len(remotes))]
                if len(remotes) > 1
                else [stem]
            )
            item = {
                "id": identifier,
                "phase": phase,
                "vus": vus,
                "hold_seconds": hold,
                "started_at": now(),
                "status": "running",
                "log": f"{stem}.log",
            }
            state["runs"].append(item)
            report(output, state)
            print(f"Run {identifier}: {phase}, {vus} users, {hold}s hold", flush=True)
            started = time.monotonic()
            sampler = Sampler(probe) if probe else None
            if profiler:
                profiler.start(
                    args.ramp_seconds, hold, output / "pprof" / f"{stem}.cpu.pb.gz"
                )
            try:
                if sha256(WORKLOAD) != workload_sha:
                    raise ExecutionError("The workload changed during the search")
                codes = execute_all(
                    [
                        k6_command(
                            args, output, container, vus, hold, shard_stem, shard
                        )
                        for shard, shard_stem in enumerate(stems)
                    ],
                    output / item["log"],
                    timeout=max(1, deadline - time.monotonic()),
                    # Exec mode runs kubectl, which may need a proxy; k6 then runs remotely.
                    env=None if args.k6 == "exec" else k6_env,
                )
                item["exit_code"] = codes[0] if len(codes) == 1 else codes
                if any(code in (137, -9) for code in codes):
                    raise ExecutionError(
                        "k6 was killed (exit 137), usually because it ran out of memory. "
                        "Give k6 more memory (BENCH_K6=local in Docker mode; K8S_K6_MEMORY "
                        "or K8S_K6_PODS in k8s mode) or lower BENCH_MAX_VUS."
                    )
                parts = []
                for shard, (shard_stem, code) in enumerate(zip(stems, codes)):
                    if remotes:
                        for name in (
                            f"{shard_stem}.json",
                            f"{shard_stem}.events.jsonl",
                        ):
                            path = REMOTE / "results" / name
                            try:
                                text = execute(
                                    [*remotes[shard], "cat", str(path)], timeout=600
                                )
                            except ExecutionError:
                                continue  # The checks below report missing output.
                            (output / "k6" / name).write_text(text, encoding="utf-8")
                    validate_events(output / "k6" / f"{shard_stem}.events.jsonl", code)
                    summary = output / "k6" / f"{shard_stem}.json"
                    if not summary.is_file():
                        raise ExecutionError(f"k6 exited {code} without a summary")
                    parts.append(
                        parse_summary(
                            json.loads(summary.read_bytes()),
                            code,
                            minimum_seconds=args.ramp_seconds
                            + hold
                            + RAMP_DOWN_SECONDS,
                        )
                    )
                events = [f"k6/{shard_stem}.events.jsonl" for shard_stem in stems]
                summaries = [f"k6/{shard_stem}.json" for shard_stem in stems]
                item["events"] = events[0] if len(stems) == 1 else events
                item["summary"] = summaries[0] if len(stems) == 1 else summaries
                item.update(merge(parts))
                if item["reached_vus"] != vus:
                    raise ExecutionError(
                        "k6 did not reach the requested virtual-user count"
                    )
                item["resources"] = (
                    sampler.finish()
                    if sampler
                    else {"unavailable": "No app probe configured"}
                )
                if not healthy(args.url):
                    raise ExecutionError("The app is unhealthy after the run")
            except KeyboardInterrupt:
                item.update(status="interrupted", error="Interrupted during this run")
                raise
            except subprocess.TimeoutExpired as error:
                item.update(
                    status="budget exhausted",
                    error="The run exceeded the remaining search budget",
                )
                raise BudgetExpired(item["error"]) from error
            except Exception as error:
                message = (
                    str(error)
                    if isinstance(error, ExecutionError)
                    else f"{type(error).__name__}: {error}"
                )
                item.update(status="execution error", error=message)
                raise ExecutionError(message) from error
            finally:
                if sampler:
                    sampler.stopped.set()
                if profiler:
                    saved = profiler.stop(output / "pprof" / f"{stem}.heap.pb.gz")
                    for kind, ok in zip(("cpu", "heap"), saved):
                        if ok:
                            item[f"{kind}_profile"] = f"pprof/{stem}.{kind}.pb.gz"
                        else:
                            print(f"  The {kind} profile was not saved", flush=True)
                item["wall_seconds"] = time.monotonic() - started
                report(output, state)
            metrics = item["metrics"]
            print(
                f"  {item['status']}: p95={metrics['p95_ms']:.2f}ms, "
                f"p99={metrics['p99_ms']:.2f}ms, errors={metrics['error_percent']:.3f}%",
                flush=True,
            )
            return item["status"] == "passed"

        run("warmup", args.warmup_vus)
        confirmed, capped = search(
            run, args.start_vus, args.max_vus, args.step, args.confirm_runs
        )
        state["confirmed_vus"] = confirmed
        if capped:
            state["status"] = "capped"
            state["message"] = (
                "User cap reached without a failing probe; the result is a lower bound."
            )
        elif confirmed:
            state.update(
                status="completed", message="Search and confirmation finished."
            )
        else:
            state.update(
                status="no passing candidate",
                message="No candidate passed confirmation.",
            )
            exit_code = 1
        if args.confirm_seconds < 300:
            state["message"] += (
                " Confirmation was shorter than 5 minutes; this is a smoke test."
            )
        if args.ramp_seconds != 60:
            state["message"] += (
                " Custom ramp duration; not comparable to the default workload."
            )
    except BudgetExpired as error:
        state.update(status="budget exhausted", message=str(error))
        exit_code = 2
    except KeyboardInterrupt:
        state.update(
            status="interrupted", message="Interrupted. Completed runs are retained."
        )
        exit_code = 130
    except Exception as error:  # noqa: BLE001 - every failure must still produce a report
        if not isinstance(error, ExecutionError):
            (output / "traceback.txt").write_text(
                traceback.format_exc(), encoding="utf-8"
            )
        state.update(status="execution error", message=str(error))
        exit_code = 2
    finally:
        for sig in previous_handlers:
            signal.signal(sig, signal.SIG_IGN)
        if args.k6 == "docker":
            subprocess.run(
                ["docker", "rm", "--force", container], capture_output=True, check=False
            )
        report(output, state)
        for sig, handler in previous_handlers.items():
            signal.signal(sig, handler)
    print(
        f"Result: {state['status']}. Reports: {output / 'report.md'} and {output / 'report.html'}",
        flush=True,
    )
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
