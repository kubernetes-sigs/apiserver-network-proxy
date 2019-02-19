package main

import (
	"github.com/anfernee/proxy-service/pkg/agent/agentclient"
)

func main() {
	client := agentclient.NewAgentClient("localhost:8091")

	if err := client.Connect(); err != nil {
		panic(err)
	}

	stopCh := make(chan struct{})

	client.Serve(stopCh)
}
