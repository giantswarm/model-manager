##@ Development

BINARY := model-manager
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

.PHONY: build-linux-amd64
build-linux-amd64: ## Build the linux/amd64 binary the Dockerfile expects.
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=dev-$(GIT_SHA) -X main.commit=$(GIT_SHA)" -o $(BINARY)-linux-amd64 .

.PHONY: docker-build
docker-build: build-linux-amd64 ## Build a local dev image (TAG=model-manager:dev).
	docker build --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 -t $(or $(TAG),model-manager:dev) .

.PHONY: test-race
test-race: ## Run tests with the race detector.
	go test -race ./...

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint helm/model-manager

.PHONY: helm-template
helm-template: ## Render the chart with defaults.
	helm template model-manager helm/model-manager

.PHONY: helm-schema
helm-schema: ## Regenerate values.schema.json (needs the helm schema plugin and schemalint).
	helm schema --config helm/model-manager/.schema.yaml
	python3 -c 'import json,sys; h=lambda o: {**{k:v for k,v in o.items() if k!="additionalProperties"},"unevaluatedProperties":False} if ("$$ref" in o and o.get("additionalProperties") is False) else o; p=sys.argv[1]; f=open(p,encoding="utf-8"); d=json.load(f,object_hook=h); f.close(); f=open(p,"w",encoding="utf-8"); json.dump(d,f); f.close()' helm/model-manager/values.schema.json
	schemalint normalize helm/model-manager/values.schema.json -o helm/model-manager/values.schema.json --force

.PHONY: helm-docs
helm-docs: ## Regenerate the chart README.
	helm-docs --chart-search-root=helm --sort-values-order=file
