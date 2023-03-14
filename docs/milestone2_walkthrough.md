# Milestone 2 Walkthrough

## Walkthrough Goals

* Get a local environment running with 1 control plane cluster and 2 workload clusters
* Define a Gateway in the control plane with a HTTPS listener, and have it instantiated in 2 workload clusters
* Deploy an Application with a HTTPRoute to the 2 workload clusters, and have the route attached to the Gateway listener
* Curl the Application host with DNS resolving from Route 53 and a TLS Certificate
* Apply a Kuadrant RateLimitPolicy to the Application
* Curl the Application host and see the rate limit being enforced

## Prerequisites

* [Docker Desktop](https://www.docker.com/products/docker-desktop/)
* [AWS Account](https://aws.amazon.com/) with [Route53](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/Welcome.html) Zone permissions
    * Choose an existing Hosted zone or create a new one for the walkthrough.
* openssl>=3
    * On macos a later version is available with `brew install openssl`. You'll need to update your PATH as macos provides an older version via libressl as well
    * On fedora use `dnf install openssl`

## Environment Setup

Create a set of environment files with your AWS configuration.
First, create a file called `aws-credentials.env` in the root of the project and add the following config, replacing values as necessary

```bash
AWS_ACCESS_KEY_ID=<AWS_ACCESS_KEY_ID>
AWS_SECRET_ACCESS_KEY=<AWS_SECRET_ACCESS_KEY>
AWS_REGION=<AWS_REGION>
```

Next, create a file called `controller-config.env` in the root of the project and add the following config, replacing values as necessary. For example, if your Hosted zone is for the domain `apps.example.com`, use that as the `ZONE_ROOT_DOMAIN`. This will mean your Application can use a hostname of `myapp.apps.example.com`

```bash
ZONE_ROOT_DOMAIN=<my.hosted.zone.domain.name>
AWS_DNS_PUBLIC_ZONE_ID=<my.hosted.zone.id>
```

Start the local clusters (1 control plane & 2 data plane workload clusters)

```bash
MCTC_WORKLOAD_CLUSTERS_COUNT=2 make local-setup
```

Run the controller in the control plane with the configuration from the env files.

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-control-plane.kubeconfig
(export $(cat ./controller-config.env | xargs) && export $(cat ./aws-credentials.env | xargs) && make build-controller install run-controller)
```

In a new terminal, create the multi cluster Gateway Class and tenant namespace

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-control-plane.kubeconfig
kubectl apply -f config/samples/gatewayclass.yaml
kubectl create namespace mctc-tenant
```

Start the syncer component in both workload clusters

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-1.kubeconfig
make kind-load-syncer
make deploy-syncer
kubectl wait --for=condition=Available deployment sync-agent -n mctc-system

export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-2.kubeconfig
kind load docker-image syncer:latest --name mctc-workload-2 --nodes mctc-workload-2-control-plane
make deploy-syncer
kubectl wait --for=condition=Available deployment sync-agent -n mctc-system
```

## Gateway Setup

Export the hostname you want to use for you Application, and copy the example gateway.
For example, if you set your `ZONE_ROOT_DOMAIN` as `example.com`, you could set `MYAPP_HOST` to `myapp.example.com` below to use it as your Application host.

```bash
export MYAPP_HOST=<myapp.host>
envsubst < docs/milestone2_gateway.yaml > gateway.yaml
```

Create the Gateway in the control plane.

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-control-plane.kubeconfig
kubectl apply -n mctc-tenant -f ./gateway.yaml
```

The Gateway should now be running in both workload clusters

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-1.kubeconfig
kubectl get gateway example-gateway -n mctc-downstream
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-2.kubeconfig
kubectl get gateway example-gateway -n mctc-downstream
```

## Application Setup

Copy the example Application, modifying the hostname.

```bash
envsubst < docs/milestone2_application.yaml > application.yaml
```

Create the Application in both workload clusters

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-1.kubeconfig
kubectl apply -f ./application.yaml
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-2.kubeconfig
kubectl apply -f ./application.yaml
```

Check that DNS has been setup.

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-control-plane.kubeconfig
kubectl wait --for=condition=Ready dnsrecord ${MYAPP_HOST} -n multi-cluster-traffic-controller-system
```

## Application Verification

Verify the Application host can be reached with curl. You should see 200 responses in the command output.

**NOTE** Although DNS records have been created at this time, depending on your DNS Provider there may be some negative caching that means it takes a while for curl to resolve the hostname correctly. Also, curl itself has some internal caching that could result in DNS resolution failing for some time.

```bash
while true; do curl -k -s -o /dev/null -w "%{http_code}\n"  https://${MYAPP_HOST} && sleep 2; done
```

## Add a RateLimitPolicy

Deploy Kuadrant to both workload clusters

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-1.kubeconfig
kubectl apply -f config/samples/kuadrant.yaml
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-2.kubeconfig
kubectl apply -f config/samples/kuadrant.yaml
```

Copy the example RateLimitPolicy, modifying the hostname.

```bash
envsubst < docs/milestone2_ratelimitpolicy.yaml > ratelimitpolicy.yaml
```

Create the RateLimitPolicy in both workload clusters

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-1.kubeconfig
kubectl apply -f ./ratelimitpolicy.yaml
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-2.kubeconfig
kubectl apply -f ./ratelimitpolicy.yaml
```

Check that the RateLimitPolicy has been reconciled by the Kuadrant Operator

```bash
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-1.kubeconfig
kubectl wait --for=condition=Available ratelimitpolicy mctc-demo -n mctc-demo
export KUBECONFIG=./tmp/kubeconfigs/mctc-workload-2.kubeconfig
kubectl wait --for=condition=Available ratelimitpolicy mctc-demo -n mctc-demo
```

## RateLimitPolicy Verifcation

Verify the Application host can be reached with curl, and rate limiting kicks in with a 429 response periodically.

```bash
while true; do curl -k -s -o /dev/null -w "%{http_code}\n"  https://${MYAPP_HOST} && sleep 2; done
```
