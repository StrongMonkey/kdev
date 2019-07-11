//go:generate go run pkg/codegen/cleanup/main.go
//go:generate /bin/rm -rf pkg/generated
//go:generate go run pkg/codegen/main.go

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rio/pkg/constructors"
	context2 "context"
	"github.com/containerd/console"
	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/mattn/go-shellwords"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/cmd/buildctl/build"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/rancher/rio/cli/pkg/builder"
	"github.com/rancher/rio/cli/pkg/clicontext"
	"github.com/rancher/rio/cli/pkg/kvfile"
	"github.com/rancher/rio/cli/pkg/stack"
	"github.com/rancher/rio/pkg/pretty/stringers"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/rancher/wrangler/pkg/kv"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
	appv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	portforwardtools "k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/kubernetes/pkg/kubectl/cmd/portforward"
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
)

func main() {
	app := cli.NewApp()
	app.Name = appName
	app.Version = fmt.Sprintf("%s (%s)", Version, GitCommit)
	app.Usage = "Develop and build aaplication on k8s without docker"
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
			"Run a pod based on buildfile",
			appName+" run ${args}", "Run a pod based on buildFile, podSpec is defined inside buildFile"),
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
		cfg.Core = core
		return nil
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

type buildFile struct {
	BuildContext           string `json:"buildContext,omitempty"`
	DockerFile             string `json:"dockerFile,omitempty"`
	PushRegistry           string `json:"pushRegistry,omitempty"`
	PushRegistrySecretName string `json:"pushRegistrySecretName,omitempty"`
	BuildImageName         string `json:"buildImageName,omitempty"`
	Tag                    string `json:"tag,omitempty"`
}

type Run struct {
	AddHost                []string          `desc:"Add a custom host-to-IP mapping (host:ip)"`
	Annotations            map[string]string `desc:"Annotations to attach to this pod"`
	Command                []string          `desc:"Overwrite the default ENTRYPOINT of the image"`
	Config                 []string          `desc:"Configs to expose to the service (format: name:target)"`
	Cpus                   string            `desc:"Number of CPUs"`
	DNSOption              []string          `desc:"Set DNS options (format: key:value or key)"`
	DNSSearch              []string          `desc:"Set custom DNS search domains"`
	DNS                    []string          `desc:"Set custom DNS servers"`
	E_Env                  []string          `desc:"Set environment variables"`
	EnvFile                []string          `desc:"Read in a file of environment variables"`
	Group                  string            `desc:"The GID to run the entrypoint of the container process"`
	HealthCmd              string            `desc:"Command to run to check health"`
	HealthFailureThreshold int               `desc:"Consecutive failures needed to report unhealthy"`
	HealthHeader           map[string]string `desc:"HTTP Headers to send in GET request for healthcheck"`
	HealthInitialDelay     string            `desc:"Start period for the container to initialize before starting healthchecks (ms|s|m|h)" default:"0s"`
	HealthInterval         string            `desc:"Time between running the check (ms|s|m|h)" default:"0s"`
	HealthSuccessThreshold int               `desc:"Consecutive successes needed to report healthy"`
	HealthTimeout          string            `desc:"Maximum time to allow one check to run (ms|s|m|h)" default:"0s"`
	HealthURL              string            `desc:"URL to hit to check health (example: http://localhost:8080/ping)"`
	Hostname               string            `desc:"Container host name"`
	I_Interactive          bool              `desc:"Keep STDIN open even if not attached"`
	ImagePullPolicy        string            `desc:"Behavior determining when to pull the image (never|always|not-present)" default:"not-present"`
	ImagePullSecrets       []string          `desc:"Specify image pull secrets"`
	LabelFile              []string          `desc:"Read in a line delimited file of labels"`
	L_Label                map[string]string `desc:"Set meta data on a container"`
	M_Memory               string            `desc:"Memory reservation (format: <number>[<unit>], where unit = b, k, m or g)"`
	N_Name                 string            `desc:"Assign a name to the container"`
	P_Ports                []string          `desc:"Publish a container's port(s) externally (default: \"80:8080/http\")"`
	ReadOnly               bool              `desc:"Mount the container's root filesystem as read only"`
	Secret                 []string          `desc:"Secrets to inject to the service (format: name:target)"`
	T_Tty                  bool              `desc:"Allocate a pseudo-TTY"`
	Volume                 []string          `desc:"Specify volumes"`
	U_User                 string            `desc:"UID[:GID] Sets the UID used and optionally GID for entrypoint process (format: <uid>[:<gid>])"`
	W_Workdir              string            `desc:"Working directory inside the container"`
}

func (r *Run) Run(c *clicontext.CLIContext) error {
	pod, err := r.preparePod(c)
	if err != nil {
		return err
	}

	if pod.Name == "" {
		rand.Seed(time.Now().UnixNano())
		pod.Name = strings.Replace(namesgenerator.GetRandomName(2), "_", "-", -1)
		pod.Spec.Containers[0].Name = pod.Name
	}

	if err := prepareBuildkit(c, pod.Namespace, buildkitContainerdConfig); err != nil {
		return err
	}
	buildFile := &buildFile{}
	if _, err := os.Stat("./.dev-build"); err == nil {
		data, err := ioutil.ReadFile("./.dev-build")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, buildFile); err != nil {
			return err
		}
	}
	setDefaults(buildFile, pod.Namespace, pod.Name)

	logrus.Info("Running build")
	image, err := runBuild(buildFile, c)
	if err != nil {
		return err
	}

	pod.Spec.Containers[0].Image = image

	if err := c.Create(pod); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(c.Ctx)
	wait.JitterUntil(func() {
		logrus.Info("Waiting for pod to be running")
		if p, err := c.Core.Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{}); err == nil {
			if p.Status.Phase == v1.PodRunning {
				cancel()
				return
			}
		}
	}, time.Second, 1.5, false, ctx.Done())

	if len(r.P_Ports) > 0 {
		publishPorts, err := stringers.ParsePorts(r.P_Ports...)
		if err != nil {
			return err
		}
		for _, publishPort := range publishPorts {
			p := publishPort
			logrus.Infof("Forwarding port %s to %s", p.Port, p.TargetPort)
			if err := portForward(pod, c, strconv.Itoa(int(p.Port))); err != nil {
				logrus.Error(err)
			}
		}
	}
	return nil
}

func (r *Run) preparePod(c *clicontext.CLIContext) (*v1.Pod, error) {
	pod := &v1.Pod{}
	pod.Namespace, pod.Name = stack.NamespaceAndName(c, r.N_Name)
	pod.Spec.Containers = []v1.Container{
		{},
	}
	var err error

	pod.Spec.HostAliases, err = stringers.ParseHostAliases(r.AddHost...)
	if err != nil {
		return nil, err
	}

	pod.Annotations = r.Annotations
	pod.Labels = r.L_Label

	pod.Labels, err = parseLabels(r.LabelFile, pod.Labels)
	if err != nil {
		return nil, err
	}

	pod.Spec.SecurityContext = &v1.PodSecurityContext{}
	pod.Spec.SecurityContext.RunAsUser, pod.Spec.SecurityContext.RunAsGroup, err = stringers.ParseUserGroup(r.U_User, r.Group)
	if err != nil {
		return nil, err
	}

	pod.Spec.Containers[0].Command = r.Command
	if r.Cpus != "" {
		cpus, err := stringers.ParseQuantity(r.Cpus)
		if err != nil {
			return nil, err
		}
		if pod.Spec.Containers[0].Resources.Requests == nil {
			pod.Spec.Containers[0].Resources.Requests = map[v1.ResourceName]resource.Quantity{}
		}
		pod.Spec.Containers[0].Resources.Requests[v1.ResourceCPU] = cpus
	}

	if r.M_Memory != "" {
		memory, err := stringers.ParseQuantity(r.M_Memory)
		if err != nil {
			return nil, err
		}
		if pod.Spec.Containers[0].Resources.Requests == nil {
			pod.Spec.Containers[0].Resources.Requests = map[v1.ResourceName]resource.Quantity{}
		}
		pod.Spec.Containers[0].Resources.Requests[v1.ResourceMemory] = memory
	}

	if err := parseEnvs(pod, r); err != nil {
		return nil, err
	}

	if err := populateHealthCheck(pod, r); err != nil {
		return nil, err
	}

	if err := parsePorts(pod, r); err != nil {
		return nil, err
	}

	pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{}
	pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = &r.ReadOnly
	pod.Spec.Containers[0].ImagePullPolicy, err = stringers.ParseImagePullPolicy(r.ImagePullPolicy)
	if err != nil {
		return nil, err
	}
	for _, s := range r.ImagePullSecrets {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets,
			v1.LocalObjectReference{
				Name: s,
			})
	}

	parseDNSOptions(pod, r)
	parseConfigs(pod, r)
	parseSecrets(pod, r)
	parseVolume(pod, r)

	return pod, err
}

func runBuild(buildSpec *buildFile, c *clicontext.CLIContext) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	attachable := []session.Attachable{authprovider.NewDockerAuthProvider()}

	buildkitClient, err := client.New(ctx, "tcp://localhost:9000")
	if err != nil {
		return "", err
	}

	if buildSpec.PushRegistry != "" {
		buildSpec.PushRegistry += "/"
	}
	image := fmt.Sprintf("%s%s:%s", buildSpec.PushRegistry, buildSpec.BuildImageName, buildSpec.Tag)
	exports, err := build.ParseOutput([]string{"type=image,name=" + image})
	if err != nil {
		return "", err
	}
	solveOpt := client.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename": "Dockerfile",
		},
		LocalDirs: map[string]string{
			"context":    buildSpec.BuildContext,
			"dockerfile": buildSpec.DockerFile,
		},
		Session: attachable,
		Exports: exports,
	}
	ch := make(chan *client.SolveStatus)
	eg, ctx := errgroup.WithContext(c.Ctx)
	displayCh := ch

	eg.Go(func() error {
		resp, err := buildkitClient.Solve(ctx, nil, solveOpt, ch)
		if err != nil {
			return err
		}
		for k, v := range resp.ExporterResponse {
			logrus.Debugf("exporter response: %s=%s", k, v)
		}
		return err
	})

	eg.Go(func() error {
		var c console.Console
		return progressui.DisplaySolveStatus(context.TODO(), "", c, os.Stderr, displayCh)
	})

	return image, eg.Wait()
}

func prepareBuildkit(c *clicontext.CLIContext, namespace string, config string) error {
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
	buildkitdDeploy := buildkitDeployment(namespace)
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
			if IsReady(&deploy.Status) {
				cancel()
				return
			}
			logrus.Info("Waiting for buildkitd deploy to be ready")
		}
	}, time.Second, 1.5, false, ctx.Done())
	buildkitdPod, err := findBuildkitPod(c, namespace)
	if err != nil {
		return err
	}
	go func() {
		if err := portForward(buildkitdPod, c, "9000"); err != nil {
			logrus.Error(err)
		}
	}()
	time.Sleep(time.Second)
	return nil
}

func portForward(pod *v1.Pod, c *clicontext.CLIContext, port string) error {
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
		Ports:        []string{fmt.Sprintf("%s:%s", "9000", "8080")},
		StopChannel:  make(chan struct{}, 1),
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
	fmt.Println(url.String())
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

func setConfigDefaults(config *rest.Config) error {
	gv := v1.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/api"
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return nil
}

func findBuildkitPod(c *clicontext.CLIContext, namespace string) (*v1.Pod, error) {
	pods, err := c.K8s.CoreV1().Pods(namespace).List(metav1.ListOptions{
		LabelSelector: "app=buildkitd-dev",
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod found")
	}
	return &pods.Items[0], nil
}

func buildkitDeployment(namespace string) *appv1.Deployment {
	deploy := &appv1.Deployment{}
	deploy.Name = "buildkit"
	deploy.Namespace = namespace
	deploy.Annotations = map[string]string{
		"container.apparmor.security.beta.kubernetes.io/buildkitd": "unconfined",
		"container.seccomp.security.alpha.kubernetes.io/buildkitd": "unconfined",
	}
	deploy.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": "buildkitd-dev",
		},
	}
	hostPathType := v1.HostPathFile
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
					Image: "moby/buildkit:v0.5.1-rootless@sha256:5a826464a96e11d1c1ee97f35460f8421c6bdafd1d8f20bc11b9d698a179ab0b",
					Ports: []v1.ContainerPort{
						{
							ContainerPort: 8080,
						},
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name: "config",
							MountPath: "/home/user/.config/buildkit/buildkitd.toml",
							SubPath: "buildkitd.toml",
						},
						{
							Name: "containerd-sock",
							MountPath: "/run/k3s/containerd/containerd.sock",
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
				{
					Name: "containerd-sock",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Type: &hostPathType,
							Path: "/run/k3s/containerd/containerd.sock",
						},
					},
				},
			},
		},
	}
	return deploy
}

func IsReady(status *appv1.DeploymentStatus) bool {
	if status == nil {
		return false
	}
	for _, con := range status.Conditions {
		if con.Type == appv1.DeploymentAvailable && con.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func parseEnvs(pod *v1.Pod, r *Run) error {
	envs, err := stringers.ParseEnv(r.EnvFile, r.E_Env, true)
	if err != nil {
		return err
	}
	for _, e := range envs {
		envVar := v1.EnvVar{
			Name: e.Name,
		}
		if e.Value != "" {
			envVar.Value = e.Value
		}
		if e.ConfigMapName != "" {
			envVar.ValueFrom = &v1.EnvVarSource{
				ConfigMapKeyRef: &v1.ConfigMapKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: e.ConfigMapName,
					},
				},
			}
		}
		if e.SecretName != "" {
			envVar.ValueFrom = &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: e.SecretName,
					},
				},
			}
		}
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, envVar)
	}
	return nil
}

func populateHealthCheck(pod *v1.Pod, r *Run) error {
	if r.HealthURL == "" && r.HealthCmd == "" {
		return nil
	}

	hc := v1.Probe{
		FailureThreshold: int32(r.HealthFailureThreshold),
		SuccessThreshold: int32(r.HealthSuccessThreshold),
	}

	if r.HealthInitialDelay != "" {
		delay, err := time.ParseDuration(r.HealthInitialDelay)
		if err != nil {
			return err
		}

		hc.InitialDelaySeconds = int32(delay.Seconds())
	}

	if r.HealthInterval != "" {
		interval, err := time.ParseDuration(r.HealthInterval)
		if err != nil {
			return err
		}

		hc.PeriodSeconds = int32(interval.Seconds())
	}

	if r.HealthTimeout != "" {
		timeout, err := time.ParseDuration(r.HealthTimeout)
		if err != nil {
			return err
		}

		hc.TimeoutSeconds = int32(timeout.Seconds())
	}

	if len(r.HealthCmd) > 0 {
		words, err := shellwords.Parse(r.HealthCmd)
		if err != nil {
			return err
		}
		hc.Handler.Exec = &v1.ExecAction{
			Command: words,
		}
	}

	if len(r.HealthURL) > 0 {
		u, err := url.Parse(r.HealthURL)
		if err != nil {
			return err
		}

		portStr := u.Port()
		if portStr == "" {
			return fmt.Errorf("missing port in health-url %s", r.HealthURL)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return err
		}

		if u.Scheme == "tcp" {
			hc.TCPSocket = &v1.TCPSocketAction{
				Port: intstr.FromInt(port),
			}
		} else {
			hc.HTTPGet = &v1.HTTPGetAction{
				Port: intstr.FromInt(port),
				Host: u.Host,
				Path: u.Path,
			}

			for key, value := range r.HealthHeader {
				hc.HTTPGet.HTTPHeaders = append(hc.HTTPGet.HTTPHeaders, v1.HTTPHeader{
					Name:  key,
					Value: value,
				})
			}

			switch u.Scheme {
			case "http":
				hc.HTTPGet.Scheme = v1.URISchemeHTTP
			case "https":
				hc.HTTPGet.Scheme = v1.URISchemeHTTPS
			default:
				return fmt.Errorf("invalid scheme in health-url %s: %s", r.HealthURL, u.Scheme)
			}

		}
	}

	pod.Spec.Containers[0].LivenessProbe = &hc
	pod.Spec.Containers[0].ReadinessProbe = &hc

	return nil
}

func parseVolume(pod *v1.Pod, r *Run) {
	for _, pvc := range r.Volume {
		volumeMount := v1.VolumeMount{}
		volume := v1.Volume{}
		v, mount := kv.Split(pvc, ":")
		name, subPath := kv.Split(v, "/")
		volume.Name = name
		volume.PersistentVolumeClaim = &v1.PersistentVolumeClaimVolumeSource{
			ClaimName: name,
		}
		volumeMount.Name = name
		volumeMount.MountPath = mount
		volumeMount.SubPath = subPath
		pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, volumeMount)
	}
}

func parseSecrets(pod *v1.Pod, r *Run) {
	for _, secret := range r.Secret {
		volumeMount := v1.VolumeMount{}
		volume := v1.Volume{}
		v, mount := kv.Split(secret, ":")
		name, subPath := kv.Split(v, "/")
		volume.Name = name
		volume.Secret = &v1.SecretVolumeSource{
			SecretName: name,
		}
		volumeMount.Name = name
		volumeMount.MountPath = mount
		volumeMount.SubPath = subPath
		pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, volumeMount)
	}
}

func parseConfigs(pod *v1.Pod, r *Run) {
	for _, config := range r.Config {
		volumeMount := v1.VolumeMount{}
		volume := v1.Volume{}
		v, mount := kv.Split(config, ":")
		name, subPath := kv.Split(v, "/")
		volume.Name = name
		volume.ConfigMap = &v1.ConfigMapVolumeSource{
			LocalObjectReference: v1.LocalObjectReference{
				Name: name,
			},
		}
		volumeMount.Name = name
		volumeMount.MountPath = mount
		volumeMount.SubPath = subPath
		pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, volumeMount)
	}
}

func parseLabels(files []string, override map[string]string) (map[string]string, error) {
	labels, err := kvfile.ReadKVStrings(files, nil)
	if err != nil {
		return nil, err
	}

	result := map[string]string{}

	for _, label := range labels {
		key, value := kv.Split(label, "=")
		result[key] = value
	}

	for k, v := range override {
		result[k] = v
	}

	return result, nil
}

func parsePorts(pod *v1.Pod, r *Run) error {
	ports, err := stringers.ParsePorts(r.P_Ports...)
	if err != nil {
		return err
	}
	for _, port := range ports {
		pod.Spec.Containers[0].Ports = append(pod.Spec.Containers[0].Ports, v1.ContainerPort{
			Name:          port.Name,
			Protocol:      v1.ProtocolTCP,
			ContainerPort: port.TargetPort,
		})
	}
	return nil
}

func parseDNSOptions(pod *v1.Pod, r *Run) {
	dnsOptions := stringers.ParseDNSOptions(r.DNSOption...)
	pod.Spec.DNSConfig = &v1.PodDNSConfig{
		Nameservers: r.DNS,
		Searches:    r.DNSSearch,
	}
	for _, option := range dnsOptions {
		pod.Spec.DNSConfig.Options = append(pod.Spec.DNSConfig.Options, v1.PodDNSConfigOption{
			Name:  option.Name,
			Value: option.Value,
		})
	}
}

func setDefaults(buildFile *buildFile, namespace, name string) {
	if buildFile.DockerFile == "" {
		buildFile.DockerFile = "."
	}
	if buildFile.BuildContext == "" {
		buildFile.BuildContext = "."
	}
	if buildFile.BuildImageName == "" {
		buildFile.BuildImageName = namespace + "/" + name
	}
	if buildFile.Tag == "" {
		buildFile.Tag = "dev"
	}
}
