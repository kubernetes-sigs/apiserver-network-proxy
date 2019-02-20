.PHONY: gen clean
proto/agent/agent.pb.go: proto/agent/agent.proto
	protoc -I proto proto/agent/agent.proto --go_out=plugins=grpc:proto

proto/proxy.pb.go: proto/proxy.proto
	protoc -I proto proto/proxy.proto --go_out=plugins=grpc:proto

gen: proto/agent/agent.pb.go proto/proxy.pb.go

clean:
	rm proto/agent/agent.pb.go proto/proxy.pb.go
