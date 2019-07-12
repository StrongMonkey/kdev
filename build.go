package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rancher/rio/cli/pkg/clicontext"
	"github.com/rancher/rio/pkg/constructors"
	"github.com/rancher/wrangler/pkg/kv"
	"github.com/sirupsen/logrus"
	appv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Build struct {
	BuildOptions
	Update      bool   `desc:"Whether to update image"`
	N_Namespace string `desc:"Set namespace" default:"default"`
	Selectors   string `desc:"Set selectors"`
}

type BuildOptions struct {
	Dockerfile     string `desc:"Set dockerfile name"`
	DockerfilePath string `desc:"Set dockerfile path"`
	PushRegistry   string `desc:"Set push registry"`
	NoCache        bool   `desc:"Set no-cache"`
	Tag            string `desc:"Set build tag"`
	Push           bool   `desc:"Push image"`
}

func (b *Build) Run(c *clicontext.CLIContext) error {
	if len(c.CLI.Args()) == 0 {
		return fmt.Errorf("at least one argument is required")
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := filepath.Base(workingDir)
	if b.Selectors == "" {
		b.Selectors = fmt.Sprintf("name=%s", strings.Replace(dir, "_", "-", -1))
	}

	deploy, err := findDeployment(c, b.Selectors, b.N_Namespace)
	if err != nil && b.Update {
		return err
	} else if !b.Update {
		deploy = constructors.NewDeployment(b.N_Namespace, dir, appv1.Deployment{})
	}

	buildSpec := buildFile{}
	buildSpec.BuildContext = c.CLI.Args()[0]
	buildSpec.Tag = fmt.Sprintf("%s/%s", b.N_Namespace, deploy.Name)
	mergeBuildOptions(b.BuildOptions, &buildSpec)

	buildConfig := map[string]buildFile{}
	buildConfig = map[string]buildFile{
		deploy.Name: buildSpec,
	}

	buildkitPortForwardStop := make(chan struct{})
	if err := prepareBuildkit(c, b.N_Namespace, buildkitContainerdConfig, buildkitPortForwardStop); err != nil {
		return err
	}
	logrus.Info("Running build")
	imageMeta, err := runBuild(buildConfig, b.N_Namespace, c)
	if err != nil {
		return err
	}

	if b.Update {
		deploy.Spec.Template.Spec.Containers[0].Image = imageMeta[deploy.Name]
		if _, err := c.K8s.AppsV1().Deployments(b.N_Namespace).Update(deploy); err != nil {
			return err
		}
		logrus.Infof("Deployment %s/%s is updated with image %s", deploy.Namespace, deploy.Name, imageMeta[deploy.Name])
	} else {
		fmt.Println(imageMeta[deploy.Name])
	}

	return nil
}

func findDeployment(c *clicontext.CLIContext, selector string, namespace string) (*appv1.Deployment, error) {
	key, value := kv.Split(selector, "=")
	if key == "name" {
		return c.K8s.AppsV1().Deployments(namespace).Get(value, metav1.GetOptions{})
	}
	if key == "label" {
		deploys, err := c.K8s.AppsV1().Deployments(namespace).List(metav1.ListOptions{
			LabelSelector: value,
		})
		if err != nil {
			return nil, err
		}
		if len(deploys.Items) == 0 {
			return nil, fmt.Errorf("no deploy found")
		}
		return &deploys.Items[0], nil
	}
	return nil, fmt.Errorf("Invalid selector key, must be name or label")
}
