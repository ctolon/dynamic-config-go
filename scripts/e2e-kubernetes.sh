#!/usr/bin/env bash
# End-to-end proof that a ConfigMap edit reaches a running pod.
#
# Synthetic filesystem tests can show that a symlink swap is noticed. Only a
# cluster can show that the whole chain works: kubectl, the API server, the
# kubelet's sync, the projected volume it writes, the watcher inside the
# container, and the reload that follows — with the pod never restarting.
#
#   scripts/e2e-kubernetes.sh
#
# Needs docker, kind and kubectl. Creates a throwaway cluster and deletes it
# on the way out, including on failure.
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER="${CLUSTER:-dynamic-config-go-e2e}"
IMAGE="${IMAGE:-dynamic-config-go-example:e2e}"
NAMESPACE=default

# The kubelet republishes a projected volume on its sync period, which is a
# minute by default. This is the ceiling, not the expectation.
TIMEOUT="${TIMEOUT:-180}"

say() { printf '\n\033[1m── %s\033[0m\n' "$*"; }

for tool in docker kind kubectl; do
    command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required" >&2; exit 1; }
done

cleanup() {
    local status=$?

    if [ "${KEEP_CLUSTER:-0}" != "1" ]; then
        say "deleting the cluster"
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    fi

    exit "$status"
}

trap cleanup EXIT

say "creating the cluster"
kind create cluster --name "$CLUSTER" --wait 120s

say "building the image"
docker build -f examples/kubernetes/Dockerfile -t "$IMAGE" .

say "loading the image into the cluster"
kind load docker-image "$IMAGE" --name "$CLUSTER"

say "deploying"
kubectl apply -f examples/kubernetes/manifests/

# The manifests name a registry image; point the deployment at the one just
# built, and make sure the node never tries to pull it.
kubectl set image deployment/example "example=$IMAGE"
kubectl patch deployment example --type=json \
    -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'

kubectl rollout status deployment/example --timeout=120s

POD=$(kubectl get pod -l app=example -o jsonpath='{.items[0].metadata.name}')
UID_BEFORE=$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}')
RESTARTS_BEFORE=$(kubectl get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')

say "pod $POD is running (uid ${UID_BEFORE:0:8}…, $RESTARTS_BEFORE restarts)"

say "waiting for the initial configuration in the log"
kubectl wait --for=condition=Ready "pod/$POD" --timeout=60s

if ! kubectl logs "$POD" | grep -q '"message":"hello from the ConfigMap"'; then
    echo "the pod is not serving the ConfigMap's message" >&2
    kubectl logs "$POD" >&2
    exit 1
fi

say "editing the ConfigMap"
kubectl patch configmap example-config --type=merge -p "$(cat <<'JSON'
{"data":{"config.yaml":"message: reloaded without a restart\nlog_level: debug\nfeatures:\n  beta: true\n"}}
JSON
)"

say "waiting up to ${TIMEOUT}s for the pod to notice (the kubelet's sync period, not the library's latency)"

deadline=$((SECONDS + TIMEOUT))
noticed=0

while [ "$SECONDS" -lt "$deadline" ]; do
    if kubectl logs "$POD" | grep -q 'configuration reloaded from the mounted volume'; then
        noticed=1
        break
    fi

    sleep 5
done

if [ "$noticed" != 1 ]; then
    echo "the pod never reloaded within ${TIMEOUT}s" >&2
    kubectl logs "$POD" >&2
    kubectl exec "$POD" -- ls -la /etc/example >&2 2>/dev/null || true
    exit 1
fi

say "checking that it is serving the new configuration"

# The example reports what it is working on every ten seconds, so the proof
# that the new snapshot is in use is the next one of those lines — not the
# reload log line, which only says a reload happened.
deadline=$((SECONDS + 60))
serving=0

while [ "$SECONDS" -lt "$deadline" ]; do
    if kubectl logs "$POD" | grep -q '"msg":"working".*"message":"reloaded without a restart"'; then
        serving=1
        break
    fi

    sleep 5
done

if [ "$serving" != 1 ]; then
    echo "the pod reloaded but never served the new message" >&2
    kubectl logs "$POD" >&2
    exit 1
fi

say "checking that nothing restarted"

UID_AFTER=$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}')
RESTARTS_AFTER=$(kubectl get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')

if [ "$UID_BEFORE" != "$UID_AFTER" ]; then
    echo "the pod was replaced: $UID_BEFORE -> $UID_AFTER" >&2
    exit 1
fi

if [ "$RESTARTS_BEFORE" != "$RESTARTS_AFTER" ]; then
    echo "the container restarted: $RESTARTS_BEFORE -> $RESTARTS_AFTER" >&2
    exit 1
fi

say "checking that a bad ConfigMap is refused rather than adopted"

kubectl patch configmap example-config --type=merge -p "$(cat <<'JSON'
{"data":{"config.yaml":"message: \"\"\nlog_level: nonsense\n"}}
JSON
)"

deadline=$((SECONDS + TIMEOUT))
rejected=0

while [ "$SECONDS" -lt "$deadline" ]; do
    if kubectl logs "$POD" | grep -q 'configuration reload rejected'; then
        rejected=1
        break
    fi

    sleep 5
done

if [ "$rejected" != 1 ]; then
    echo "an invalid ConfigMap was not reported as rejected within ${TIMEOUT}s" >&2
    kubectl logs "$POD" >&2
    exit 1
fi

if ! kubectl get pod "$POD" -o jsonpath='{.status.phase}' | grep -q Running; then
    echo "the pod did not survive an invalid ConfigMap" >&2
    exit 1
fi

RESTARTS_FINAL=$(kubectl get pod "$POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')

if [ "$RESTARTS_BEFORE" != "$RESTARTS_FINAL" ]; then
    echo "the container restarted on an invalid ConfigMap: $RESTARTS_BEFORE -> $RESTARTS_FINAL" >&2
    exit 1
fi

say "passed"

cat <<EOF

  ConfigMap edited        → pod reloaded, no restart, same uid
  ConfigMap made invalid  → reload rejected, pod still serving the last good one

EOF
