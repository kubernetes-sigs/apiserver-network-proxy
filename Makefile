.PHONY: gen clean certs build
proto/agent/agent.pb.go: proto/agent/agent.proto
	protoc -I proto proto/agent/agent.proto --go_out=plugins=grpc:proto

bin:
	mkdir -p bin

proto/agent/agent.pb.go: proto/agent/agent.proto
	protoc -I proto proto/agent/agent.proto --go_out=plugins=grpc:proto

proto/proxy.pb.go: proto/proxy.proto
	protoc -I proto proto/proxy.proto --go_out=plugins=grpc:proto

bin/proxy-agent: bin cmd/agent/main.go proto/agent/agent.pb.go
	go build -o bin/proxy-agent cmd/agent/main.go

bin/proxy-server: bin cmd/proxy/main.go proto/agent/agent.pb.go proto/proxy.pb.go
	go build -o bin/proxy-server cmd/proxy/main.go

bin/proxy-test-client: bin cmd/client/main.go proto/proxy.pb.go
	go build -o bin/proxy-test-client cmd/client/main.go

easy-rsa.tar.gz:
	curl -L -O --connect-timeout 20 --retry 6 --retry-delay 2 https://storage.googleapis.com/kubernetes-release/easy-rsa/easy-rsa.tar.gz

easy-rsa-master: easy-rsa.tar.gz
	tar xvf easy-rsa.tar.gz

cfssl:
	curl --retry 10 -L -o cfssl https://pkg.cfssl.org/R1.2/cfssl_linux-amd64
	chmod +x cfssl

cfssljson:
	curl --retry 10 -L -o cfssljson https://pkg.cfssl.org/R1.2/cfssljson_linux-amd64
	chmod +x cfssljson

certs: easy-rsa-master cfssl cfssljson
	cp -rf easy-rsa-master/easyrsa3 easy-rsa-master/master
	cp -rf easy-rsa-master/easyrsa3 easy-rsa-master/agent
	cd easy-rsa-master/master; \
	./easyrsa init-pki; \
	./easyrsa --batch "--req-cn=127.0.0.1@$(date +%s)" build-ca nopass; \
	./easyrsa --subject-alt-name="DNS:kubernetes,IP:127.0.0.1" build-server-full "proxy-master" nopass; \
	./easyrsa build-client-full proxy-client nopass; \
	echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","client auth"]}}}' > "ca-config.json"; \
	echo '{"CN":"proxy","names":[{"O":"system:nodes"}],"hosts":[""],"key":{"algo":"rsa","size":2048}}' | "../../cfssl" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "../../cfssljson" -bare proxy
	mkdir -p certs/master
	cp -r easy-rsa-master/master/pki/private certs/master
	cp -r easy-rsa-master/master/pki/issued certs/master
	cp easy-rsa-master/master/pki/ca.crt certs/master/issued
	cd easy-rsa-master/agent; \
	./easyrsa init-pki; \
	./easyrsa --batch "--req-cn=127.0.0.1@$(date +%s)" build-ca nopass; \
	./easyrsa --subject-alt-name="DNS:kubernetes,IP:127.0.0.1" build-server-full "proxy-master" nopass; \
	./easyrsa build-client-full proxy-agent nopass; \
	echo '{"signing":{"default":{"expiry":"43800h","usages":["signing","key encipherment","agent auth"]}}}' > "ca-config.json"; \
	echo '{"CN":"proxy","names":[{"O":"system:nodes"}],"hosts":[""],"key":{"algo":"rsa","size":2048}}' | "../../cfssl" gencert -ca=pki/ca.crt -ca-key=pki/private/ca.key -config=ca-config.json - | "../../cfssljson" -bare proxy
	mkdir -p certs/agent
	cp -r easy-rsa-master/agent/pki/private certs/agent
	cp -r easy-rsa-master/agent/pki/issued certs/agent
	cp easy-rsa-master/agent/pki/ca.crt certs/agent/issued

gen: proto/agent/agent.pb.go proto/proxy.pb.go

build: bin/proxy-agent bin/proxy-server bin/proxy-test-client

clean:
	rm -rf proto/agent/agent.pb.go proto/proxy.pb.go easy-rsa.tar.gz easy-rsa-master cfssl cfssljson certs bin
