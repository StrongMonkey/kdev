package main

import (
	"io/ioutil"
	"os"

	"github.com/rancher/rio/cli/pkg/clicontext"
	riov1 "github.com/rancher/rio/pkg/apis/rio.cattle.io/v1"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	appv1 "k8s.io/api/apps/v1"
	extensionv1 "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Up struct {
	F_File      string `desc:"Specify build file"`
	N_Namespace string `desc:"Set namespace" default:"default"`
}

func (u *Up) Run(c *clicontext.CLIContext) error {
	buildConfig, err := u.loadBuildFile()
	if err != nil {
		return err
	}

	buildkitPortForwardStop := make(chan struct{})
	buildkitConfig := buildkitDockerConfig
	if containerRuntime() == "" {
		buildkitConfig = buildkitContainerdConfig
	}
	if err := prepareBuildkit(c, u.N_Namespace, buildkitConfig, buildkitPortForwardStop); err != nil {
		return err
	}

	imageMeta, err := runBuild(buildConfig, u.N_Namespace, c)
	if err != nil {
		return err
	}

	for name, config := range buildConfig {
		if err := updateImage(c, config.Target.APIVersion, config.Target.Kind, u.N_Namespace, name, imageMeta); err != nil {
			return err
		}
	}
	return nil
}

func updateImage(c *clicontext.CLIContext, apiVersion, kind, namespace, name string, imageMeta map[string]string) error {
	if apiVersion == riov1.SchemeGroupVersion.String() && kind == "Service" {
		service, err := c.Rio.Services(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		logrus.Infof("Service.rio.cattle.io %s/%s is updated with image %s", namespace, name, imageMeta[name])
		service.Spec.Image = imageMeta[name]
		if _, err := c.Rio.Services(namespace).Update(service); err != nil {
			return err
		}

	} else if (apiVersion == appv1.SchemeGroupVersion.String() || apiVersion == extensionv1.SchemeGroupVersion.String()) && kind == "Deployment" {
		deploy, err := c.K8s.AppsV1().Deployments(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		logrus.Infof("Deployment %s/%s is updated with image %s", namespace, name, imageMeta[name])
		deploy.Spec.Template.Spec.Containers[0].Image = imageMeta[name]
		if _, err := c.K8s.AppsV1().Deployments(namespace).Update(deploy); err != nil {
			return err
		}
	}
	return nil

}

func (u *Up) loadBuildFile() (map[string]buildFile, error) {
	buildConfig := map[string]buildFile{}
	fileName := "./.kdev-config"
	if u.F_File != "" {
		fileName = u.F_File
	}
	if _, err := os.Stat(fileName); err == nil {
		data, err := ioutil.ReadFile(fileName)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, buildConfig); err != nil {
			return nil, err
		}
	}
	setDefaults(buildConfig, u.N_Namespace)
	return buildConfig, nil
}
