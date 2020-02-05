module sigs.k8s.io/apiserver-network-proxy

go 1.12

require (
	github.com/beorn7/perks v1.0.0 // indirect
	github.com/golang/mock v1.4.0
	github.com/golang/protobuf v1.3.3
	github.com/google/uuid v1.1.1
	github.com/imdario/mergo v0.3.8 // indirect
	github.com/inconshreveable/mousetrap v1.0.0 // indirect
	github.com/prometheus/client_golang v0.9.2
	github.com/prometheus/common v0.4.0 // indirect
	github.com/prometheus/procfs v0.0.0-20190507164030-5867b95ac084 // indirect
	github.com/spf13/cobra v0.0.3
	github.com/spf13/pflag v1.0.5
	golang.org/x/lint v0.0.0-20190313153728-d0100b6bd8b3 // indirect
	golang.org/x/time v0.0.0-20191024005414-555d28b269f0 // indirect
	golang.org/x/tools v0.0.0-20190524140312-2c0ae7006135 // indirect
	google.golang.org/grpc v1.27.0
	honnef.co/go/tools v0.0.0-20190523083050-ea95bdfd59fc // indirect
	k8s.io/api v0.17.2
	k8s.io/apimachinery v0.17.2
	k8s.io/client-go v11.0.0+incompatible
	k8s.io/klog v1.0.0
	k8s.io/utils v0.0.0-20200124190032-861946025e34 // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.4
)

replace sigs.k8s.io/apiserver-network-proxy/konnectivity-client => ./konnectivity-client
