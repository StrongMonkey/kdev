//go:generate go run pkg/codegen/cleanup/main.go
//go:generate /bin/rm -rf pkg/generated
//go:generate go run pkg/codegen/main.go

package main

import (
	context2 "context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rancher/rio/cli/pkg/builder"
	"github.com/rancher/rio/cli/pkg/clicontext"
	riov1 "github.com/rancher/rio/pkg/generated/clientset/versioned/typed/rio.cattle.io/v1"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

var (
	Version   = "v0.0.0-dev"
	GitCommit = "HEAD"

	appName                  = filepath.Base(os.Args[0])
	cfg                      = clicontext.Config{}
	buildkitContainerdConfig = `
[grpc]
  address = [ "tcp://0.0.0.0:8080" ]
  # debugAddress is address for attaching go profiles and debuggers.
  debugAddress = "0.0.0.0:6060"

[worker.oci]
  enabled = false

[worker.containerd]
  address = "/run/k3s/containerd/containerd.sock"
  enabled = true
  platforms = [ "linux/amd64", "linux/arm64" ]
  namespace = "k8s.io"
  gc = true
  # gckeepstorage sets storage limit for default gc profile, in bytes.
  gckeepstorage = 9000

  [[worker.containerd.gcpolicy]]
    keepBytes = 512000000
    keepDuration = 172800 # in seconds
    filters = [ "type==source.local", "type==exec.cachemount", "type==source.git.checkout"]
  [[worker.containerd.gcpolicy]]
    all = true
    keepBytes = 1024000000
`
	buildkitDockerConfig = `
[grpc]
  address = [ "tcp://0.0.0.0:8080" ]
  # debugAddress is address for attaching go profiles and debuggers.
  debugAddress = "0.0.0.0:6060"
`
)

func main() {
	app := cli.NewApp()
	app.Name = appName
	app.Version = fmt.Sprintf("%s (%s)", Version, GitCommit)
	app.Usage = "Develop and build application on k8s without docker"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "kubeconfig",
			EnvVar: "KUBECONFIG",
			Value:  "${HOME}/.kube/config",
		},
		cli.StringFlag{
			Name:  "debug",
			Value: "Show debug logs",
		},
	}
	app.Commands = []cli.Command{
		builder.Command(&Run{},
			"Build and Run a pod",
			appName+" run [OPTION] $CONTEXT ", "Run a pod based on buildFile, podSpec is defined inside buildFile"),
		builder.Command(&Build{},
			"Build images",
			appName+" build [OPTION] $CONTEXT ", "Build image and update deployment"),
		builder.Command(&Up{},
			"apply build config file",
			appName+" up [OPTION]", "Apply a buildConfig file"),
		builder.Command(&Pf{},
			"forward ports on a pod",
			appName+" pf [OPTION] $pod_name", "Port forwarding"),
			builder.Command(&Pods{},
			"view pods launched by kdev",
				appName+" pf", ""),
	}
	app.Before = func(context *cli.Context) error {
		if context.Bool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}
		cc := clicontext.CLIContext{
			Config: &cfg,
			Ctx:    context2.Background(),
		}
		cc.Store(context.App.Metadata)

		loader := kubeconfig.GetInteractiveClientConfig(context.String("kubeconfig"))

		restConfig, err := loader.ClientConfig()
		if err != nil {
			return err
		}
		k8s := kubernetes.NewForConfigOrDie(restConfig)
		cfg.K8s = k8s

		core, err := corev1.NewForConfig(restConfig)
		if err != nil {
			return err
		}
		rio, err := riov1.NewForConfig(restConfig)
		if err != nil {
			return err
		}

		cfg.Rio = rio
		cfg.Core = core
		return nil
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

type buildFile struct {
	BuildContext   string    `yaml:"build_context,omitempty"`
	Dockerfile     string    `yaml:"dockerfile,omitempty"`
	DockerfilePath string    `yaml:"dockerfile_path,omitempty"`
	PushRegistry   string    `yaml:"push_registry,omitempty"`
	Tag            string    `yaml:"tag,omitempty"`
	NoCache        bool      `yaml:"noCache,omitempty"`
	Push           bool      `yaml:"push,omitempty"`
	Target         Reference `yaml:"target,omitempty"`
}

type Reference struct {
	Kind       string `yaml:"kind,omitempty"`
	APIVersion string `yaml:"apiVersion,omitempty"`
}
