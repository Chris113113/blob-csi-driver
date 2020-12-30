# Copyright 2017 The Kubernetes Authors.
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

PKG = sigs.k8s.io/blob-csi-driver
GIT_COMMIT ?= $(shell git rev-parse HEAD)
REGISTRY ?= andyzhangx
REGISTRY_NAME ?= $(shell echo $(REGISTRY) | sed "s/.azurecr.io//g")
IMAGE_NAME ?= blob-csi
IMAGE_VERSION ?= v0.7.1
# Use a custom version for E2E tests if we are in Prow
ifdef CI
ifndef PUBLISH
override IMAGE_VERSION := e2e-$(GIT_COMMIT)
endif
endif
IMAGE_TAG = $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_VERSION)
IMAGE_TAG_LATEST = $(REGISTRY)/$(IMAGE_NAME):latest
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS ?= "-X ${PKG}/pkg/blob.driverVersion=${IMAGE_VERSION} -X ${PKG}/pkg/blob.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/blob.buildDate=${BUILD_DATE} -s -w -extldflags '-static'"
GINKGO_FLAGS = -ginkgo.noColor -ginkgo.v
GO111MODULE = off
GOPATH ?= $(shell go env GOPATH)
GOBIN ?= $(GOPATH)/bin
DOCKER_CLI_EXPERIMENTAL = enabled
export GOPATH GOBIN GO111MODULE DOCKER_CLI_EXPERIMENTAL

all: blob

.PHONY: verify
verify: unit-test
	hack/verify-all.sh

.PHONY: unit-test
unit-test:
	go test -covermode=count -coverprofile=profile.cov ./pkg/... ./test/utils/credentials

.PHONY: sanity-test
sanity-test: blob
	go test -v -timeout=30m ./test/sanity

.PHONY: integration-test
integration-test: blob
	go test -v -timeout=30m ./test/integration

.PHONY: e2e-test
e2e-test:
	go test -v -timeout=0 ./test/e2e ${GINKGO_FLAGS}

.PHONY: e2e-bootstrap
e2e-bootstrap: install-helm
	# Only build and push the image if it does not exist in the registry
	docker pull $(IMAGE_TAG) || make blob-container push
	helm install charts/latest/blob-csi-driver -n blob-csi-driver --namespace kube-system --wait \
		--set image.blob.pullPolicy=IfNotPresent \
		--set image.blob.repository=$(REGISTRY)/$(IMAGE_NAME) \
		--set image.blob.tag=$(IMAGE_VERSION)

.PHONY: install-helm
install-helm:
	# Use v2.11.0 helm to match tiller's version in clusters made by aks-engine
	curl https://raw.githubusercontent.com/helm/helm/master/scripts/get | DESIRED_VERSION=v2.11.0 bash
	# Make sure tiller is ready
	kubectl wait pod -l name=tiller --namespace kube-system --for condition=ready --timeout 5m
	helm version

.PHONY: e2e-teardown
e2e-teardown:
	helm delete --purge blob-csi-driver

.PHONY: blob
blob:
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags ${LDFLAGS} -o _output/blobplugin ./pkg/blobplugin

.PHONY: blob-windows
blob-windows:
	CGO_ENABLED=0 GOOS=windows go build -a -ldflags ${LDFLAGS} -o _output/blobplugin.exe ./pkg/blobplugin

.PHONY: container
container: blob
	docker build --no-cache -t $(IMAGE_TAG) -f ./pkg/blobplugin/dev.Dockerfile .

.PHONY: blob-container
blob-container:
	docker buildx rm container-builder || true
	docker buildx create --use --name=container-builder
ifdef CI
	docker buildx build --no-cache --build-arg LDFLAGS=${LDFLAGS} -t $(IMAGE_TAG) -f ./pkg/blobplugin/Dockerfile --platform="linux/amd64" --push .

	docker manifest create $(IMAGE_TAG) $(IMAGE_TAG)
	docker manifest inspect $(IMAGE_TAG)
ifdef PUBLISH
	docker manifest create $(IMAGE_TAG_LATEST) $(IMAGE_TAG)
	docker manifest inspect $(IMAGE_TAG_LATEST)
endif
endif

.PHONY: push
push:
ifdef CI
	docker manifest push --purge $(IMAGE_TAG)
else
	docker push $(IMAGE_TAG)
endif

.PHONY: push-latest
push-latest:
ifdef CI
	docker manifest push --purge $(IMAGE_TAG_LATEST)
else
	docker push $(IMAGE_TAG_LATEST)
endif

.PHONY: build-push
build-push: blob-container
	docker tag $(IMAGE_TAG) $(IMAGE_TAG_LATEST)
	docker push $(IMAGE_TAG_LATEST)

.PHONY: clean
clean:
	go clean -r -x
	-rm -rf _output
