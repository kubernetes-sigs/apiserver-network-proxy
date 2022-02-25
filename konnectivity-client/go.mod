module sigs.k8s.io/apiserver-network-proxy/konnectivity-client

go 1.17

// Prefer to keep requirements compatible with the oldest supported
// k/k minor version, to prevent client backport issues.
require (
	github.com/golang/protobuf v1.4.3
	google.golang.org/grpc v1.27.1
	k8s.io/klog/v2 v2.0.0
)

require (
	github.com/go-logr/logr v0.1.0 // indirect
	go.uber.org/goleak v1.1.10 // indirect
	golang.org/x/lint v0.0.0-20190930215403-16217165b5de // indirect
	golang.org/x/net v0.0.0-20200822124328-c89045814202 // indirect
	golang.org/x/sys v0.0.0-20200323222414-85ca7c5b95cd // indirect
	golang.org/x/text v0.3.0 // indirect
	golang.org/x/tools v0.0.0-20191108193012-7d206e10da11 // indirect
	google.golang.org/genproto v0.0.0-20200526211855-cb27e3aa2013 // indirect
	google.golang.org/protobuf v1.26.0-rc.1 // indirect
)
