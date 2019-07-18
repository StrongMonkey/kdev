package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/containerd/console"
	"github.com/docker/cli/cli/streams"
	client2 "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/cmd/buildctl/build"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/rancher/rio/cli/pkg/clicontext"
	"github.com/rancher/rio/pkg/constructors"
	"github.com/rancher/wrangler/pkg/kv"
	"github.com/rancher/wrangler/pkg/name"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	appv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

func buildInternal(buildSpec *buildFile, c *clicontext.CLIContext) (string, error) {
	if containerRuntime(c) == "" {
		return buildInternalContainerd(buildSpec, c)
	}
	image, err := buildInternalDocker(buildSpec, c)
	if err != nil {
		return "", err
	}
	if strings.Contains(image, "@") {
		repo, digest := kv.Split(image, "@")
		return fmt.Sprintf("%s:%s", repo, name.Hex(digest, 5)), nil
	}
	return image, nil
}

func initializeBuildkitClient(buildSpec *buildFile, c *clicontext.CLIContext) (*client.Client, client.SolveOpt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	attachable := []session.Attachable{authprovider.NewDockerAuthProvider()}

	buildkitClient, err := client.New(ctx, "tcp://localhost:9000")
	if err != nil {
		return nil, client.SolveOpt{}, err
	}

	solveOpt := client.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename": buildSpec.Dockerfile,
		},
		LocalDirs: map[string]string{
			"context":    buildSpec.BuildContext,
			"dockerfile": buildSpec.DockerfilePath,
		},
		Session: attachable,
	}
	if buildSpec.NoCache {
		solveOpt.FrontendAttrs["no-cache"] = ""
	}
	return buildkitClient, solveOpt, nil
}

func buildInternalDocker(buildSpec *buildFile, c *clicontext.CLIContext) (string, error) {
	buildkitClient, solveOpt, err := initializeBuildkitClient(buildSpec, c)
	if err != nil {
		return "", nil
	}
	var exportFormat string
	var outputFile *os.File
	image := fmt.Sprintf("%s/%s", buildSpec.PushRegistry, buildSpec.Tag)
	if !strings.Contains(buildSpec.Tag, "@") {
		exportFormat = fmt.Sprintf("type=image,name=%s", image)
		if buildSpec.Push {
			exportFormat += ",push=true"
		}
	} else {
		outputFile, err = ioutil.TempFile("", "docker-image")
		if err != nil {
			return "", err
		}
		repo, digest := kv.Split(buildSpec.Tag, "@")
		exportFormat = fmt.Sprintf("type=docker,name=%s:%s,dest=%s", repo, name.Hex(digest, 5), outputFile.Name())
	}

	exports, err := build.ParseOutput([]string{exportFormat})
	if err != nil {
		return "", err
	}
	if strings.Contains(buildSpec.Tag, "@") {
		exports[0].Output = outputFile
	}
	solveOpt.Exports = exports

	ch := make(chan *client.SolveStatus)
	eg, ctx := errgroup.WithContext(c.Ctx)
	displayCh := ch

	var digest string
	eg.Go(func() error {
		resp, err := buildkitClient.Solve(ctx, nil, solveOpt, ch)
		if err != nil {
			return err
		}
		for k, v := range resp.ExporterResponse {
			if k == "containerimage.digest" {
				digest = v
			}
		}

		// this is a hack that when exporting to repoDigest will not be exported if digest is not specified
		if !strings.Contains(buildSpec.Tag, ":") {
			newBuildSpec := *buildSpec
			newBuildSpec.Tag += "@" + digest
			_, err = buildInternalDocker(&newBuildSpec, c)
			if err != nil {
				return err
			}
		}
		return nil
	})

	eg.Go(func() error {
		var c console.Console
		return progressui.DisplaySolveStatus(context.TODO(), "", c, os.Stderr, displayCh)
	})
	if err := eg.Wait(); err != nil {
		return "", err
	}

	if strings.Contains(buildSpec.Tag, "@") {
		os.Setenv("DOCKER_HOST", "tcp://localhost:9001")
		os.Setenv("DOCKER_API_VERSION", "1.35")
		dockerClient, err := client2.NewClientWithOpts(client2.FromEnv)
		if err != nil {
			return "", err
		}
		f, err := os.Open(outputFile.Name())
		if err != nil {
			return "", err
		}
		defer f.Close()

		resp, err := dockerClient.ImageLoad(context.Background(), f, true)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		if resp.Body != nil && resp.JSON {
			if err := jsonmessage.DisplayJSONMessagesToStream(resp.Body, streams.NewOut(os.Stdout), nil); err != nil {
				return "", err
			}
		}
	}

	if !strings.Contains(buildSpec.Tag, ":") && !strings.Contains(buildSpec.Tag, "@") {
		image = fmt.Sprintf("%s/%s@%s", buildSpec.PushRegistry, buildSpec.Tag, digest)
	}

	return image, nil
}

func buildInternalContainerd(buildSpec *buildFile, c *clicontext.CLIContext) (string, error) {
	buildkitClient, solveOpt, err := initializeBuildkitClient(buildSpec, c)
	if err != nil {
		return "", nil
	}

	image := fmt.Sprintf("%s/%s", buildSpec.PushRegistry, buildSpec.Tag)
	exportFormat := "type=image,name=" + image
	if buildSpec.Push {
		exportFormat += ",push=true"
	}
	exports, err := build.ParseOutput([]string{exportFormat})
	if err != nil {
		return "", err
	}
	solveOpt.Exports = exports

	ch := make(chan *client.SolveStatus)
	eg, ctx := errgroup.WithContext(c.Ctx)
	displayCh := ch

	var digest string
	eg.Go(func() error {
		resp, err := buildkitClient.Solve(ctx, nil, solveOpt, ch)
		if err != nil {
			return err
		}
		for k, v := range resp.ExporterResponse {
			if k == "containerimage.digest" {
				digest = v
			}
		}
		// this is a hack that when exporting to repoDigest will not be exported if digest is not specified
		if !strings.Contains(buildSpec.Tag, ":") {
			newBuildSpec := *buildSpec
			newBuildSpec.Tag += "@" + digest
			_, err = buildInternalContainerd(&newBuildSpec, c)
			if err != nil {
				return err
			}
		}
		return nil
	})

	eg.Go(func() error {
		var c console.Console
		return progressui.DisplaySolveStatus(context.TODO(), "", c, os.Stderr, displayCh)
	})
	if err := eg.Wait(); err != nil {
		return "", err
	}
	if !strings.Contains(buildSpec.Tag, ":") && !strings.Contains(buildSpec.Tag, "@") {
		image = fmt.Sprintf("%s/%s@%s", buildSpec.PushRegistry, buildSpec.Tag, digest)
	}

	return image, nil
}

func buildkitDeployment(namespace string, c *clicontext.CLIContext) *appv1.Deployment {
	deploy := &appv1.Deployment{}
	deploy.Name = "buildkit"
	deploy.Namespace = namespace
	deploy.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": "buildkitd-dev",
		},
	}
	hostPathDirectoryType := v1.HostPathDirectory
	hostPathDirectoryOrCreateType := v1.HostPathDirectoryOrCreate
	deploy.Spec.Template = v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "buildkitd-dev",
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "buildkitd",
					Image: "moby/buildkit:v0.5.1",
					Ports: []v1.ContainerPort{
						{
							ContainerPort: 8080,
						},
					},
					SecurityContext: &v1.SecurityContext{
						Privileged: &[]bool{true}[0],
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "config",
							MountPath: "/etc/buildkit/buildkitd.toml",
							SubPath:   "buildkitd.toml",
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "config",
					VolumeSource: v1.VolumeSource{
						ConfigMap: &v1.ConfigMapVolumeSource{
							LocalObjectReference: v1.LocalObjectReference{
								Name: "buildkitd-config",
							},
						},
					},
				},
			},
		},
	}
	if containerRuntime(c) == "" {
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = append(deploy.Spec.Template.Spec.Containers[0].VolumeMounts, []v1.VolumeMount{
			{
				Name:      "rancher",
				MountPath: "/var/lib/rancher",
			},
			{
				Name:      "run",
				MountPath: "/run",
			},
			{
				Name:      "buildkit",
				MountPath: "/var/lib/buildkit",
			},
		}...)
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, []v1.Volume{
			{
				Name: "rancher",
				VolumeSource: v1.VolumeSource{
					HostPath: &v1.HostPathVolumeSource{
						Type: &hostPathDirectoryType,
						Path: "/var/lib/rancher",
					},
				},
			},
			{
				Name: "run",
				VolumeSource: v1.VolumeSource{
					HostPath: &v1.HostPathVolumeSource{
						Type: &hostPathDirectoryType,
						Path: "/run",
					},
				},
			},
			{
				Name: "buildkit",
				VolumeSource: v1.VolumeSource{
					HostPath: &v1.HostPathVolumeSource{
						Type: &hostPathDirectoryOrCreateType,
						Path: "/var/lib/buildkit",
					},
				},
			},
		}...)
	}
	return deploy
}

func prepareBuildkit(c *clicontext.CLIContext, namespace string, stopChan chan struct{}) error {
	config := buildkitContainerdConfig
	if containerRuntime(c) == "minikube" {
		config = buildkitDockerConfig
	}

	configName := "buildkitd-config"
	buildkitdConfig := constructors.NewConfigMap(namespace, configName, v1.ConfigMap{
		Data: map[string]string{
			"buildkitd.toml": config,
		},
	})
	if existing, err := c.Core.ConfigMaps(namespace).Get(configName, metav1.GetOptions{}); err != nil && !errors.IsNotFound(err) {
		return err
	} else if err != nil {
		if _, err := c.Core.ConfigMaps(namespace).Create(buildkitdConfig); err != nil {
			return err
		}
	} else {
		existing.Data = buildkitdConfig.Data
		if _, err := c.Core.ConfigMaps(namespace).Update(existing); err != nil {
			return err
		}
	}
	buildkitdDeploy := buildkitDeployment(namespace, c)
	if existing, err := c.K8s.AppsV1().Deployments(buildkitdDeploy.Namespace).Get(buildkitdDeploy.Name, metav1.GetOptions{}); err != nil && !errors.IsNotFound(err) {
		return err
	} else if errors.IsNotFound(err) {
		if _, err := c.K8s.AppsV1().Deployments(buildkitdDeploy.Namespace).Create(buildkitdDeploy); err != nil {
			return err
		}
	} else {
		existing.Spec = buildkitdDeploy.Spec
		if _, err := c.K8s.AppsV1().Deployments(namespace).Update(existing); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait.JitterUntil(func() {
		if deploy, err := c.K8s.AppsV1().Deployments(buildkitdDeploy.Namespace).Get(buildkitdDeploy.Name, metav1.GetOptions{}); err == nil {
			if isReady(&deploy.Status) {
				cancel()
				return
			}
			logrus.Info("Waiting for buildkitd deploy to be ready")
		}
	}, time.Second, 1.5, false, ctx.Done())
	buildkitdPod, err := findPod(c, namespace, "app=buildkitd-dev")
	if err != nil {
		return err
	}
	go func() {
		if err := portForward(buildkitdPod, c, "9000", "8080", stopChan); err != nil {
			logrus.Error(err)
		}
	}()
	time.Sleep(time.Second)

	if containerRuntime(c) == "minikube" {
		if err := prepareDockerSocat(c, minikubeDockerSocat, stopChan); err != nil {
			return err
		}
	}
	return nil
}
