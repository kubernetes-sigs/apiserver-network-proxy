# Copyright 2019 The Kubernetes Authors.
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

# ARCH specifies the golang target platform, for docker build
# targetes. It is supported for backward compatibility, but prefer
# BUILDARCH and TARGETARCH.
ARCH ?= amd64
# BUILDARCH specifies the golang build platform, for docker build targets.
BUILDARCH ?= amd64
# TARGETARCH specifies the golang target platform, for docker build targets.
TARGETARCH ?= $(ARCH)
ALL_ARCH ?= amd64 arm arm64 ppc64le s390x
# The output type could either be docker (local), or registry.
OUTPUT_TYPE ?= docker
GO_TOOLCHAIN ?= golang
GO_VERSION ?= 1.23.6
BASEIMAGE ?= gcr.io/distroless/static-debian12:nonroot

ifeq ($(GOPATH),)
export GOPATH := $(shell go env GOPATH)
endif
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
INSTALL_LOCATION:=$(shell go env GOPATH)/bin
GOLANGCI_LINT_VERSION ?= 2.1.6
GOSEC_VERSION ?= 2.13.1

REGISTRY ?= gcr.io/$(shell gcloud config get-value project)
STAGING_REGISTRY := gcr.io/k8s-staging-kas-network-proxy

SERVER_IMAGE_NAME ?= proxy-server
AGENT_IMAGE_NAME ?= proxy-agent
TEST_CLIENT_IMAGE_NAME ?= proxy-test-client
TEST_SERVER_IMAGE_NAME ?= http-test-server

SERVER_FULL_IMAGE ?= $(REGISTRY)/$(SERVER_IMAGE_NAME)
AGENT_FULL_IMAGE ?= $(REGISTRY)/$(AGENT_IMAGE_NAME)
TEST_CLIENT_FULL_IMAGE ?= $(REGISTRY)/$(TEST_CLIENT_IMAGE_NAME)
TEST_SERVER_FULL_IMAGE ?= $(REGISTRY)/$(TEST_SERVER_IMAGE_NAME)

TAG ?= $(shell git rev-parse HEAD)

DOCKER_CMD ?= docker
DOCKER_CLI_EXPERIMENTAL ?= enabled
PROXY_SERVER_IP ?= 127.0.0.1

KIND_IMAGE ?= kindest/node:v1.30.2
CONNECTION_MODE ?= grpc

MOCKGEN_VERSION := $(shell mockgen -version)
DESIRED_MOCKGEN := "v0.5.2"
## --------------------------------------
## Testing
## --------------------------------------
.PHONY: mock_gen
mock_gen:
	echo "Mock gen is set to $(MOCKGEN_VERSION)"
	if [ "$(MOCKGEN_VERSION)" != $(DESIRED_MOCKGEN) ]; then echo "Error need mockgen version $(DESIRED_VERSION)"; exit 1; fi
	mkdir -p proto/agent/mocks
	mockgen --build_flags=--mod=mod sigs.k8s.io/apiserver-network-proxy/proto/agent AgentService_ConnectServer > proto/agent/mocks/agent_mock.go
	cat hack/go-license-header.txt proto/agent/mocks/agent_mock.go > proto/agent/mocks/agent_mock.licensed.go
	mv proto/agent/mocks/agent_mock.licensed.go proto/agent/mocks/agent_mock.go

# Unit tests with faster execution (nicer for development).
.PHONY: fast-test
fast-test:
	go test -mod=vendor -race $(shell go list ./... | grep -v -e "/e2e$$" -e "/e2e/.*")
	cd konnectivity-client && go test -race ./...

# Unit tests with fuller coverage, invoked by CI system.
.PHONY: test
test:
	go test -v -mod=vendor -race -covermode=atomic -coverprofile=konnectivity.out $(shell go list ./... | grep -v -e "/e2e$$" -e "/e2e/.*") && go tool cover -html=konnectivity.out -o=konnectivity.html
	cd konnectivity-client && go test -race -covermode=atomic -coverprofile=client.out ./... && go tool cover -html=client.out -o=client.html

.PHONY: test-integration
test-integration: build
	go test -mod=vendor -race ./tests -agent-path $(PWD)/bin/proxy-agent

.PHONY: test-e2e
test-e2e: docker-build
	go test -mod=vendor ./e2e -race -agent-image ${AGENT_FULL_IMAGE}-$(TARGETARCH):${TAG} -server-image ${SERVER_FULL_IMAGE}-$(TARGETARCH):${TAG} -kind-image ${KIND_IMAGE} -mode ${CONNECTION_MODE}

# e2e test runner for continuous integration that does not build a new image.
.PHONY: test-e2e-ci
test-e2e-ci:
	go test -mod=vendor ./e2e -race -agent-image ${AGENT_FULL_IMAGE}-$(TARGETARCH):${TAG} -server-image ${SERVER_FULL_IMAGE}-$(TARGETARCH):${TAG} -kind-image ${KIND_IMAGE} -mode ${CONNECTION_MODE}

## --------------------------------------
## Binaries
## --------------------------------------

bin:
	mkdir -p bin

.PHONY: build
build: bin/proxy-agent bin/proxy-server bin/proxy-test-client bin/http-test-server

.PHONY: bin/proxy-agent
bin/proxy-agent:
	GO111MODULE=on go build -mod=vendor -o bin/proxy-agent cmd/agent/main.go

.PHONY: bin/proxy-test-client
bin/proxy-test-client:
	GO111MODULE=on go build -mod=vendor -o bin/proxy-test-client cmd/test-client/main.go

.PHONY: bin/http-test-server
bin/http-test-server:
	GO111MODULE=on go build -mod=vendor -o bin/http-test-server cmd/test-server/main.go

.PHONY: bin/proxy-server
bin/proxy-server:
	GO111MODULE=on go build -mod=vendor -o bin/proxy-server cmd/server/main.go

## --------------------------------------
## Linting
## --------------------------------------

.PHONY: lint
lint:
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(INSTALL_LOCATION) v$(GOLANGCI_LINT_VERSION)
	$(INSTALL_LOCATION)/golangci-lint run --config ./.golangci.yaml --verbose

## --------------------------------------
## Go
## --------------------------------------

.PHONY: mod-download
mod-download:
	go mod download

## --------------------------------------
## Proto
## --------------------------------------

.PHONY: gen
gen: mod-download gen-proto mock_gen

.PHONY: gen-proto
gen-proto:
	protoc -I . konnectivity-client/proto/client/client.proto --go_out=. --go_opt=paths=source_relative --go-grpc_out=require_unimplemented_servers=false:. --go-grpc_opt=paths=source_relative
	cat hack/go-license-header.txt konnectivity-client/proto/client/client_grpc.pb.go > konnectivity-client/proto/client/client_grpc.licensed.go
	mv konnectivity-client/proto/client/client_grpc.licensed.go konnectivity-client/proto/client/client_grpc.pb.go
	protoc -I . proto/agent/agent.proto --go_out=. --go_opt=paths=source_relative --go-grpc_out=require_unimplemented_servers=false:. --go-grpc_opt=paths=source_relative
	cat hack/go-license-header.txt proto/agent/agent_grpc.pb.go > proto/agent/agent_grpc.licensed.go
	mv proto/agent/agent_grpc.licensed.go proto/agent/agent_grpc.pb.go

## --------------------------------------
## Certs
## --------------------------------------

easy-rsa.tar.gz:
	curl -L -o easy-rsa.tar.gz --connect-timeout 20 --retry 6 --retry-delay 2 https://dl.k8s.io/easy-rsa/easy-rsa.tar.gz

easy-rsa: easy-rsa.tar.gz
	tar xvf easy-rsa.tar.gz
	mv easy-rsa-master easy-rsa

cfssl:
	@if ! command -v cfssl &> /dev/null; then \
		curl --retry 10 -L -o cfssl https://github.com/cloudflare/cfssl/releases/download/v1.6.5/cfssl_1.6.5_$(GOOS)_$(GOARCH); \
		chmod +x cfssl; \
	fi

cfssljson:
	@if ! command -v cfssljson &> /dev/null; then \
		curl --retry 10 -L -o cfssljson https://github.com/cloudflare/cfssl/releases/download/v1.6.5/cfssljson_1.6.5_$(GOOS)_$(GOARCH); \
		chmod +x cfssljson; \
	fi

.PHONY: certs
certs: export PATH := $(shell pwd):$(PATH)
certs: easy-rsa cfssl cfssljson
	# set up easy-rsa
	cp -rf easy-rsa/easyrsa3 easy-rsa/frontend
	cp -rf easy-rsa/easyrsa3 easy-rsa/agent
	# create the client <-> server-proxy connection certs
	cd easy-rsa/frontend; \
	./easyrsa init-pki; \
	./easyrsa --batch "--req-cn=127.0.0.1@$(date +%s)" build-ca nopass; \
	./easyrsa --subject-alt-name="DNS:kubernetes,DNS:localhost,IP:127.0.0.1" build-server-full "proxy-frontend" nopass; \
	./easyrsa build-client-full proxy-client nopass; \
	echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","client auth"]}}}' > "ca-config.json"; \
	echo '{"CN":"proxy","names":[{"O":"system:nodes"}],"hosts":[""],"key":{"algo":"rsa","size":2048}}' | cfssl gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | cfssljson -bare proxy
	mkdir -p certs/frontend
	cp -r easy-rsa/frontend/pki/private certs/frontend
	cp -r easy-rsa/frontend/pki/issued certs/frontend
	cp easy-rsa/frontend/pki/ca.crt certs/frontend/issued
	# create the agent <-> server-proxy connection certs
	cd easy-rsa/agent; \
	./easyrsa init-pki; \
	./easyrsa --batch "--req-cn=$(PROXY_SERVER_IP)@$(date +%s)" build-ca nopass; \
	./easyrsa --subject-alt-name="DNS:kubernetes,DNS:localhost,IP:$(PROXY_SERVER_IP)" build-server-full "proxy-frontend" nopass; \
	./easyrsa build-client-full proxy-agent nopass; \
	echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","agent auth"]}}}' > "ca-config.json"; \
	echo '{"CN":"proxy","names":[{"O":"system:nodes"}],"hosts":[""],"key":{"algo":"rsa","size":2048}}' | cfssl gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | cfssljson -bare proxy
	mkdir -p certs/agent
	cp -r easy-rsa/agent/pki/private certs/agent
	cp -r easy-rsa/agent/pki/issued certs/agent
	cp easy-rsa/agent/pki/ca.crt certs/agent/issued

## --------------------------------------
## Docker
## --------------------------------------

.PHONY: buildx-setup
buildx-setup:
	${DOCKER_CMD} buildx inspect img-builder > /dev/null || docker buildx create --name img-builder --use

# Does not include test images
.PHONY: docker-build
docker-build: docker-build/proxy-agent docker-build/proxy-server

.PHONY: docker-build-test
docker-build-test: docker-build/proxy-test-client docker-build/http-test-server

.PHONY: docker-push
docker-push: docker-push/proxy-agent docker-push/proxy-server

.PHONY: docker-build/proxy-agent
docker-build/proxy-agent: cmd/agent/main.go proto/agent/agent.pb.go buildx-setup
	@[ "${TAG}" ] || ( echo "TAG is not set"; exit 1 )
	echo "Building proxy-agent with ${BUILDARCH} for ${TARGETARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(TARGETARCH) --build-arg GO_TOOLCHAIN=$(GO_TOOLCHAIN) --build-arg GO_VERSION=$(GO_VERSION) --build-arg BUILDARCH=$(BUILDARCH) --build-arg TARGETARCH=$(TARGETARCH) --build-arg BASEIMAGE=$(BASEIMAGE) -f artifacts/images/agent-build.Dockerfile -t ${AGENT_FULL_IMAGE}-$(TARGETARCH):${TAG}

.PHONY: docker-push/proxy-agent
docker-push/proxy-agent: docker-build/proxy-agent
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${AGENT_FULL_IMAGE}-$(TARGETARCH):${TAG}

.PHONY: docker-build/proxy-server
docker-build/proxy-server: cmd/server/main.go proto/agent/agent.pb.go buildx-setup
	@[ "${TAG}" ] || ( echo "TAG is not set"; exit 1 )
	echo "Building proxy-server with ${BUILDARCH} for ${TARGETARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(TARGETARCH) --build-arg GO_TOOLCHAIN=$(GO_TOOLCHAIN) --build-arg GO_VERSION=$(GO_VERSION) --build-arg BUILDARCH=$(BUILDARCH) --build-arg TARGETARCH=$(TARGETARCH) --build-arg BASEIMAGE=$(BASEIMAGE) -f artifacts/images/server-build.Dockerfile -t ${SERVER_FULL_IMAGE}-$(TARGETARCH):${TAG}

.PHONY: docker-push/proxy-server
docker-push/proxy-server: docker-build/proxy-server
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${SERVER_FULL_IMAGE}-$(TARGETARCH):${TAG}

.PHONY: docker-build/proxy-test-client
docker-build/proxy-test-client: cmd/test-client/main.go proto/agent/agent.pb.go buildx-setup
	@[ "${TAG}" ] || ( echo "TAG is not set"; exit 1 )
	echo "Building proxy-test-client with ${BUILDARCH} for ${TARGETARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(TARGETARCH) --build-arg GO_TOOLCHAIN=$(GO_TOOLCHAIN) --build-arg GO_VERSION=$(GO_VERSION) --build-arg BUILDARCH=$(BUILDARCH) --build-arg TARGETARCH=$(TARGETARCH) --build-arg BASEIMAGE=$(BASEIMAGE) -f artifacts/images/test-client-build.Dockerfile -t ${TEST_CLIENT_FULL_IMAGE}-$(TARGETARCH):${TAG}

.PHONY: docker-push/proxy-test-client
docker-push/proxy-test-client: docker-build/proxy-test-client
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${TEST_CLIENT_FULL_IMAGE}-$(TARGETARCH):${TAG}

.PHONY: docker-build/http-test-server
docker-build/http-test-server: cmd/test-server/main.go buildx-setup
	@[ "${TAG}" ] || ( echo "TAG is not set"; exit 1 )
	echo "Building http-test-server with ${BUILDARCH} for ${TARGETARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(TARGETARCH) --build-arg GO_TOOLCHAIN=$(GO_TOOLCHAIN) --build-arg GO_VERSION=$(GO_VERSION) --build-arg BUILDARCH=$(BUILDARCH) --build-arg TARGETARCH=$(TARGETARCH) --build-arg BASEIMAGE=$(BASEIMAGE) -f artifacts/images/test-server-build.Dockerfile -t ${TEST_SERVER_FULL_IMAGE}-$(TARGETARCH):${TAG}

.PHONY: docker-push/http-test-server
docker-push/http-test-server: docker-build/http-test-server
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${TEST_SERVER_FULL_IMAGE}-$(TARGETARCH):${TAG}

## --------------------------------------
## Docker — All ARCH
## --------------------------------------

# As `docker buildx` is time and resource consuming, if not necessary, building specific arch images,
# like `make docker-build-arch-amd64`, is recommended.
.PHONY: docker-build-all
docker-build-all: $(addprefix docker-build-arch-,$(ALL_ARCH))

.PHONY: docker-push-all
docker-push-all: $(addprefix docker-push/proxy-agent-,$(ALL_ARCH)) $(addprefix docker-push/proxy-server-,$(ALL_ARCH))
	$(MAKE) docker-push-manifest/proxy-agent
	$(MAKE) docker-push-manifest/proxy-server

docker-build-arch-%:
	$(MAKE) docker-build/proxy-agent-$*
	$(MAKE) docker-build/proxy-server-$*

docker-build/proxy-agent-%:
	$(MAKE) TARGETARCH=$* docker-build/proxy-agent

docker-push/proxy-agent-%:
	$(MAKE) TARGETARCH=$* docker-push/proxy-agent

docker-build/proxy-server-%:
	$(MAKE) TARGETARCH=$* docker-build/proxy-server

docker-push/proxy-server-%:
	$(MAKE) TARGETARCH=$* docker-push/proxy-server

.PHONY: docker-push-manifest/proxy-agent
docker-push-manifest/proxy-agent: ## Push the fat manifest docker image.
	## Minimum docker version 18.06.0 is required for creating and pushing manifest images.
	${DOCKER_CMD} manifest create --amend $(AGENT_FULL_IMAGE):$(TAG) $(shell echo $(ALL_ARCH) | sed -e "s~[^ ]*~$(AGENT_FULL_IMAGE)\-&:$(TAG)~g")
	@for arch in $(ALL_ARCH); do ${DOCKER_CMD} manifest annotate --arch $${arch} ${AGENT_FULL_IMAGE}:${TAG} ${AGENT_FULL_IMAGE}-$${arch}:${TAG}; done
	${DOCKER_CMD} manifest push --purge $(AGENT_FULL_IMAGE):$(TAG)

.PHONY: docker-push-manifest/proxy-server
docker-push-manifest/proxy-server: ## Push the fat manifest docker image.
	## Minimum docker version 18.06.0 is required for creating and pushing manifest images.
	${DOCKER_CMD} manifest create --amend $(SERVER_FULL_IMAGE):$(TAG) $(shell echo $(ALL_ARCH) | sed -e "s~[^ ]*~$(SERVER_FULL_IMAGE)\-&:$(TAG)~g")
	@for arch in $(ALL_ARCH); do ${DOCKER_CMD} manifest annotate --arch $${arch} ${SERVER_FULL_IMAGE}:${TAG} ${SERVER_FULL_IMAGE}-$${arch}:${TAG}; done
	${DOCKER_CMD} manifest push --purge $(SERVER_FULL_IMAGE):$(TAG)

## --------------------------------------
## Release
## --------------------------------------

.PHONY: release-staging
release-staging: ## Builds and push container images to the staging bucket.
	REGISTRY=$(STAGING_REGISTRY) $(MAKE) docker-push-all release-alias-tag

.PHONY: release-alias-tag
release-alias-tag: # Adds the tag to the last build tag. BASE_REF comes from the cloudbuild.yaml
	gcloud container images add-tag $(AGENT_FULL_IMAGE):$(TAG) $(AGENT_FULL_IMAGE):$(BASE_REF)
	gcloud container images add-tag $(SERVER_FULL_IMAGE):$(TAG) $(SERVER_FULL_IMAGE):$(BASE_REF)

## --------------------------------------
## Cleanup / Verification
## --------------------------------------

.PHONY: clean
clean:
	go clean -testcache
	rm -rf proto/agent/agent.pb.go proto/agent/agent_grpc.pb.go konnectivity-client/proto/client/client.pb.go konnectivity-client/proto/client/client_grpc.pb.go konnectivity-client/proto/client/client_grpc.licensed.go proto/agent/agent_grpc.licensed.go easy-rsa.tar.gz easy-rsa cfssl cfssljson certs bin proto/agent/mocks konnectivity.html konnectivity.out konnectivity-client/client.html konnectivity-client/client.out
