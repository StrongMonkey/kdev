package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-shellwords"
	"github.com/rancher/rio/cli/pkg/clicontext"
	"github.com/rancher/rio/cli/pkg/kvfile"
	"github.com/rancher/rio/pkg/pretty/stringers"
	"github.com/rancher/wrangler/pkg/kv"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	appv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

const (
	minikubeDockerSocat = "/run/docker.sock"
)

type RunOptions struct {
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
	P_Ports                []string          `desc:"Publish a container's port(s) externally (default: \"80:8080/http\")"`
	ReadOnly               bool              `desc:"Mount the container's root filesystem as read only"`
	Secret                 []string          `desc:"Secrets to inject to the service (format: name:target)"`
	T_Tty                  bool              `desc:"Allocate a pseudo-TTY"`
	Volume                 []string          `desc:"Specify volumes"`
	U_User                 string            `desc:"UID[:GID] Sets the UID used and optionally GID for entrypoint process (format: <uid>[:<gid>])"`
	W_Workdir              string            `desc:"Working directory inside the container"`
}

type Run struct {
	BuildOptions
	RunOptions
	Pod         bool   `desc:"Running a pod instead of deployment"`
	Name        string `desc:"Assign a name to the pod or deployment"`
	N_Namespace string `desc:"Set namespace" default:"default"`
}

func (r *Run) Run(c *clicontext.CLIContext) error {
	if len(c.CLI.Args()) == 0 {
		return fmt.Errorf("at least one argument is required")
	}

	deploy, err := r.prepareDeploy(c)
	if err != nil {
		return err
	}
	deploy.Namespace = r.N_Namespace
	deploy.Name = r.Name
	podSpec := deploy.Spec.Template.Spec
	podSpec.Containers[0].Name = r.Name

	if deploy.Name == "" {
		workingDir, err := os.Getwd()
		if err != nil {
			return err
		}
		dir := filepath.Base(workingDir)
		deploy.Name = strings.Replace(dir, "_", "-", -1)
		podSpec.Containers[0].Name = deploy.Name
	}

	buildkitPortForwardStop := make(chan struct{})
	if err := prepareBuildkit(c, r.N_Namespace, buildkitPortForwardStop); err != nil {
		return err
	}

	buildSpec := buildFile{}
	buildSpec.BuildContext = c.CLI.Args()[0]
	buildSpec.Tag = fmt.Sprintf("%s/%s", deploy.Namespace, deploy.Name)
	mergeBuildOptions(r.BuildOptions, &buildSpec)

	buildConfig := map[string]buildFile{}
	buildConfig = map[string]buildFile{
		deploy.Name: buildSpec,
	}

	logrus.Info("Running build")
	imageMeta, err := runBuild(buildConfig, r.N_Namespace, c)
	if err != nil {
		return err
	}
	buildkitPortForwardStop <- struct{}{}

	podSpec.Containers[0].Image = imageMeta[deploy.Name]

	pod := &v1.Pod{}
	if !r.Pod {
		selector := map[string]string{
			"app":  deploy.Name,
			"kdev": "true",
		}
		deploy.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: selector,
		}
		deploy.Spec.Template.Labels = selector
		deploy.Spec.Template.Spec = podSpec
		if _, err := c.K8s.AppsV1().Deployments(deploy.Namespace).Create(deploy); err != nil && !errors.IsAlreadyExists(err) {
			return err
		} else if errors.IsAlreadyExists(err) {
			if _, err := c.K8s.AppsV1().Deployments(deploy.Namespace).Update(deploy); err != nil {
				return err
			}
		}
		ctx, cancel := context.WithCancel(c.Ctx)
		wait.JitterUntil(func() {
			logrus.Info("waiting for new pod to be coming")
			pod, err = findPod(c, deploy.Namespace, fmt.Sprintf("app=%s", deploy.Name))
			if err == nil && pod.Spec.Containers[0].Image == imageMeta[deploy.Name] {
				cancel()
				return
			}
		}, time.Second, 1.5, false, ctx.Done())

	} else {
		pod.Namespace = r.N_Namespace
		pod.Name = deploy.Name
		pod.Annotations = deploy.Annotations
		pod.Labels = labels.Merge(deploy.Labels, map[string]string{
			"kdev": "true",
		})
		pod.Spec = podSpec
		if err := c.Create(pod); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(c.Ctx)
	wait.JitterUntil(func() {
		logrus.Info("Waiting for pod to be running")
		if p, err := c.Core.Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{}); err == nil {
			if p.Status.Phase == v1.PodRunning {
				for _, c := range p.Status.ContainerStatuses {
					if (strings.Contains(imageMeta[deploy.Name], c.ImageID) || strings.Contains(imageMeta[deploy.Name], c.Image)) && c.Ready == true && c.State.Running != nil {
						cancel()
						return
					}
				}
			}
		}
	}, time.Second, 1.5, false, ctx.Done())

	if len(r.P_Ports) > 0 {
		publishPorts, err := stringers.ParsePorts(r.P_Ports...)
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
	}
	return nil
}

func (r *Run) prepareDeploy(c *clicontext.CLIContext) (*appv1.Deployment, error) {
	var err error
	deploy := &appv1.Deployment{}

	deploy.Annotations = r.Annotations
	deploy.Labels = r.L_Label

	deploy.Labels, err = parseLabels(r.LabelFile, deploy.Labels)
	if err != nil {
		return nil, err
	}

	podSpec := v1.PodSpec{}
	podSpec.Containers = []v1.Container{
		{},
	}

	podSpec.HostAliases, err = stringers.ParseHostAliases(r.AddHost...)
	if err != nil {
		return nil, err
	}

	podSpec.SecurityContext = &v1.PodSecurityContext{}
	podSpec.SecurityContext.RunAsUser, podSpec.SecurityContext.RunAsGroup, err = stringers.ParseUserGroup(r.U_User, r.Group)
	if err != nil {
		return nil, err
	}

	podSpec.Containers[0].Command = r.Command
	if r.Cpus != "" {
		cpus, err := stringers.ParseQuantity(r.Cpus)
		if err != nil {
			return nil, err
		}
		if podSpec.Containers[0].Resources.Requests == nil {
			podSpec.Containers[0].Resources.Requests = map[v1.ResourceName]resource.Quantity{}
		}
		podSpec.Containers[0].Resources.Requests[v1.ResourceCPU] = cpus
	}

	if r.M_Memory != "" {
		memory, err := stringers.ParseQuantity(r.M_Memory)
		if err != nil {
			return nil, err
		}
		if podSpec.Containers[0].Resources.Requests == nil {
			podSpec.Containers[0].Resources.Requests = map[v1.ResourceName]resource.Quantity{}
		}
		podSpec.Containers[0].Resources.Requests[v1.ResourceMemory] = memory
	}

	if err := parseEnvs(podSpec, r); err != nil {
		return nil, err
	}

	if err := populateHealthCheck(podSpec, r); err != nil {
		return nil, err
	}

	if err := parsePorts(podSpec, r); err != nil {
		return nil, err
	}

	podSpec.Containers[0].SecurityContext = &v1.SecurityContext{}
	podSpec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = &r.ReadOnly
	podSpec.Containers[0].ImagePullPolicy, err = stringers.ParseImagePullPolicy(r.ImagePullPolicy)
	if err != nil {
		return nil, err
	}
	for _, s := range r.ImagePullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets,
			v1.LocalObjectReference{
				Name: s,
			})
	}

	parseDNSOptions(&podSpec, r)
	parseConfigs(&podSpec, r)
	parseSecrets(&podSpec, r)
	parseVolume(&podSpec, r)

	deploy.Spec.Template.Spec = podSpec

	return deploy, err
}

func runBuild(buildSpec map[string]buildFile, namespace string, c *clicontext.CLIContext) (map[string]string, error) {
	m := sync.Map{}
	setDefaults(buildSpec, namespace)
	errg, _ := errgroup.WithContext(c.Ctx)
	for name, config := range buildSpec {
		buildConfig := config
		errg.Go(func() error {
			image, err := buildInternal(&buildConfig, c)
			if err != nil {
				return err
			}
			m.LoadOrStore(name, image)
			return nil
		})
	}
	if err := errg.Wait(); err != nil {
		return nil, err
	}
	result := map[string]string{}
	m.Range(func(key, value interface{}) bool {
		result[key.(string)] = value.(string)
		return true
	})
	return result, nil
}

func prepareDockerSocat(c *clicontext.CLIContext, socketAddress string, stopChan chan struct{}) error {
	T := true
	hostPathFileType := v1.HostPathFile
	socatPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "socat-docker-socket",
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			HostNetwork: true,
			Containers: []v1.Container{
				{
					Name:  "socat",
					Image: "alpine/socat:1.0.3",
					Command: []string{
						"socat",
						"TCP-LISTEN:2375,fork",
						fmt.Sprintf("UNIX-CONNECT:%s", socketAddress),
					},
					SecurityContext: &v1.SecurityContext{
						Privileged: &T,
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "containerd-sock",
							MountPath: minikubeDockerSocat,
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "containerd-sock",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Type: &hostPathFileType,
							Path: minikubeDockerSocat,
						},
					},
				},
			},
		},
	}
	if _, err := c.Core.Pods("default").Create(socatPod); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	wait.JitterUntil(func() {
		logrus.Info("Waiting for pod to be running")
		if p, err := c.Core.Pods(socatPod.Namespace).Get(socatPod.Name, metav1.GetOptions{}); err == nil {
			if p.Status.Phase == v1.PodRunning {
				cancel()
				return
			}
		}
	}, time.Second, 1.5, false, ctx.Done())

	go func() {
		if err := portForward(socatPod, c, "9001", "2375", stopChan); err != nil {
			logrus.Error(err)
		}
	}()
	time.Sleep(time.Second)
	return nil
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

func findPod(c *clicontext.CLIContext, namespace string, selector string) (*v1.Pod, error) {
	pods, err := c.K8s.CoreV1().Pods(namespace).List(metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod found")
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == v1.PodRunning {
			return &pod, nil
		}
	}
	return &pods.Items[0], nil
}

func isReady(status *appv1.DeploymentStatus) bool {
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

func parseEnvs(podSpec v1.PodSpec, r *Run) error {
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
		podSpec.Containers[0].Env = append(podSpec.Containers[0].Env, envVar)
	}
	return nil
}

func populateHealthCheck(podSpec v1.PodSpec, r *Run) error {
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

	podSpec.Containers[0].LivenessProbe = &hc
	podSpec.Containers[0].ReadinessProbe = &hc

	return nil
}

func parseVolume(podSpec *v1.PodSpec, r *Run) {
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
		podSpec.Volumes = append(podSpec.Volumes, volume)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, volumeMount)
	}
}

func parseSecrets(podSpec *v1.PodSpec, r *Run) {
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
		podSpec.Volumes = append(podSpec.Volumes, volume)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, volumeMount)
	}
}

func parseConfigs(podSpec *v1.PodSpec, r *Run) {
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
		podSpec.Volumes = append(podSpec.Volumes, volume)
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, volumeMount)
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

func parsePorts(podSpec v1.PodSpec, r *Run) error {
	ports, err := stringers.ParsePorts(r.P_Ports...)
	if err != nil {
		return err
	}
	for _, port := range ports {
		podSpec.Containers[0].Ports = append(podSpec.Containers[0].Ports, v1.ContainerPort{
			Name:          port.Name,
			Protocol:      v1.ProtocolTCP,
			ContainerPort: port.TargetPort,
		})
	}
	return nil
}

func parseDNSOptions(podSpec *v1.PodSpec, r *Run) {
	dnsOptions := stringers.ParseDNSOptions(r.DNSOption...)
	podSpec.DNSConfig = &v1.PodDNSConfig{
		Nameservers: r.DNS,
		Searches:    r.DNSSearch,
	}
	for _, option := range dnsOptions {
		podSpec.DNSConfig.Options = append(podSpec.DNSConfig.Options, v1.PodDNSConfigOption{
			Name:  option.Name,
			Value: option.Value,
		})
	}
}

func setDefaults(buildConfig map[string]buildFile, namespace string) {
	for name, config := range buildConfig {
		if config.Dockerfile == "" {
			config.Dockerfile = "Dockerfile"
		}
		if config.BuildContext == "" {
			config.BuildContext = "."
		}
		if config.DockerfilePath == "" {
			config.DockerfilePath = config.BuildContext
		}
		if config.Tag == "" {
			config.Tag = fmt.Sprintf("%s/%s", namespace, name)
		}
		if config.PushRegistry == "" {
			config.PushRegistry = "docker.io"
		}
		buildConfig[name] = config
	}
}

func mergeBuildOptions(options BuildOptions, buildConfig *buildFile) {
	if options.Tag != "" {
		buildConfig.Tag = options.Tag
	}
	if options.Tag != "" {
		buildConfig.Tag = options.Tag
	}
	if options.PushRegistry != "" {
		buildConfig.PushRegistry = options.PushRegistry
	}
	if options.Dockerfile != "" {
		buildConfig.Dockerfile = options.Dockerfile
	}
	if options.DockerfilePath != "" {
		buildConfig.DockerfilePath = options.DockerfilePath
	}
	if options.Push {
		buildConfig.Push = options.Push
	}
}

func containerRuntime(c *clicontext.CLIContext) string {
	nodes, err := c.Core.Nodes().List(metav1.ListOptions{})
	if err == nil && len(nodes.Items) == 1 && nodes.Items[0].Name == "minikube"{
		return "minikube"
	}
	return ""
}
