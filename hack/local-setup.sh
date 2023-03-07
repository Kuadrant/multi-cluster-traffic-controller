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
source "${LOCAL_SETUP_DIR}"/.cleanupUtils

KIND_CLUSTER_PREFIX="mctc-"
KIND_CLUSTER_CONTROL_PLANE="${KIND_CLUSTER_PREFIX}control-plane"
KIND_CLUSTER_WORKLOAD="${KIND_CLUSTER_PREFIX}workload"

METALLB_KUSTOMIZATION_DIR=${LOCAL_SETUP_DIR}/../config/metallb

LOCAL_SETUP_CONFIG_DIR=${LOCAL_SETUP_DIR}/../config/local-setup
LOCAL_SETUP_CLUSTERS_DIR=${LOCAL_SETUP_CONFIG_DIR}/clusters
LOCAL_SETUP_CLUSTERS_TMPL_DIR=${LOCAL_SETUP_CONFIG_DIR}/cluster-templates

set -e pipefail

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

# Generates a kubeconfig for the given cluster. The server is changed to use the mctc network internal node ip which is
# reachable from the host and inside containers running on clusters. Note: Mac users need to do something or other for
# this to work.
generateClusterKubeconfig() {
    clusterName=${1}
    clusterKubeconfig=${2}
    kubectl config use-context kind-${clusterName}
    echo "Generate kubeconfig for ${clusterName} to ${clusterKubeconfig}"
    ${KIND_BIN} get kubeconfig --name ${clusterName} --internal \
      | sed "s/${clusterName}-control-plane/$(docker inspect "${clusterName}-control-plane" \
      --format "{{ .NetworkSettings.Networks.mctc.IPAddress }}")/g" \
      > "${clusterKubeconfig}"
}

# Generates a secret kustomization for the the given cluster. The tlsconfig, server, name etc.. are extracted from the
# kubeconfig and stored as config for the secret generator.
generateClusterSecret() {
    clusterName=${1}
    clusterKubeconfig=${2}
    clusterDir=${LOCAL_SETUP_CLUSTERS_DIR}/${clusterName}
    kubectl config use-context kind-${clusterName}
    echo "Generate cluster secret for ${clusterName}"

    mkdir -p ${clusterDir}/cluster-secret
    (
     export CLUSTER_NAME=${clusterName}
     export CLUSTER_SERVER=$(kubectl --kubeconfig ${clusterKubeconfig} config view -o jsonpath="{$.clusters[?(@.name == 'kind-${clusterName}')].cluster.server}")
     export CLUSTER_CA_DATA=$(kubectl --kubeconfig ${clusterKubeconfig} config view --raw -o jsonpath="{$.clusters[?(@.name == 'kind-${clusterName}')].cluster.certificate-authority-data}")
     export CLUSTER_CERT_DATA=$(kubectl --kubeconfig ${clusterKubeconfig} config view --raw -o jsonpath="{$.users[?(@.name == 'kind-${clusterName}')].user.client-certificate-data}")
     export CLUSTER_KEY_DATA=$(kubectl --kubeconfig ${clusterKubeconfig} config view --raw -o jsonpath="{$.users[?(@.name == 'kind-${clusterName}')].user.client-key-data}")

     envsubst \
             < "${LOCAL_SETUP_CLUSTERS_TMPL_DIR}/cluster-secret/cluster-config.template.env" \
             > "${clusterDir}/cluster-secret/cluster-config.env"
     envsubst \
             < "${LOCAL_SETUP_CLUSTERS_TMPL_DIR}/cluster-secret/config.template" \
             > "${clusterDir}/cluster-secret/config"
     envsubst \
             < "${LOCAL_SETUP_CLUSTERS_TMPL_DIR}/cluster-secret/kustomization.template.yaml" \
             > "${clusterDir}/cluster-secret/kustomization.yaml"
    )
}

# Generates a control cluster kustomize overlay for the given cluster.
generateControlClusterConfig() {
    clusterName=${1}
    clusterDir=${LOCAL_SETUP_CLUSTERS_DIR}/${clusterName}
    kubectl config use-context kind-${clusterName}
    echo "Generate control cluster config for ${clusterName} in ${clusterDir}"

    mkdir -p ${clusterDir}
    cp -r ${LOCAL_SETUP_CLUSTERS_TMPL_DIR}/control-cluster/* ${clusterDir}/

    clusterKubeconfig=${clusterDir}/${clusterName}.kubeconfig
    generateClusterKubeconfig $1 $clusterKubeconfig
    generateClusterSecret $1 $clusterKubeconfig
}

#  Generates a workload cluster kustomize overlay for the given cluster.
generateWorkloadClusterConfig() {
    clusterName=${1}
    controllerClusterName=${2}
    clusterDir=${LOCAL_SETUP_CLUSTERS_DIR}/${clusterName}
    kubectl config use-context kind-${clusterName}
    echo "Generate workload cluster config for ${clusterName} in ${clusterDir}"

    mkdir -p ${clusterDir}
    cp -r ${LOCAL_SETUP_CLUSTERS_TMPL_DIR}/workload-cluster/* ${clusterDir}/

    clusterKubeconfig=${clusterDir}/${clusterName}.kubeconfig
    generateClusterKubeconfig $1 $clusterKubeconfig
    generateClusterSecret $1 $clusterKubeconfig

    controllerClusterDir=${LOCAL_SETUP_CLUSTERS_DIR}/${controllerClusterName}
    # Add workload cluster secret to argocd on control plane cluster
    ${YQ_BIN} eval ". *+ {\"resources\":[\"../../${clusterName}/cluster-secret\"]}" "${controllerClusterDir}/argocd/kustomization.yaml" --inplace
    # Add control plane cluster to workload cluster
    ${YQ_BIN} eval ". *+ {\"resources\":[\"../${controllerClusterName}/cluster-secret\"]}" "${clusterDir}/kustomization.yaml" --inplace
}

#  Apply a kustomize overlay for the given cluster.
applyClusterConfig() {
    clusterName=${1}
    clusterDir=${LOCAL_SETUP_CLUSTERS_DIR}/${clusterName}
    kubectl config use-context kind-${clusterName}
    echo "Apply cluster config for ${clusterName}"
    #Apply anything in the init config first
    ${KUSTOMIZE_BIN} --load-restrictor LoadRestrictionsNone build ${clusterDir}/init --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
    # Wait for all expected deployments to be ready
    echo "Waiting for deployments to be ready ..."
    kubectl -n ingress-nginx wait --timeout=300s --for=condition=Available deployments --all
    #Apply all config for the cluster
    ${KUSTOMIZE_BIN} --load-restrictor LoadRestrictionsNone build ${clusterDir} --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
}

printClusterInfo() {
    clusterName=${1}
    kubectl config use-context kind-${clusterName}

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

cleanup

port80=9090
port443=8445
proxyPort=9200
metalLBSubnetStart=200

# Create network for the clusters
docker network create -d bridge --subnet 172.32.0.0/16 mctc --gateway 172.32.0.1 \
  -o "com.docker.network.bridge.enable_ip_masquerade"="true" \
  -o "com.docker.network.driver.mtu"="1500"

### Control Cluster ###

kindCreateCluster ${KIND_CLUSTER_CONTROL_PLANE} ${port80} ${port443}
generateControlClusterConfig ${KIND_CLUSTER_CONTROL_PLANE}

### Workload Clusters ###

# Create workload Kind clusters if MCTC_WORKLOAD_CLUSTERS_COUNT environment variable is set
if [[ -n "${MCTC_WORKLOAD_CLUSTERS_COUNT}" ]]; then
  for ((i = 1; i <= ${MCTC_WORKLOAD_CLUSTERS_COUNT}; i++)); do
    kindCreateCluster ${KIND_CLUSTER_WORKLOAD}-${i} $((${port80} + ${i})) $((${port443} + ${i}))
  done
fi

# Generate workload cluster configs if MCTC_WORKLOAD_CLUSTERS_COUNT environment variable is set
if [[ -n "${MCTC_WORKLOAD_CLUSTERS_COUNT}" ]]; then
  for ((i = 1; i <= ${MCTC_WORKLOAD_CLUSTERS_COUNT}; i++)); do
    generateWorkloadClusterConfig ${KIND_CLUSTER_WORKLOAD}-${i} ${KIND_CLUSTER_CONTROL_PLANE}
  done
fi

# Apply all cluster configs to local Kind clusters
applyClusterConfig ${KIND_CLUSTER_CONTROL_PLANE}
deployDashboard $KIND_CLUSTER_CONTROL_PLANE 0
if [[ -n "${MCTC_WORKLOAD_CLUSTERS_COUNT}" ]]; then
  for ((i = 1; i <= ${MCTC_WORKLOAD_CLUSTERS_COUNT}; i++)); do
    deployMetalLB ${KIND_CLUSTER_WORKLOAD}-${i} $((${metalLBSubnetStart} + ${i} - 1))
    deployDashboard ${KIND_CLUSTER_WORKLOAD}-${i} ${i}
    applyClusterConfig ${KIND_CLUSTER_WORKLOAD}-${i}
  done
fi

printClusterInfo $KIND_CLUSTER_CONTROL_PLANE

kubectl config use-context kind-${KIND_CLUSTER_CONTROL_PLANE}
