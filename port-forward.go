package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/rancher/rio/cli/pkg/clicontext"
	"github.com/rancher/rio/pkg/pretty/stringers"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	portforwardtools "k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/kubernetes/pkg/kubectl/cmd/portforward"
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

func portForward(pod *v1.Pod, c *clicontext.CLIContext, port, targetPort string, stopChan chan struct{}) error {
	loader := kubeconfig.GetInteractiveClientConfig(c.CLI.String("kubeconfig"))

	restConfig, err := loader.ClientConfig()
	if err != nil {
		return err
	}
	if err := setConfigDefaults(restConfig); err != nil {
		return err
	}
	restClient, err := rest.RESTClientFor(restConfig)
	if err != nil {
		return err
	}
	ioStreams := genericclioptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}

	portForwardOpt := portforward.PortForwardOptions{
		Namespace:    pod.Namespace,
		PodName:      pod.Name,
		RESTClient:   restClient,
		Config:       restConfig,
		PodClient:    c.K8s.CoreV1(),
		Address:      []string{"localhost"},
		Ports:        []string{fmt.Sprintf("%s:%s", port, targetPort)},
		StopChannel:  stopChan,
		ReadyChannel: make(chan struct{}),
		PortForwarder: &defaultPortForwarder{
			IOStreams: ioStreams,
		},
	}
	return portForwardOpt.RunPortForward()
}

type defaultPortForwarder struct {
	genericclioptions.IOStreams
}

func (f *defaultPortForwarder) ForwardPorts(method string, url *url.URL, opts portforward.PortForwardOptions) error {
	transport, upgrader, err := spdy.RoundTripperFor(opts.Config)
	if err != nil {
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, method, url)
	fw, err := portforwardtools.NewOnAddresses(dialer, opts.Address, opts.Ports, opts.StopChannel, opts.ReadyChannel, f.Out, f.ErrOut)
	if err != nil {
		return err
	}
	return fw.ForwardPorts()
}
