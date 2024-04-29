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

ARCH ?= amd64
ARCH_LIST ?= amd64 arm arm64 ppc64le s390x
RELEASE_ARCH_LIST = amd64 arm64
# The output type could either be docker (local), or registry.
OUTPUT_TYPE ?= docker

ifeq ($(GOPATH),)
export GOPATH := $(shell go env GOPATH)
endif
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
INSTALL_LOCATION:=$(shell go env GOPATH)/bin
GOLANGCI_LINT_VERSION ?= 1.54.0
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
## --------------------------------------
## Testing
## --------------------------------------
.PHONY: mock_gen
mock_gen:
	mkdir -p proto/agent/mocks
	mockgen sigs.k8s.io/apiserver-network-proxy/proto/agent AgentService_ConnectServer > proto/agent/mocks/agent_mock.go
	cat hack/go-license-header.txt proto/agent/mocks/agent_mock.go > proto/agent/mocks/agent_mock.licensed.go
	mv proto/agent/mocks/agent_mock.licensed.go proto/agent/mocks/agent_mock.go

.PHONY: test
test:
	go test -race -covermode=atomic -coverprofile=konnectivity.out ./... && go tool cover -html=konnectivity.out -o=konnectivity.html
	cd konnectivity-client && go test -race -covermode=atomic -coverprofile=client.out ./... && go tool cover -html=client.out -o=client.html

## --------------------------------------
## Binaries
## --------------------------------------

SOURCE = $(shell find . -name \*.go)

bin:
	mkdir -p bin

.PHONY: build
build: bin/proxy-agent bin/proxy-server bin/proxy-test-client bin/http-test-server

bin/proxy-agent: bin $(SOURCE)
	GO111MODULE=on go build -o bin/proxy-agent cmd/agent/main.go

bin/proxy-test-client: bin $(SOURCE)
	GO111MODULE=on go build -o bin/proxy-test-client cmd/test-client/main.go

bin/http-test-server: bin $(SOURCE)
	GO111MODULE=on go build -o bin/http-test-server cmd/test-server/main.go

bin/proxy-server: bin $(SOURCE)
	GO111MODULE=on go build -o bin/proxy-server cmd/server/main.go

## --------------------------------------
## Linting
## --------------------------------------

.PHONY: lint
lint:
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(INSTALL_LOCATION) v$(GOLANGCI_LINT_VERSION)
	$(INSTALL_LOCATION)/golangci-lint run --verbose

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
		curl --retry 10 -L -o cfssl https://github.com/cloudflare/cfssl/releases/download/v1.5.0/cfssl_1.5.0_$(GOOS)_$(GOARCH); \
		chmod +x cfssl; \
	fi

cfssljson:
	@if ! command -v cfssljson &> /dev/null; then \
		curl --retry 10 -L -o cfssljson https://github.com/cloudflare/cfssl/releases/download/v1.5.0/cfssljson_1.5.0_$(GOOS)_$(GOARCH); \
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
	echo "Building proxy-agent for ${ARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(ARCH) --build-arg ARCH=$(ARCH) -f artifacts/images/agent-build.Dockerfile -t ${AGENT_FULL_IMAGE}-$(ARCH):${TAG}

.PHONY: docker-push/proxy-agent
docker-push/proxy-agent: docker-build/proxy-agent
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${AGENT_FULL_IMAGE}-$(ARCH):${TAG}

.PHONY: docker-build/proxy-server
docker-build/proxy-server: cmd/server/main.go proto/agent/agent.pb.go buildx-setup
	@[ "${TAG}" ] || ( echo "TAG is not set"; exit 1 )
	echo "Building proxy-server for ${ARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(ARCH) --build-arg ARCH=$(ARCH) -f artifacts/images/server-build.Dockerfile -t ${SERVER_FULL_IMAGE}-$(ARCH):${TAG}

.PHONY: docker-push/proxy-server
docker-push/proxy-server: docker-build/proxy-server
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${SERVER_FULL_IMAGE}-$(ARCH):${TAG}

.PHONY: docker-build/proxy-test-client
docker-build/proxy-test-client: cmd/test-client/main.go proto/agent/agent.pb.go buildx-setup
	@[ "${TAG}" ] || ( echo "TAG is not set"; exit 1 )
	echo "Building proxy-test-client for ${ARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(ARCH) --build-arg ARCH=$(ARCH) -f artifacts/images/test-client-build.Dockerfile -t ${TEST_CLIENT_FULL_IMAGE}-$(ARCH):${TAG}

.PHONY: docker-push/proxy-test-client
docker-push/proxy-test-client: docker-build/proxy-test-client
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${TEST_CLIENT_FULL_IMAGE}-$(ARCH):${TAG}

.PHONY: docker-build/http-test-server
docker-build/http-test-server: cmd/test-server/main.go buildx-setup
	@[ "${TAG}" ] || ( echo "TAG is not set"; exit 1 )
	echo "Building http-test-server for ${ARCH}"
	${DOCKER_CMD} buildx build . --pull --output=type=$(OUTPUT_TYPE) --platform linux/$(ARCH) --build-arg ARCH=$(ARCH) -f artifacts/images/test-server-build.Dockerfile -t ${TEST_SERVER_FULL_IMAGE}-$(ARCH):${TAG}

.PHONY: docker-push/http-test-server
docker-push/http-test-server: docker-build/http-test-server
	@[ "${DOCKER_CMD}" ] || ( echo "DOCKER_CMD is not set"; exit 1 )
	${DOCKER_CMD} push ${TEST_SERVER_FULL_IMAGE}-$(ARCH):${TAG}

## --------------------------------------
## Docker — All ARCH
## --------------------------------------

# As `docker buildx` is time and resource consuming, if not necessary, building specific arch images,
# like `make docker-build-arch-aamd64`, is recommended.
.PHONY: docker-build-all
docker-build-all: $(addprefix docker-build-arch-,$(ARCH_LIST))

.PHONY: docker-push-all
docker-push-all: $(addprefix docker-push/proxy-agent-,$(ARCH_LIST)) $(addprefix docker-push/proxy-server-,$(ARCH_LIST))
	$(MAKE) docker-push-manifest/proxy-agent
	$(MAKE) docker-push-manifest/proxy-server

docker-build-arch-%:
	$(MAKE) docker-build/proxy-agent-$*
	$(MAKE) docker-build/proxy-server-$*

docker-build/proxy-agent-%:
	$(MAKE) ARCH=$* docker-build/proxy-agent

docker-push/proxy-agent-%:
	$(MAKE) ARCH=$* docker-push/proxy-agent

docker-build/proxy-server-%:
	$(MAKE) ARCH=$* docker-build/proxy-server

docker-push/proxy-server-%:
	$(MAKE) ARCH=$* docker-push/proxy-server

.PHONY: docker-push-manifest/proxy-agent
docker-push-manifest/proxy-agent: ## Push the fat manifest docker image.
	## Minimum docker version 18.06.0 is required for creating and pushing manifest images.
	${DOCKER_CMD} manifest create --amend $(AGENT_FULL_IMAGE):$(TAG) $(shell echo $(ARCH_LIST) | sed -e "s~[^ ]*~$(AGENT_FULL_IMAGE)\-&:$(TAG)~g")
	@for arch in $(ARCH_LIST); do ${DOCKER_CMD} manifest annotate --arch $${arch} ${AGENT_FULL_IMAGE}:${TAG} ${AGENT_FULL_IMAGE}-$${arch}:${TAG}; done
	${DOCKER_CMD} manifest push --purge $(AGENT_FULL_IMAGE):$(TAG)

.PHONY: docker-push-manifest/proxy-server
docker-push-manifest/proxy-server: ## Push the fat manifest docker image.
	## Minimum docker version 18.06.0 is required for creating and pushing manifest images.
	${DOCKER_CMD} manifest create --amend $(SERVER_FULL_IMAGE):$(TAG) $(shell echo $(ARCH_LIST) | sed -e "s~[^ ]*~$(SERVER_FULL_IMAGE)\-&:$(TAG)~g")
	@for arch in $(ARCH_LIST); do ${DOCKER_CMD} manifest annotate --arch $${arch} ${SERVER_FULL_IMAGE}:${TAG} ${SERVER_FULL_IMAGE}-$${arch}:${TAG}; done
	${DOCKER_CMD} manifest push --purge $(SERVER_FULL_IMAGE):$(TAG)

## --------------------------------------
## Release
## --------------------------------------

.PHONY: release-staging
release-staging: ## Builds and push container images to the staging bucket.
	REGISTRY=$(STAGING_REGISTRY) ARCH_LIST="$(RELEASE_ARCH_LIST)" $(MAKE) docker-push-all release-alias-tag

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
