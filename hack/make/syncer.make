##@ Syncer

SYNCER_IMG ?= syncer:$(TAG)

.PHONY: build-syncer
build-syncer: manifests generate fmt vet ## Build syncer binary.
	go build -o bin/syncer ./cmd/syncer/main.go

.PHONY: run-syncer
run-syncer: manifests generate fmt vet install
	go run ./cmd/syncer/main.go \
	    --metrics-bind-address=:8086 \
	    --health-probe-bind-address=:8087 \
	    --control-plane-config-name=control-plane-cluster \
	    --control-plane-config-namespace=mctc-system \
	    --synced-resources=gateways.v1beta1.gateway.networking.k8s.io \
	    --synced-resources=secrets.v1

.PHONY: docker-build-syncer
docker-build-syncer: ## Build docker image with the syncer.
	docker build --target syncer -t ${SYNCER_IMG} .
	docker image prune -f --filter label=stage=mctc-builder

.PHONY: kind-load-syncer
kind-load-syncer: docker-build-syncer
	kind load docker-image ${SYNCER_IMG} --name mctc-workload-1  --nodes mctc-workload-1-control-plane

.PHONY: docker-push-syncer
docker-push-syncer: ## Push docker image with the syncer.
	docker push ${SYNCER_IMG}

.PHONY: deploy-syncer
deploy-syncer: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/syncer && $(KUSTOMIZE) edit set image syncer=${SYNCER_IMG}
	$(KUSTOMIZE) build config/syncer | kubectl apply -f -

.PHONY: undeploy-syncer
undeploy-syncer: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/syncer | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: redeploy-syncer
redeploy-syncer: undeploy-syncer deploy-syncer

.PHONY: restart-syncer
restart-syncer:
	kubectl rollout restart deploy sync-agent -n mctc-system
    