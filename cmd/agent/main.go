package main

import (
	"github.com/anfernee/proxy-service/pkg/agent/agentclient"
	"github.com/golang/glog"
	"os"
	"github.com/spf13/cobra"
)

func main() {
	agent := &Agent{}
	command := newAgentCommand(agent)
	if err := command.Execute(); err != nil {
		glog.Errorf( "error: %v\n", err)
		os.Exit(1)
	}
}


func newAgentCommand(a *Agent) *cobra.Command {
	cmd := &cobra.Command{
		Use:  "agent",
		Long: `A gRPC agent, Connects to the proxy and then allows traffic to be forwarded to it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.run()
		},
	}

	return cmd
}

type Agent struct {

}

func (a *Agent) run() error {
	client := agentclient.NewAgentClient("localhost:8091")

	if err := client.Connect(); err != nil {
		return err
	}

	stopCh := make(chan struct{})

	client.Serve(stopCh)
	return nil
}
