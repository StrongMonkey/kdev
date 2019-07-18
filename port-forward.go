package main

import (
	"fmt"
	"github.com/rancher/rio/cli/pkg/clicontext"
	"github.com/rancher/rio/pkg/pretty/stringers"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strconv"
)

type Pf struct {
	P_Port      string `desc:"Set port forward, example 8080:80"`
	N_Namespace string `desc:"Set namespace" default:"default"`
}

func (p *Pf) Run(c *clicontext.CLIContext) error {
	if len(c.CLI.Args()) == 0 {
		return fmt.Errorf("pod name is required")
	}
	pod, err := c.Core.Pods(p.N_Namespace).Get(c.CLI.Args()[0], metav1.GetOptions{})
	if err != nil {
		return err
	}
	publishPorts, err := stringers.ParsePorts(p.P_Port)
	if err != nil {
		return err
	}
	// only support forwarding one port
	for _, publishPort := range publishPorts {
		p := publishPort
		stop := make(chan struct{})
		go func() {
			select {
			case <-c.Ctx.Done():
				stop <- struct{}{}
			}
		}()
		logrus.Infof("Forwarding port %v to %v", p.Port, p.TargetPort)
		if err := portForward(pod, c, strconv.Itoa(int(p.Port)), strconv.Itoa(int(p.TargetPort)), stop); err != nil {
			logrus.Error(err)
		}
	}
	return nil
}
