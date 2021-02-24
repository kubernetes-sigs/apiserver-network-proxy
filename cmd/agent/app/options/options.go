package options

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
	"sigs.k8s.io/apiserver-network-proxy/pkg/features"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

type GrpcProxyAgentOptions struct {
	// Configuration for authenticating with the proxy-server
	AgentCert string
	AgentKey  string
	CaCert    string

	// Configuration for connecting to the proxy-server
	ProxyServerHost string
	ProxyServerPort int
	AlpnProtos      []string

	// Ports for the health and admin server
	HealthServerPort int
	AdminServerPort  int
	// Enables pprof at host:adminPort/debug/pprof.
	EnableProfiling bool
	// If EnableProfiling is true, this enables the lock contention
	// profiling at host:adminPort/debug/pprof/block.
	EnableContentionProfiling bool

	AgentID          string
	AgentIdentifiers string
	SyncInterval     time.Duration
	ProbeInterval    time.Duration
	SyncIntervalCap  time.Duration

	// file contains service account authorization token for enabling proxy-server token based authorization
	ServiceAccountTokenPath string

	BindAddress      string
	ApiServerMapping portMapping
}

var _ pflag.Value = &portMapping{}

// port mapping represents the mapping between a local port and a remote
// destination.
type portMapping agent.PortMapping

func (pm *portMapping) String() string {
	return (*agent.PortMapping)(pm).String()
}

func (pm *portMapping) Set(s string) error {
	return (*agent.PortMapping)(pm).Parse(s)
}

func (pm *portMapping) Type() string {
	return "portMapping"
}

func (o *GrpcProxyAgentOptions) ClientSetConfig(dialOptions ...grpc.DialOption) *agent.ClientSetConfig {
	return &agent.ClientSetConfig{
		Address:                 fmt.Sprintf("%s:%d", o.ProxyServerHost, o.ProxyServerPort),
		AgentID:                 o.AgentID,
		AgentIdentifiers:        o.AgentIdentifiers,
		SyncInterval:            o.SyncInterval,
		ProbeInterval:           o.ProbeInterval,
		SyncIntervalCap:         o.SyncIntervalCap,
		DialOptions:             dialOptions,
		ServiceAccountTokenPath: o.ServiceAccountTokenPath,
	}
}

func (o *GrpcProxyAgentOptions) Flags() *pflag.FlagSet {
	flags := pflag.NewFlagSet("proxy-agent", pflag.ContinueOnError)
	flags.StringVar(&o.AgentCert, "agent-cert", o.AgentCert, "If non-empty secure communication with this cert.")
	flags.StringVar(&o.AgentKey, "agent-key", o.AgentKey, "If non-empty secure communication with this key.")
	flags.StringVar(&o.CaCert, "ca-cert", o.CaCert, "If non-empty the CAs we use to validate clients.")
	flags.StringVar(&o.ProxyServerHost, "proxy-server-host", o.ProxyServerHost, "The hostname to use to connect to the proxy-server.")
	flags.IntVar(&o.ProxyServerPort, "proxy-server-port", o.ProxyServerPort, "The port the proxy server is listening on.")
	flags.StringSliceVar(&o.AlpnProtos, "alpn-proto", o.AlpnProtos, "Additional ALPN protocols to be presented when connecting to the server. Useful to distinguish between network proxy and apiserver connections that share the same destination address.")
	flags.IntVar(&o.HealthServerPort, "health-server-port", o.HealthServerPort, "The port the health server is listening on.")
	flags.IntVar(&o.AdminServerPort, "admin-server-port", o.AdminServerPort, "The port the admin server is listening on.")
	flags.BoolVar(&o.EnableProfiling, "enable-profiling", o.EnableProfiling, "enable pprof at host:admin-port/debug/pprof")
	flags.BoolVar(&o.EnableContentionProfiling, "enable-contention-profiling", o.EnableContentionProfiling, "enable contention profiling at host:admin-port/debug/pprof/block. \"--enable-profiling\" must also be set.")
	flags.StringVar(&o.AgentID, "agent-id", o.AgentID, "The unique ID of this agent. Default to a generated uuid if not set.")
	flags.DurationVar(&o.SyncInterval, "sync-interval", o.SyncInterval, "The initial interval by which the agent periodically checks if it has connections to all instances of the proxy server.")
	flags.DurationVar(&o.ProbeInterval, "probe-interval", o.ProbeInterval, "The interval by which the agent periodically checks if its connections to the proxy server are ready.")
	flags.DurationVar(&o.SyncIntervalCap, "sync-interval-cap", o.SyncIntervalCap, "The maximum interval for the SyncInterval to back off to when unable to connect to the proxy server")
	flags.StringVar(&o.ServiceAccountTokenPath, "service-account-token-path", o.ServiceAccountTokenPath, "If non-empty proxy agent uses this token to prove its identity to the proxy server.")
	flags.StringVar(&o.AgentIdentifiers, "agent-identifiers", o.AgentIdentifiers, "Identifiers of the agent that will be used by the server when choosing agent. N.B. the list of identifiers must be in URL encoded format. e.g.,host=localhost&host=node1.mydomain.com&cidr=127.0.0.1/16&ipv4=1.2.3.4&ipv4=5.6.7.8&ipv6=:::::&default-route=true")
	flags.Var(&o.ApiServerMapping, "apiserver-port-mapping", "Mapping between a local port and the host:port used to reach the Kubernetes API Server")
	flags.StringVar(&o.BindAddress, "bind-address", o.BindAddress, "Address used to listen for traffic generated on cluster network")
	// add feature gates flag
	features.DefaultMutableFeatureGate.AddFlag(flags)
	return flags
}

func (o *GrpcProxyAgentOptions) Print() {
	klog.V(1).Infof("AgentCert set to %q.\n", o.AgentCert)
	klog.V(1).Infof("AgentKey set to %q.\n", o.AgentKey)
	klog.V(1).Infof("CACert set to %q.\n", o.CaCert)
	klog.V(1).Infof("ProxyServerHost set to %q.\n", o.ProxyServerHost)
	klog.V(1).Infof("ProxyServerPort set to %d.\n", o.ProxyServerPort)
	klog.V(1).Infof("ALPNProtos set to %+s.\n", o.AlpnProtos)
	klog.V(1).Infof("HealthServerPort set to %d.\n", o.HealthServerPort)
	klog.V(1).Infof("AdminServerPort set to %d.\n", o.AdminServerPort)
	klog.V(1).Infof("EnableProfiling set to %v.\n", o.EnableProfiling)
	klog.V(1).Infof("EnableContentionProfiling set to %v.\n", o.EnableContentionProfiling)
	klog.V(1).Infof("AgentID set to %s.\n", o.AgentID)
	klog.V(1).Infof("SyncInterval set to %v.\n", o.SyncInterval)
	klog.V(1).Infof("ProbeInterval set to %v.\n", o.ProbeInterval)
	klog.V(1).Infof("SyncIntervalCap set to %v.\n", o.SyncIntervalCap)
	klog.V(1).Infof("ServiceAccountTokenPath set to %q.\n", o.ServiceAccountTokenPath)
	klog.V(1).Infof("AgentIdentifiers set to %s.\n", util.PrettyPrintURL(o.AgentIdentifiers))
	if features.DefaultMutableFeatureGate.Enabled(features.NodeToMasterTraffic) {
		klog.V(1).Infof("AgentBindAddress set to %s.\n", o.BindAddress)
		klog.V(1).Infof("Apiserver port mapping set to %s.\n", o.ApiServerMapping.String())
	}
}

func (o *GrpcProxyAgentOptions) Validate() error {
	if o.AgentKey != "" {
		if _, err := os.Stat(o.AgentKey); os.IsNotExist(err) {
			return fmt.Errorf("error checking agent key %s, got %v", o.AgentKey, err)
		}
		if o.AgentCert == "" {
			return fmt.Errorf("cannot have agent cert empty when agent key is set to \"%s\"", o.AgentKey)
		}
	}
	if o.AgentCert != "" {
		if _, err := os.Stat(o.AgentCert); os.IsNotExist(err) {
			return fmt.Errorf("error checking agent cert %s, got %v", o.AgentCert, err)
		}
		if o.AgentKey == "" {
			return fmt.Errorf("cannot have agent key empty when agent cert is set to \"%s\"", o.AgentCert)
		}
	}
	if o.CaCert != "" {
		if _, err := os.Stat(o.CaCert); os.IsNotExist(err) {
			return fmt.Errorf("error checking agent CA cert %s, got %v", o.CaCert, err)
		}
	}
	if o.ProxyServerPort <= 0 {
		return fmt.Errorf("proxy server port %d must be greater than 0", o.ProxyServerPort)
	}
	if o.HealthServerPort <= 0 {
		return fmt.Errorf("health server port %d must be greater than 0", o.HealthServerPort)
	}
	if o.AdminServerPort <= 0 {
		return fmt.Errorf("admin server port %d must be greater than 0", o.AdminServerPort)
	}
	if o.EnableContentionProfiling && !o.EnableProfiling {
		return fmt.Errorf("if --enable-contention-profiling is set, --enable-profiling must also be set")
	}
	if o.SyncInterval > o.SyncIntervalCap {
		return fmt.Errorf("sync interval %v must be less than sync interval cap %v", o.SyncInterval, o.SyncIntervalCap)
	}
	if o.ServiceAccountTokenPath != "" {
		if _, err := os.Stat(o.ServiceAccountTokenPath); os.IsNotExist(err) {
			return fmt.Errorf("error checking service account token path %s, got %v", o.ServiceAccountTokenPath, err)
		}
	}
	if err := validateAgentIdentifiers(o.AgentIdentifiers); err != nil {
		return fmt.Errorf("agent address is invalid: %v", err)
	}
	if err := validateHostnameOrIP(o.BindAddress); err != nil {
		return fmt.Errorf("agent bind address is invalid: %v", err)
	}
	if err := validateHostnameOrIP(o.ApiServerMapping.RemoteHost); err != nil {
		return fmt.Errorf("apiserver address is invalid: %v", err)
	}
	if o.ApiServerMapping.LocalPort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the apiserver local port", o.ApiServerMapping.LocalPort)
	}
	if o.ApiServerMapping.LocalPort < 1024 {
		return fmt.Errorf("please do not try to use reserved port %d for the apiserver local port", o.ApiServerMapping.LocalPort)
	}
	if o.ApiServerMapping.RemotePort > 49151 {
		return fmt.Errorf("please do not try to use ephemeral port %d for the apiserver remote port", o.ApiServerMapping.LocalPort)
	}
	if o.ApiServerMapping.RemotePort < 1 {
		return fmt.Errorf("invalid port %d for the apiserver remote port", o.ApiServerMapping.RemotePort)
	}
	return nil
}

func validateAgentIdentifiers(agentIdentifiers string) error {
	decoded, err := url.ParseQuery(agentIdentifiers)
	if err != nil {
		return err
	}
	for idType := range decoded {
		switch agent.IdentifierType(idType) {
		case agent.IPv4:
		case agent.IPv6:
		case agent.CIDR:
		case agent.Host:
		case agent.DefaultRoute:
		default:
			return fmt.Errorf("unknown address type: %s", idType)
		}
	}
	return nil
}

func validateHostnameOrIP(hostnameOrIP string) error {
	// If it is an IP return immediately otherwise check if it is a hostname
	if net.ParseIP(hostnameOrIP) != nil {
		return nil
	}
	// If it it not a valid hostname return an error
	if errs := validation.IsDNS1123Label(hostnameOrIP); len(errs) > 0 {
		return fmt.Errorf("%s is not a valid ip or hostname: %s", hostnameOrIP, strings.Join(errs, ","))
	}
	return nil
}

func NewGrpcProxyAgentOptions() *GrpcProxyAgentOptions {
	o := GrpcProxyAgentOptions{
		AgentCert:                 "",
		AgentKey:                  "",
		CaCert:                    "",
		ProxyServerHost:           "127.0.0.1",
		ProxyServerPort:           8091,
		HealthServerPort:          8093,
		AdminServerPort:           8094,
		EnableProfiling:           false,
		EnableContentionProfiling: false,
		AgentID:                   uuid.New().String(),
		AgentIdentifiers:          "",
		SyncInterval:              1 * time.Second,
		ProbeInterval:             1 * time.Second,
		SyncIntervalCap:           10 * time.Second,
		ServiceAccountTokenPath:   "",
		ApiServerMapping:          portMapping{LocalPort: 6443, RemoteHost: "localhost", RemotePort: 6443},
		BindAddress:               "127.0.0.1",
	}
	return &o
}
