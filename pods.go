package main

import (
	"fmt"
	"github.com/rancher/rio/cli/cmd/ps"
	"github.com/rancher/rio/cli/pkg/clicontext"
	"github.com/rancher/rio/cli/pkg/lookup"
	"github.com/rancher/rio/cli/pkg/tables"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Pods struct {
	N_Namespace string `desc:"Set namespace" default:"default"`
}

func (p *Pods) Run(c *clicontext.CLIContext) error {
	pods, err := c.Core.Pods(p.N_Namespace).List(metav1.ListOptions{
		LabelSelector: "kdev=true",
	})
	if err != nil {
		return err
	}

	writer := tables.NewPods(c)
	defer writer.TableWriter().Close()

	for _, pd := range pods.Items {
		writer.TableWriter().Write(ps.ContainerData{
			Name:    fmt.Sprintf("%s/%s", pd.Namespace, pd.Name),
			PodData: &tables.PodData{
				Pod: &pd,
				Name: pd.Name,
				Service: &lookup.StackScoped{},
			},
		})
	}

	return writer.TableWriter().Err()
}