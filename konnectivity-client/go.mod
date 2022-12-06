module sigs.k8s.io/apiserver-network-proxy/konnectivity-client

go 1.17

// Prefer to keep requirements compatible with the oldest supported
// k/k minor version, to prevent client backport issues.
require (
	github.com/golang/protobuf v1.5.2
	go.uber.org/goleak v1.2.0
	google.golang.org/grpc v1.51.0
	k8s.io/klog/v2 v2.80.1
)

require google.golang.org/protobuf v1.28.1

require (
	github.com/go-logr/logr v1.2.3 // indirect
	golang.org/x/net v0.3.0 // indirect
	golang.org/x/sys v0.3.0 // indirect
	golang.org/x/text v0.5.0 // indirect
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
	google.golang.org/genproto v0.0.0-20221205194025-8222ab48f5fc // indirect
)
