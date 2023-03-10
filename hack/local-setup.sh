#!/bin/bash

#
# Copyright 2022 Red Hat, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

LOCAL_SETUP_DIR="$(dirname "${BASH_SOURCE[0]}")"
source "${LOCAL_SETUP_DIR}"/.setupEnv
source "${LOCAL_SETUP_DIR}"/.kindUtils
source "${LOCAL_SETUP_DIR}"/.clusterUtils
source "${LOCAL_SETUP_DIR}"/.argocdUtils
source "${LOCAL_SETUP_DIR}"/.cleanupUtils

KIND_CLUSTER_PREFIX="mctc-"
KIND_CLUSTER_CONTROL_PLANE="${KIND_CLUSTER_PREFIX}control-plane"
KIND_CLUSTER_WORKLOAD="${KIND_CLUSTER_PREFIX}workload"

INGRESS_NGINX_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/ingress-nginx
METALLB_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/metallb
CERT_MANAGER_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/cert-manager
EXTERNAL_DNS_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/external-dns
ARGOCD_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/argocd
ISTIO_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/istio/istio-operator.yaml
GATEWAY_API_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/gateway-api
REDIS_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/redis
LIMITADOR_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/limitador

WEBHOOK_PATH=${LOCAL_SETUP_DIR}/../config/webhook-setup/workload
TLS_CERT_PATH=${LOCAL_SETUP_DIR}/../config/webhook-setup/control/tls

set -e pipefail

deployIngressController () {
  clusterName=${1}
  kubectl config use-context kind-${clusterName}
  echo "Deploying Ingress controller to ${clusterName}"
  ${KUSTOMIZE_BIN} build ${INGRESS_NGINX_KUSTOMIZATION_DIR} --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
  echo "Waiting for deployments to be ready ..."
  kubectl -n ingress-nginx wait --timeout=300s --for=condition=Available deployments --all
}

deployMetalLB () {
  clusterName=${1}
  metalLBSubnet=${2}

  kubectl config use-context kind-${clusterName}
  echo "Deploying MetalLB to ${clusterName}"
  ${KUSTOMIZE_BIN} build ${METALLB_KUSTOMIZATION_DIR} | kubectl apply -f -
  echo "Waiting for deployments to be ready ..."
  kubectl -n metallb-system wait --for=condition=ready pod --selector=app=metallb --timeout=90s
  echo "Creating MetalLB AddressPool"
  cat <<EOF | kubectl apply -f -
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: example
  namespace: metallb-system
spec:
  addresses:
  - 172.32.${metalLBSubnet}.0/24
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: empty
  namespace: metallb-system
EOF
}

deployCertManager() {
  clusterName=${1}
  echo "Deploying Cert Manager to (${clusterName})"

  kubectl config use-context kind-${clusterName}

  ${KUSTOMIZE_BIN} build ${CERT_MANAGER_KUSTOMIZATION_DIR} --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
  echo "Waiting for Cert Manager deployments to be ready..."
  kubectl -n cert-manager wait --timeout=300s --for=condition=Available deployments --all

  kubectl delete validatingWebhookConfiguration mctc-cert-manager-webhook
  kubectl delete mutatingWebhookConfiguration mctc-cert-manager-webhook
  # Apply the default glbc-ca issuer
  kubectl create -n cert-manager -f ./config/default/issuer.yaml
}

deployExternalDNS() {
  clusterName=${1}
  echo "Deploying ExternalDNS to (${clusterName})"

  kubectl config use-context kind-${clusterName}

  ${KUSTOMIZE_BIN} build ${EXTERNAL_DNS_KUSTOMIZATION_DIR} --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
  echo "Waiting for External DNS deployments to be ready..."
  kubectl -n external-dns wait --timeout=300s --for=condition=Available deployments --all
}

deployArgoCD() {
  clusterName=${1}
  echo "Deploying ArgoCD to (${clusterName})"

  kubectl config use-context kind-${clusterName}

  ${KUSTOMIZE_BIN} build ${ARGOCD_KUSTOMIZATION_DIR} --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
  echo "Waiting for ARGOCD deployments to be ready..."
  kubectl -n argocd wait --timeout=300s --for=condition=Available deployments --all

  ports=$(docker ps --format '{{json .}}' | jq "select(.Names == \"$clusterName-control-plane\").Ports")
  httpsport=$(echo $ports | sed -e 's/.*0.0.0.0\:\(.*\)->443\/tcp.*/\1/')
  argoPassword=$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" | base64 -d)
  nodeIP=$(kubectl get nodes -o json | jq -r ".items[] | select(.metadata.name == \"$clusterName-control-plane\").status | .addresses[] | select(.type == \"InternalIP\").address")

  echo -ne "\n\n\tConnect to ArgoCD UI\n\n"
  echo -ne "\t\tLocal URL: https://argocd.127.0.0.1.nip.io:$httpsport\n"
  echo -ne "\t\tNode URL : https://argocd.$nodeIP.nip.io\n"
  echo -ne "\t\tUser     : admin\n"
  echo -ne "\t\tPassword : $argoPassword\n\n\n"
}

deployIstio() {
  clusterName=${1}
  echo "Deploying Istio to (${clusterName})"

  kubectl config use-context kind-${clusterName}
  ${ISTIOCTL_BIN} operator init
	kubectl apply -f  ${ISTIO_KUSTOMIZATION_DIR}
}

installGatewayAPI() {
  clusterName=${1}
  kubectl config use-context kind-${clusterName}
  echo "Installing Gateway API in ${clusterName}"

  ${KUSTOMIZE_BIN} build ${GATEWAY_API_KUSTOMIZATION_DIR} | kubectl apply -f -
}

deployKuadrant(){
  clusterName=${1}
  kubectl config use-context kind-${clusterName}

  echo "Installing Kuadrant in ${clusterName}"
  ${KUSTOMIZE_BIN} build config/kuadrant | kubectl apply -f -
}

deployRedis(){
  clusterName=${1}
  kubectl config use-context kind-${clusterName}
  
  echo "Installing Redis in ${clusterName}"
  ${KUSTOMIZE_BIN} build ${REDIS_KUSTOMIZATION_DIR} | kubectl apply -f - 
}

deployLimitador(){
  clusterName=${1}
  kubectl config use-context kind-${clusterName}

  echo "Installing Limitador in ${clusterName}"
  ${KUSTOMIZE_BIN} build ${LIMITADOR_KUSTOMIZATION_DIR} | kubectl apply -f -
}

deployDashboard() {
  clusterName=${1}
  portOffset=${2}

  echo "Deploying Kubernetes Dashboard to (${clusterName})"

  kubectl config use-context kind-${clusterName}

  kubectl apply -f https://raw.githubusercontent.com/kubernetes/dashboard/v2.7.0/aio/deploy/recommended.yaml
  ${KUSTOMIZE_BIN} build config/dashboard | kubectl apply -f -
  token=$(kubectl get secret/admin-user-token -n kubernetes-dashboard -o go-template="{{.data.token | base64decode}}")

  port=$((proxyPort + portOffset))

  kubectl proxy --context kind-${clusterName} --port ${port} &
  proxyPID=$!
  echo $proxyPID >> /tmp/dashboard_pids

  echo -ne "\n\n\tAccess Kubernetes Dashboard\n\n"
  echo -ne "\t\t\t* The dashboard is available at http://localhost:$port/api/v1/namespaces/kubernetes-dashboard/services/https:kubernetes-dashboard:/proxy/\n"
  echo -ne "\t\tAccess the dashboard using the following Bearer Token: $token\n"
}

deployWebhookConfigs(){
  clusterName=${1}
  echo "Deploying the webhook configuration to (${clusterName})"

  kubectl config use-context kind-${clusterName}

  kubectl apply -f $WEBHOOK_PATH/webhook-configs.yaml
}

deployAgentSecret() {
  clusterName=${1}
  localAccess=${2:=LOCAL_ACCESS}
  if [ $localAccess == "true" ]; then
    secretName=control-plane-cluster
  else
    secretName=control-plane-cluster-internal
  fi
  echo "Deploying the agent secret to (${clusterName})"

  kubectl config use-context kind-${clusterName}

  kubectl create namespace mctc-system || true

  makeSecretForCluster $KIND_CLUSTER_CONTROL_PLANE $(kubectl config current-context) $localAccess |
  setNamespacedName mctc-system ${secretName} |
  setLabel argocd.argoproj.io/secret-type cluster |
  kubectl apply -f -
}

initController() {
    clusterName=${1}
    kubectl config use-context kind-${clusterName}
    echo "Initialize local dev setup for the controller on ${clusterName}"

    # Add the mctc CRDs
    ${KUSTOMIZE_BIN} build config/crd | kubectl apply -f -
    # Create the mctc ns and dev managed zone
    ${KUSTOMIZE_BIN} --reorder none --load-restrictor LoadRestrictionsNone build config/local-setup/controller | kubectl apply -f -
    # Create the local dev webhook proxy
    ${KUSTOMIZE_BIN} --load-restrictor LoadRestrictionsNone build config/webhook-setup/proxy | kubectl apply -f -
}

cleanup

port80=9090
port443=8445
proxyPort=9200
metalLBSubnetStart=200

# Create network for the clusters
docker network create -d bridge --subnet 172.32.0.0/16 mctc --gateway 172.32.0.1 \
  -o "com.docker.network.bridge.enable_ip_masquerade"="true" \
  -o "com.docker.network.driver.mtu"="1500"

#1. Create Kind control plane cluster
kindCreateCluster ${KIND_CLUSTER_CONTROL_PLANE} ${port80} ${port443}

#2. Install the Gateway API CRDs in the control cluster
installGatewayAPI ${KIND_CLUSTER_CONTROL_PLANE}

#3. Deploy ingress controller
deployIngressController ${KIND_CLUSTER_CONTROL_PLANE}

#4. Deploy cert manager
deployCertManager ${KIND_CLUSTER_CONTROL_PLANE}

#5. Deploy redis
deployRedis ${KIND_CLUSTER_CONTROL_PLANE}

#5. Deploy argo cd
deployArgoCD ${KIND_CLUSTER_CONTROL_PLANE}

#6. Deploy Dashboard
deployDashboard $KIND_CLUSTER_CONTROL_PLANE 0

#7. Add the control plane cluster
argocdAddCluster ${KIND_CLUSTER_CONTROL_PLANE} ${KIND_CLUSTER_CONTROL_PLANE}

#8. Initialize local dev setup for the controller on the control-plane cluster
initController ${KIND_CLUSTER_CONTROL_PLANE}

#9. Add workload clusters if MCTC_WORKLOAD_CLUSTERS_COUNT environment variable is set
if [[ -n "${MCTC_WORKLOAD_CLUSTERS_COUNT}" ]]; then
  for ((i = 1; i <= ${MCTC_WORKLOAD_CLUSTERS_COUNT}; i++)); do
    kindCreateCluster ${KIND_CLUSTER_WORKLOAD}-${i} $((${port80} + ${i})) $((${port443} + ${i}))
    deployIstio ${KIND_CLUSTER_WORKLOAD}-${i}
    installGatewayAPI ${KIND_CLUSTER_WORKLOAD}-${i}
    deployIngressController ${KIND_CLUSTER_WORKLOAD}-${i}
    deployMetalLB ${KIND_CLUSTER_WORKLOAD}-${i} $((${metalLBSubnetStart} + ${i} - 1))
    deployKuadrant ${KIND_CLUSTER_WORKLOAD}-${i}
    deployLimitador ${KIND_CLUSTER_WORKLOAD}-${i}
    deployWebhookConfigs ${KIND_CLUSTER_WORKLOAD}-${i}
    deployDashboard ${KIND_CLUSTER_WORKLOAD}-${i} ${i}
    argocdAddCluster ${KIND_CLUSTER_CONTROL_PLANE} ${KIND_CLUSTER_WORKLOAD}-${i}
    deployAgentSecret ${KIND_CLUSTER_WORKLOAD}-${i} "true"
    deployAgentSecret ${KIND_CLUSTER_WORKLOAD}-${i} "false"
  done
fi

#10. Ensure the current context points to the control plane cluster
kubectl config use-context kind-${KIND_CLUSTER_CONTROL_PLANE}
