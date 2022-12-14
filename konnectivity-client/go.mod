module sigs.k8s.io/apiserver-network-proxy/konnectivity-client

go 1.17

// Prefer to keep requirements compatible with the oldest supported
// k/k minor version, to prevent client backport issues.
require (
	github.com/golang/protobuf v1.4.3
	github.com/prometheus/client_golang v1.0.0
	go.uber.org/goleak v1.1.10
	google.golang.org/grpc v1.27.1
	k8s.io/klog/v2 v2.0.0
)

require (
	github.com/beorn7/perks v1.0.0 // indirect
	github.com/go-logr/logr v0.1.0 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.1 // indirect
	github.com/prometheus/client_model v0.0.0-20190812154241-14fe0d1b01d4 // indirect
	github.com/prometheus/common v0.4.1 // indirect
	github.com/prometheus/procfs v0.0.2 // indirect
	golang.org/x/lint v0.0.0-20190930215403-16217165b5de // indirect
	golang.org/x/net v0.0.0-20190620200207-3b0461eec859 // indirect
	golang.org/x/sys v0.0.0-20190215142949-d0b11bdaac8a // indirect
	golang.org/x/text v0.3.0 // indirect
	golang.org/x/tools v0.0.0-20191108193012-7d206e10da11 // indirect
	google.golang.org/genproto v0.0.0-20190819201941-24fa4b261c55 // indirect
	google.golang.org/protobuf v1.23.0 // indirect
)
