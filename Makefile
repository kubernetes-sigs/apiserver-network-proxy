.PHONY: gen
gen:
	protoc -I proto proto/proxy.proto --go_out=plugins=grpc:proto
	protoc -I proto proto/agent/agent.proto --go_out=plugins=grpc:proto
