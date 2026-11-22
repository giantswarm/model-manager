#!/usr/bin/env bash
set -euo pipefail

kube_context="${1:?pass the explicit Kubernetes context for Helm}"
repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "${HOME}/.cache/tmp"
scratch_directory="$(mktemp -d "${HOME}/.cache/tmp/model-manager-chart.XXXXXX")"
trap 'rm -rf -- "$scratch_directory"' EXIT

render() {
  helm --kube-context "$kube_context" template model-manager \
    "$repository_root/helm/model-manager" --namespace agent-platform "$@" |
    yq -o=json -I=0 '.' | jq -s '.'
}

render --set serviceMonitor.enabled=true > "$scratch_directory/defaults.json"
jq -e '
  [.[] | select(.kind == "NetworkPolicy" and .metadata.name == "model-manager")][0].spec.ingress as $ingress |
  ($ingress[0].ports[0].port == 8080) and
  ($ingress[0].from | length == 2) and
  ($ingress[0].from[0].podSelector.matchLabels["app.kubernetes.io/name"] == "muster") and
  ($ingress[0].from[1].podSelector.matchLabels["app.kubernetes.io/name"] == "agentgateway") and
  ($ingress[1].from[0].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "kube-system") and
  ($ingress[1].from[0].podSelector.matchLabels["app.kubernetes.io/instance"] == "alloy-metrics") and
  ([.[] | select(.kind == "ServiceMonitor")][0].metadata.labels["observability.giantswarm.io/tenant"] == "giantswarm")
' "$scratch_directory/defaults.json" > /dev/null

render --set networkPolicy.enabled=false > "$scratch_directory/local.json"
jq -e 'all(.[]; .kind != "NetworkPolicy")' "$scratch_directory/local.json" > /dev/null

render --set 'networkPolicy.ingressNamespaces[0]=legacy-gateway' \
  --set 'networkPolicy.metricsNamespaces[0]=legacy-metrics' > "$scratch_directory/overlap.json"
jq -e '
  [.[] | select(.kind == "NetworkPolicy" and .metadata.name == "model-manager")][0].spec.ingress as $ingress |
  ($ingress[0].from[2].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "legacy-gateway") and
  ($ingress[1].from[1].namespaceSelector.matchLabels["kubernetes.io/metadata.name"] == "legacy-metrics")
' "$scratch_directory/overlap.json" > /dev/null

if render --set serviceMonitor.enabled=true \
  --set 'serviceMonitor.labels.observability\.giantswarm\.io/tenant=' \
  > "$scratch_directory/missing-tenant.json" 2> "$scratch_directory/missing-tenant.log"; then
  echo 'ServiceMonitor accepted an empty tenant' >&2
  exit 1
fi
rg -q 'must be set' "$scratch_directory/missing-tenant.log"
