package agentclient

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"k8s.io/klog"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	// "k8s.io/client-go/util/retry"
)

const (
	defaultRetry    = 20
	defaultInterval = 5 * time.Second
)

type ReconnectError struct {
	internalErr error
	errChan     <-chan error
}

func (e *ReconnectError) Error() string {
	return "transient error: " + e.internalErr.Error()
}

func (e *ReconnectError) Wait() error {
	return <-e.errChan
}

type RedialableAgentClient struct {
	stream     agent.AgentService_ConnectClient
	streamLock sync.Mutex

	// connect opts
	address       string
	opts          []grpc.DialOption
	conn          *grpc.ClientConn
	stopCh        chan struct{}
	reconnOngoing bool
	reconnWaiters []chan error

	// locks
	sendLock   sync.Mutex
	recvLock   sync.Mutex
	reconnLock sync.Mutex

	// Retry times to reconnect to proxy server
	Retry int

	// Interval between every reconnect
	Interval time.Duration
}

func NewRedialableAgentClient(address string, opts ...grpc.DialOption) (*RedialableAgentClient, error) {
	c := &RedialableAgentClient{
		address:  address,
		opts:     opts,
		Retry:    defaultRetry,
		Interval: defaultInterval,
		stopCh:   make(chan struct{}),
	}

	return c, c.Connect()
}

func (c *RedialableAgentClient) probe() {
	for {
		select {
		case <-c.stopCh:
			return
		case <-time.After(c.Interval):
			// health check
			conn := c.getConn()
			if conn != nil && conn.GetState() == connectivity.Ready {
				continue
			} else {
				klog.Infof("Connection state %v", conn.GetState())
			}
		}

		klog.Info("probe failure: reconnect")
		if err := <-c.triggerReconnect(); err != nil {
			klog.Infof("probe reconnect failed: %v", err)
		}
	}
}

func (c *RedialableAgentClient) Send(pkt *agent.Packet) error {
	c.sendLock.Lock()
	defer c.sendLock.Unlock()

	stream := c.getStream()

	if err := stream.Send(pkt); err != nil {
		if err == io.EOF {
			return err
		}
		return &ReconnectError{
			internalErr: err,
			errChan:     c.triggerReconnect(),
		}
	}

	return nil
}

func (c *RedialableAgentClient) RetrySend(pkt *agent.Packet) error {
	err := c.Send(pkt)
	if err == nil {
		return nil
	} else if err == io.EOF {
		return err
	}

	if err2, ok := err.(*ReconnectError); ok {
		err = err2.Wait()
	}
	if err != nil {
		return err
	}
	return c.RetrySend(pkt)
}

func (c *RedialableAgentClient) triggerReconnect() <-chan error {
	c.reconnLock.Lock()
	defer c.reconnLock.Unlock()

	errch := make(chan error)
	c.reconnWaiters = append(c.reconnWaiters, errch)

	if !c.reconnOngoing {
		go c.reconnect()
		c.reconnOngoing = true
	}

	return errch
}

func (c *RedialableAgentClient) doneReconnect(err error) {
	c.reconnLock.Lock()
	defer c.reconnLock.Unlock()

	for _, ch := range c.reconnWaiters {
		ch <- err
	}
	c.reconnOngoing = false
	c.reconnWaiters = nil
}

func (c *RedialableAgentClient) Recv() (*agent.Packet, error) {
	c.recvLock.Lock()
	defer c.recvLock.Unlock()

	var pkt *agent.Packet
	var err error

	stream := c.getStream()

	if pkt, err = stream.Recv(); err != nil {
		if err == io.EOF {
			return pkt, err
		}
		return pkt, &ReconnectError{
			internalErr: err,
			errChan:     c.triggerReconnect(),
		}
	}

	return pkt, nil
}

func (c *RedialableAgentClient) Connect() error {
	if err := c.tryConnect(); err != nil {
		return err
	}

	go c.probe()
	return nil
}

func (c *RedialableAgentClient) reconnect() {
	klog.Info("start to connect...")

	var err error
	var retry int

	for retry < c.Retry {
		if err = c.tryConnect(); err == nil {
			klog.Info("connected")
			c.doneReconnect(nil)
			return
		}
		retry++
		klog.V(5).Infof("Failed to connect to proxy server, retry %d in %v: %v", retry, c.Interval, err)
		klog.Infof("Failed to connect to proxy server, retry %d in %v: %v", retry, c.Interval, err)
		time.Sleep(c.Interval)
	}

	c.doneReconnect(fmt.Errorf("Failed to connect to proxy server: %v", err))
}

func (c *RedialableAgentClient) getStream() agent.AgentService_ConnectClient {
	c.streamLock.Lock()
	defer c.streamLock.Unlock()

	return c.stream
}

func (c *RedialableAgentClient) getConn() *grpc.ClientConn {
	c.streamLock.Lock()
	defer c.streamLock.Unlock()

	return c.conn
}

func (c *RedialableAgentClient) setStreamAndConn(stream agent.AgentService_ConnectClient, conn *grpc.ClientConn) {
	c.streamLock.Lock()
	defer c.streamLock.Unlock()

	c.conn = conn
	c.stream = stream
}

func (c *RedialableAgentClient) tryConnect() error {
	var err error

	conn, err := grpc.Dial(c.address, c.opts...)
	if err != nil {
		return err
	}

	stream, err := agent.NewAgentServiceClient(conn).Connect(context.Background())
	if err != nil {
		return err
	}

	c.setStreamAndConn(stream, conn)

	return nil
}

// interrupt interrupt the stream connection. (For testing purpose)
func (c *RedialableAgentClient) interrupt() {
	c.getConn().Close()
}
