package agentclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"k8s.io/klog"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

const (
	defaultRetry    = 20
	defaultInterval = 5 * time.Second
)

type RedialableAgentClient struct {
	stream agent.AgentService_ConnectClient

	// connect opts
	address       string
	opts          []grpc.DialOption
	conn          *grpc.ClientConn
	stopCh        chan struct{}
	reconnTrigger chan struct{}

	// locks
	sendLock   sync.Mutex
	recvLock   sync.Mutex
	reconnLock sync.Mutex

	// Retry times to reconnect to proxy server
	Retry int

	// Interval between every reconnect
	Interval time.Duration
}

func NewRedialableAgentClient(address string, opts ...grpc.DialOption) *RedialableAgentClient {
	c := &RedialableAgentClient{
		address:  address,
		opts:     opts,
		Retry:    defaultRetry,
		Interval: defaultInterval,
		stopCh:   make(chan struct{}),
	}

	c.reconnect()

	go c.probe()

	return c
}

func (c *RedialableAgentClient) probe() {
	for {
		select {
		case <-c.stopCh:
			return
		case <-time.After(c.Interval):
			// health check
			if c.conn != nil && c.conn.GetState() == connectivity.Ready {
				continue
			} else {
				klog.Infof("Connection state %v", c.conn.GetState())
			}
		}

		c.reconnect()
	}
}

func (c *RedialableAgentClient) Send(pkt *agent.Packet) error {
	c.sendLock.Lock()
	defer c.sendLock.Unlock()

	if err := c.stream.Send(pkt); err != nil {
		if c.conn.GetState() == connectivity.Ready {
			return err
		}

		if err2 := c.reconnect(); err2 != nil {
			return err2
		}
	}

	return c.stream.Send(pkt)
}

func (c *RedialableAgentClient) Recv() (*agent.Packet, error) {
	c.recvLock.Lock()
	defer c.recvLock.Unlock()

	// this just get block..
	if pkt, err := c.stream.Recv(); err != nil {
		klog.Infof("error recving: %v", err)
		klog.Info("start reconnecting")

		if err2 := c.reconnect(); err2 != nil {
			klog.Infof("reconnect failed: %v", err2)
			return pkt, err2
		}

		return c.stream.Recv()
	} else {
		return pkt, err
	}
}

func (c *RedialableAgentClient) reconnect() error {
	c.reconnLock.Lock()
	defer c.reconnLock.Unlock()

	if c.conn != nil && c.conn.GetState() == connectivity.Ready {
		return nil
	}

	klog.Info("start to connect...")

	var err error
	var retry int

	for retry < c.Retry {
		if err = c.tryConnect(); err == nil {
			klog.Info("connected")
			return nil
		}
		retry++
		klog.V(5).Infof("Failed to connect to proxy server, retry %d in %v: %v", retry, c.Interval, err)
		time.Sleep(c.Interval)
	}

	return fmt.Errorf("Failed to connect to proxy server: %v", err)
}

func (c *RedialableAgentClient) tryConnect() error {
	var err error

	c.conn, err = grpc.Dial(c.address, c.opts...)
	if err != nil {
		return err
	}

	c.stream, err = agent.NewAgentServiceClient(c.conn).Connect(context.Background())
	return err
}

// interrupt interrupt the stream connection. (For testing purpose)
func (c *RedialableAgentClient) interrupt() {
	c.conn.Close()
}
