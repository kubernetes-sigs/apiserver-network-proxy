package util

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

type ClientsetConfig struct {
	Kubeconfig      string
	InClusterConfig bool
	ClientConfig    *ClientConfig
}

type ClientConfig struct {
	QPS            float32
	Burst          int
	APIContentType string
}

func IsRunningInKubernetes() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != "" {
		return true
	}

	return false
}

func (cfg *ClientsetConfig) NewClientset() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	switch {
	case cfg.Kubeconfig != "":
		config, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubernetes client config: %v", err)
		}
	case cfg.InClusterConfig:
		config, err = rest.InClusterConfig()
	default:
		return nil, fmt.Errorf("no valid configuration option provided; either provide kubeconfig path or set --use-in-cluster=true with necessary RBAC permissions")
	}

	cfg.setClientOptions(config)

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	return clientset, nil
}

func (cfg *ClientsetConfig) setClientOptions(config *rest.Config) {

	if cfg.ClientConfig.QPS != 0 {
		klog.V(1).Infof("Setting k8s client QPS: %v", cfg.ClientConfig.QPS)
		config.QPS = cfg.ClientConfig.QPS
	}
	if cfg.ClientConfig.Burst != 0 {
		klog.V(1).Infof("Setting k8s client Burst: %v", cfg.ClientConfig.Burst)
		config.Burst = cfg.ClientConfig.Burst
	}
	if len(cfg.ClientConfig.APIContentType) != 0 {
		klog.V(1).Infof("Setting k8s client Content type: %v", cfg.ClientConfig.APIContentType)
		config.ContentType = cfg.ClientConfig.APIContentType
	}
}
