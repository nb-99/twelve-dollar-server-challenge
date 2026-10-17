#!/usr/bin/env bash
# Run the challenge suite or benchmark in the current kubectl context.
#   scripts/k8s.sh test
#   scripts/k8s.sh benchmark [benchmark.py options]
# Builds the app and seed images for the cluster's node architectures, pushes them
# to $K8S_REGISTRY, and applies k8s/ pinned to those digests in $K8S_NAMESPACE.
# Its pods are deleted afterwards; the namespace and the optional image pull secret
# $K8S_PULL_SECRET are provided by the user and stay.
set -euo pipefail
action=${1:?usage: k8s.sh test|benchmark [benchmark.py options]}
shift
here=$(cd "$(dirname "$0")/.." && pwd)
root=$(cd "$here/../.." && pwd)
: "${K8S_REGISTRY:?Set K8S_REGISTRY to the image repository prefix, e.g. registry.example.com/owner}"
ns=${K8S_NAMESPACE:-twelve-dollar-challenge}
pull_secret=${K8S_PULL_SECRET:-}
selector=app.kubernetes.io/part-of=twelve-dollar-go
# Pin the context so switching contexts during a run cannot redirect it.
context=$(kubectl config current-context)
kube=(kubectl --context "$context" -n "$ns")
work=$(mktemp -d "${TMPDIR:-/tmp}/twelve-dollar-go.XXXXXX")
pids=()
child=
deployed=false
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$here/scripts/lib.sh"

cleanup() {
  local status=$?
  trap - EXIT
  for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
  if [ "$deployed" = true ] &&
    ! "${kube[@]}" delete statefulset,pod,service -l "$selector" --wait >"$work/cleanup.log" 2>&1; then
    cat "$work/cleanup.log" >&2
    status=2
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
# Bash runs traps only after a foreground command exits, so the benchmark runs in
# the background. Ctrl-C already reaches it; SIGTERM sent to make must be forwarded.
stop() {
  if [ -n "$child" ]; then
    [ "$1" = INT ] || kill -TERM "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
  fi
  exit "$2"
}
trap 'stop INT 130' INT
trap 'stop TERM 143' TERM

need curl
need docker
need python3
echo "Context $context, namespace $ns"
if [ -n "$pull_secret" ] && ! "${kube[@]}" get secret "$pull_secret" -o name >/dev/null 2>&1; then
  echo "Namespace $ns has no image pull secret $pull_secret." >&2
  exit 1
fi

platforms=$(kubectl --context "$context" get nodes \
  -o jsonpath='{range .items[*]}linux/{.status.nodeInfo.architecture}{"\n"}{end}' | sort -u | paste -sd, -)
mkdir "$work/k8s"
cp -R "$here/deployments/k8s" "$work/k8s/base"
{
  echo "resources: [base]"
  echo "namespace: $ns"
  echo "images:"
} >"$work/k8s/kustomization.yaml"
for image in runtime:app checks:seed; do
  name=twelve-dollar-go-${image#*:}
  echo "Building $name for $platforms and pushing it to $K8S_REGISTRY"
  docker buildx build --platform "$platforms" --target "${image%%:*}" \
    -f "$here/deployments/docker/Dockerfile" -t "$K8S_REGISTRY/$name:latest" --push \
    --metadata-file "$work/$name.json" "$root"
  digest=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' \
    "$work/$name.json")
  printf '  - name: %s\n    newName: %s\n    digest: %s\n' "$name" "$K8S_REGISTRY/$name" "$digest" \
    >>"$work/k8s/kustomization.yaml"
done
echo "patches:" >>"$work/k8s/kustomization.yaml"
# Adds a JSON patch for the app pod and the k6 pod template. The arguments are
# patch lines in which @SPEC@ stands for the pod spec's path in each kind.
patch_pods() {
  local kind prefix
  for kind in Pod:/spec StatefulSet:/spec/template/spec; do
    prefix=${kind#*:}
    printf '%s\n' "  - target: {kind: ${kind%%:*}}" '    patch: |-' \
      "${@//@SPEC@/$prefix}" >>"$work/k8s/kustomization.yaml"
  done
}
if [ -n "$pull_secret" ]; then
  patch_pods '      - op: add' '        path: @SPEC@/imagePullSecrets' \
    "        value: [{name: $pull_secret}]"
fi
if [ "$action" = test ]; then
  # The challenge suite runs on this host and needs no load generator.
  # shellcheck disable=SC2016 # $patch is a literal kustomize directive.
  printf '%s\n' '  - patch: |-' '      $patch: delete' '      apiVersion: apps/v1' \
    '      kind: StatefulSet' '      metadata:' '        name: k6' >>"$work/k8s/kustomization.yaml"
else
  k6_patch=()
  if [ -n "${K8S_K6_PODS:-}" ]; then
    if ! [[ $K8S_K6_PODS =~ ^[1-9][0-9]*$ ]]; then
      echo "K8S_K6_PODS must be a positive integer." >&2
      exit 2
    fi
    k6_patch+=('      - op: replace' '        path: /spec/replicas' "        value: $K8S_K6_PODS"
      '      - op: replace' '        path: /spec/template/spec/initContainers/0/env/1/value'
      "        value: \"$K8S_K6_PODS\"")
  fi
  if [ -n "${K8S_K6_CPU_REQUEST:-}" ]; then
    k6_patch+=('      - op: replace'
      '        path: /spec/template/spec/containers/0/resources/requests/cpu'
      "        value: \"$K8S_K6_CPU_REQUEST\"")
  fi
  if [ -n "${K8S_K6_CPU:-}" ]; then
    k6_patch+=('      - op: replace'
      '        path: /spec/template/spec/containers/0/resources/limits/cpu'
      "        value: \"$K8S_K6_CPU\"")
  fi
  if [ -n "${K8S_K6_MEMORY:-}" ]; then
    for kind in requests limits; do
      k6_patch+=('      - op: replace'
        "        path: /spec/template/spec/containers/0/resources/$kind/memory"
        "        value: \"$K8S_K6_MEMORY\"")
    done
  fi
  if [ ${#k6_patch[@]} -gt 0 ]; then
    printf '%s\n' '  - target: {kind: StatefulSet, name: k6}' '    patch: |-' "${k6_patch[@]}" \
      >>"$work/k8s/kustomization.yaml"
  fi
fi
if [ -n "${K8S_NODE:-}" ] && [ -n "${K8S_APP_NODE:-}" ]; then
  echo "Set K8S_NODE or K8S_APP_NODE, not both." >&2
  exit 2
fi
if [ -n "${K8S_NODE:-}" ]; then
  # One node runs every pod, so the anti-affinity that separates them goes.
  patch_pods '      - op: remove' '        path: @SPEC@/affinity' '      - op: add' \
    '        path: @SPEC@/nodeSelector' "        value: {kubernetes.io/hostname: \"$K8S_NODE\"}"
fi
if [ -n "${K8S_APP_NODE:-}" ]; then
  # Only the app is pinned; the k6 pods' anti-affinity keeps them off its node.
  printf '%s\n' '  - target: {kind: Pod, name: server}' '    patch: |-' '      - op: add' \
    '        path: /spec/nodeSelector' "        value: {kubernetes.io/hostname: \"$K8S_APP_NODE\"}" \
    >>"$work/k8s/kustomization.yaml"
fi

# Every run starts from fresh pods, so the database is a fresh seed copy.
deployed=true
"${kube[@]}" delete statefulset,pod,service -l "$selector" --wait
"${kube[@]}" apply -k "$work/k8s"
pods=(pod/server)
k6_pods=0
if [ "$action" != test ]; then
  k6_pods=$("${kube[@]}" get statefulset k6 -o jsonpath='{.spec.replicas}')
  echo "k6: $k6_pods pods, each requesting $("${kube[@]}" get statefulset k6 \
    -o jsonpath='{.spec.template.spec.containers[0].resources.requests}')"
  for ((i = 0; i < k6_pods; i++)); do pods+=("pod/k6-$i"); done
fi
echo "Waiting for image pulls, seeding, and readiness"
deadline=$((SECONDS + 600))
until "${kube[@]}" wait --for=condition=Ready "${pods[@]}" --timeout=5s >/dev/null 2>&1; do
  # Look for failed containers, and for pods no node has room for.
  states=$("${kube[@]}" get pods -l "$selector" -o jsonpath='{range .items[*]}{range .status.initContainerStatuses[*]}{.state.terminated.exitCode} {.state.waiting.reason} {end}{range .status.containerStatuses[*]}{.state.terminated.exitCode} {.state.waiting.reason} {end}{.status.conditions[?(@.type=="PodScheduled")].reason} {end}')
  if [[ $states =~ [1-9]|Err|BackOff|Unschedulable ]] || [ "$SECONDS" -ge "$deadline" ]; then
    echo "The pods did not become ready." >&2
    "${kube[@]}" get pods -o wide >&2
    "${kube[@]}" get pods -l "$selector" -o jsonpath='{range .items[*]}{.metadata.name}: {.status.conditions[?(@.type=="PodScheduled")].message}{"\n"}{end}' >&2
    for pod in "${pods[@]}"; do
      "${kube[@]}" logs "$pod" --all-containers --tail=40 >&2 || true
    done
    exit 1
  fi
done

# The port-forward only carries health checks and the challenge suite; k6 runs in the cluster.
port=$(free_port)
"${kube[@]}" port-forward pod/server "$port:80" >"$work/port-forward.log" 2>&1 &
pids+=("$!")
url=http://127.0.0.1:$port
if ! wait_healthy "$url" "$!"; then
  cat "$work/port-forward.log" >&2
  exit 1
fi

case "$action" in
  test)
    need jq
    need openssl
    NO_PROXY='*' no_proxy='*' bash "$root/test/test.sh" "$url"
    ;;
  benchmark)
    printf -v exec_prefix 'kubectl --context %q -n %q exec' "$context" "$ns"
    options=(--mode k8s --url "$url" --k6 exec --k6-url http://server
      --app-exec "$exec_prefix server -c app --"
      --app-limits "$("${kube[@]}" get pod server -o jsonpath='{.spec.resources.limits}')")
    for ((i = 0; i < k6_pods; i++)); do options+=(--k6-exec "$exec_prefix k6-$i -c k6 --"); done
    if [ "${PROFILE:-false}" = true ]; then
      options+=(--pprof 127.0.0.1:6060 --pprof-exec "$exec_prefix server -c app --")
    fi
    python3 "$here/tools/benchmark/benchmark.py" "${options[@]}" "$@" &
    child=$!
    wait "$child"
    ;;
  *)
    echo "Unknown action: $action" >&2
    exit 2
    ;;
esac
