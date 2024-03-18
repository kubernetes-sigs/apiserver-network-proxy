module sigs.k8s.io/apiserver-network-proxy/konnectivity-client

go 1.21

// Prefer to keep requirements compatible with the oldest supported
// k/k minor version, to prevent client backport issues.
require (
	github.com/prometheus/client_golang v1.11.1
	go.uber.org/goleak v1.2.0
	golang.org/x/net v0.17.0 // indirect
	google.golang.org/grpc v1.56.3
	google.golang.org/protobuf v1.33.0
	k8s.io/klog/v2 v2.0.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/go-logr/logr v0.1.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.1 // indirect
	github.com/prometheus/client_model v0.2.0 // indirect
	github.com/prometheus/common v0.26.0 // indirect
	github.com/prometheus/procfs v0.6.0 // indirect
	golang.org/x/sys v0.13.0 // indirect
	golang.org/x/text v0.13.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20230525234030-28d5490b6b19 // indirect
)
