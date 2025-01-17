// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package impl

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ServiceWeaver/weaver-kube/internal/proto"
	"github.com/ServiceWeaver/weaver/runtime/bin"
	"github.com/ServiceWeaver/weaver/runtime/graph"
	"github.com/ServiceWeaver/weaver/runtime/protos"
	"golang.org/x/exp/maps"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	_ "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const (
	// Name of the container that hosts the application binary.
	appContainerName = "serviceweaver"

	// kubeConfigEnvKey is the name of the env variable that contains deployment
	// information for a babysitter deployed using kube.
	kubeConfigEnvKey = "SERVICEWEAVER_DEPLOYMENT_CONFIG"

	// The exported port by the Service Weaver services.
	servicePort = 80

	// Port used by the weavelets to listen for internal traffic.
	//
	// TODO(mwhittaker): Remove internal port from kube.proto.
	internalPort = 10000
)

var (
	// Start value for ports used by the public and private listeners.
	externalPort int32 = 20000

	// Resource allocation units for "cpu" and "memory" resources.
	//
	// TODO(rgrandl): Should we allow the user to customize how many
	// resources each pod starts with?
	cpuUnit    = resource.MustParse("100m")
	memoryUnit = resource.MustParse("128Mi")
)

// replicaSet contains information about a replica set.
type replicaSet struct {
	name            string                        // name of the replica set
	components      []*ReplicaSetConfig_Component // components hosted by replica set
	image           string                        // name of the image to be deployed
	namespace       string                        // namespace of the replica set
	dep             *protos.Deployment            // deployment info
	internalPort    int                           // internal weavelet port
	traceServiceURL string                        // trace exporter URL
}

// ListenerOptions stores configuration options for a listener.
type ListenerOptions struct {
	// Is the listener public, i.e., should it receive ingress traffic
	// from the public internet. If false, the listener is configured only
	// for cluster-internal access.
	Public bool

	// If specified, the port inside the container on which the listener
	// is reachable. If zero or not specified, the first available port
	// is used.
	Port int32
}

// KubeConfig stores the configuration information for one execution of a
// Service Weaver application deployed using the Kube deployer.
type KubeConfig struct {
	// LocalTag is the build tag for the application container on the local
	// machine.
	//
	// If empty, the tag defaults to "<app_name>:<app_version>", where
	// <app_version> is the unique version id of the application deployment.
	LocalTag string `toml:"local_tag"`

	// Repo is the name of the repository the container should be uploaded to.
	// For example, if set to "docker.io/alanturing/repo", "weaver kube deploy"
	// will build the container locally, tag it with the appropriate Tag, and
	// then push it to "docker.io/alanturing/repo".
	//
	// If empty, the container is built and tagged locally, but is not pushed
	// to a repository.
	//
	// Example repositories are:
	//   - Docker Hub               :  docker.io/USERNAME/REPO_NAME
	//   - Google Artifact Registry :  LOCATION-docker.pkg.dev/PROJECT-ID/REPO_NAME
	//   - GitHub Container Registry: ghcr.io/NAMESPACE
	//
	// Note that the final image tag for the application container will
	// be a concatenation of Repo and Tag, i.e., "Repo/Tag".
	Repo string

	// Namespace is the name of the Kubernetes namespace where the application
	// should be deployed. If not specified, the application will be deployed in
	// the default namespace.
	Namespace string

	// If true, application listeners will use the underlying nodes' network.
	// This behavior is generally discouraged, but it may be useful when running
	// the application in a minikube environment, where using the underlying
	// nodes' network may make it easier to access the listeners directly from
	// the host machine.
	UseHostNetwork bool `toml:"use_host_network"`

	// Options for the application listeners, keyed by listener name.
	// If a listener isn't specified in the map, default options will be used.
	Listeners map[string]*ListenerOptions

	// Observability controls how the deployer will export observability information
	// such as logs, metrics and traces, keyed by service. If no options are
	// specified, the deployer will launch corresponding services for exporting logs,
	// metrics and traces automatically.
	//
	// The key must be one of the following strings:
	// "prometheus_service" - to export metrics to Prometheus [1]
	// "jaeger_service"     - to export traces to Jaeger [2]
	// "loki_service"       - to export logs to Grafana Loki [3]
	// "grafana_service"    - to visualize/manipulate observability information [4]
	//
	// Possible values for each service:
	// 1) do not specify a value at all; leave it empty
	// this is the default behavior; kube deployer will automatically create the
	// observability service for you.
	//
	// 2) "none"
	// kube deployer will not export the corresponding observability information to
	// any service. E.g., prometheus_service = "none" means that the user will not
	// be able to see any metrics at all. This can be useful for testing or
	// benchmarking the performance of your application.
	//
	// 3) "your_observability_service_name"
	// if you already have a running service to collect metrics, traces or logs,
	// then you can simply specify the service name, and your application will
	// automatically export the corresponding information to your service. E.g.,
	// jaeger_service = "jaeger-all-in-one" will enable your running Jaeger
	// "service/jaeger-all-in-one" to capture all the app traces.
	//
	// [1] - https://prometheus.io/
	// [2] - https://www.jaegertracing.io/
	// [3] - https://grafana.com/oss/loki/
	// [4] - https://grafana.com/
	Observability map[string]string
}

// globalName returns an unique name that persists across app versions.
func (r *replicaSet) globalName() string {
	return name{r.dep.App.Name, r.name}.DNSLabel()
}

// deploymentName returns a name that is version specific.
func (r *replicaSet) deploymentName() string {
	return name{r.dep.App.Name, r.name, r.dep.Id[:8]}.DNSLabel()
}

// buildDeployment generates a kubernetes deployment for a replica set.
//
// TODO(rgrandl): test to see if it works with an app where a component foo is
// collocated with main, and a component bar that is not collocated with main
// calls foo.
func (r *replicaSet) buildDeployment(cfg *KubeConfig) (*appsv1.Deployment, error) {
	matchLabels := map[string]string{}
	podLabels := map[string]string{
		"appName": r.dep.App.Name,
		"depName": r.deploymentName(),
		"metrics": r.dep.App.Name, // Needed by Prometheus to scrape the metrics.
	}
	name := r.deploymentName()
	if r.hasListeners() {
		name = r.globalName()

		// Set the match and the pod labels, so they can be reachable across
		// multiple app versions.
		matchLabels["globalName"] = r.globalName()
		podLabels["globalName"] = r.globalName()
	} else {
		matchLabels["depName"] = r.deploymentName()
	}
	dnsPolicy := corev1.DNSClusterFirst
	if cfg.UseHostNetwork {
		dnsPolicy = corev1.DNSClusterFirstWithHostNet
	}

	container, err := r.buildContainer()
	if err != nil {
		return nil, err
	}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:    podLabels,
					Namespace: r.namespace,
				},
				Spec: corev1.PodSpec{
					Containers:  []corev1.Container{container},
					DNSPolicy:   dnsPolicy,
					HostNetwork: cfg.UseHostNetwork,
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDeployment{},
			},
			// Number of old ReplicaSets to retain to allow rollback.
			RevisionHistoryLimit: ptrOf(int32(1)),
			MinReadySeconds:      int32(5),
		},
	}, nil
}

// buildListenerService generates a kubernetes service for a listener.
//
// Note that for public listeners, we generate a Load Balancer service because
// it has to be reachable from the outside; for internal listeners, we generate
// a ClusterIP service, reachable only from internal Service Weaver services.
func (r *replicaSet) buildListenerService(lis *ReplicaSetConfig_Listener) (*corev1.Service, error) {
	// Unique name that persists across app versions.
	// TODO(rgrandl): Specify whether the listener is public in the name.
	globalLisName := name{r.dep.App.Name, "lis", lis.Name}.DNSLabel()

	var serviceType string
	if lis.IsPublic {
		serviceType = "LoadBalancer"
	} else {
		serviceType = "ClusterIP"
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      globalLisName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"lisName": lis.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceType(serviceType),
			Selector: map[string]string{
				"globalName": r.globalName(),
			},
			Ports: []corev1.ServicePort{
				{
					Port:       servicePort,
					Protocol:   "TCP",
					TargetPort: intstr.IntOrString{IntVal: lis.ExternalPort},
				},
			},
		},
	}, nil
}

// buildAutoscaler generates a kubernetes horizontal pod autoscaler for a replica set.
func (r *replicaSet) buildAutoscaler() (*autoscalingv2.HorizontalPodAutoscaler, error) {
	// Per deployment name that is app version specific.
	aname := name{r.dep.App.Name, "hpa", r.name, r.dep.Id[:8]}.DNSLabel()

	var depName string
	if r.hasListeners() {
		depName = r.globalName()
	} else {
		depName = r.deploymentName()
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "autoscaling/v2",
			Kind:       "HorizontalPodAutoscaler",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      aname,
			Namespace: r.namespace,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       depName,
			},
			MinReplicas: ptrOf(int32(1)),
			MaxReplicas: 10,
			Metrics: []autoscalingv2.MetricSpec{
				{
					// The pods are scaled up/down when the average CPU
					// utilization is above/below 80%.
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: ptrOf(int32(80)),
						},
					},
				},
			},
		},
	}, nil
}

// buildContainer builds a container specification for a replica set.
func (r *replicaSet) buildContainer() (corev1.Container, error) {
	// Set the binary path in the deployment w.r.t. to the binary path in the
	// docker image.
	r.dep.App.Binary = fmt.Sprintf("/weaver/%s", filepath.Base(r.dep.App.Binary))
	kubeCfgStr, err := proto.ToEnv(&ReplicaSetConfig{
		Namespace:       r.namespace,
		Name:            r.name,
		Deployment:      r.dep,
		InternalPort:    int32(r.internalPort),
		TraceServiceUrl: r.traceServiceURL,
		Components:      r.components,
	})
	if err != nil {
		return corev1.Container{}, err
	}

	// Always expose the metrics port from the container, so it can be
	// discoverable for scraping by Prometheus.
	ports := []corev1.ContainerPort{
		{Name: "prometheus", ContainerPort: defaultMetricsPort},
	}
	// Expose all of the listener ports.
	for _, ls := range r.components {
		for _, l := range ls.Listeners {
			ports = append(ports, corev1.ContainerPort{
				Name:          l.Name,
				ContainerPort: l.ExternalPort,
			})
		}
	}

	return corev1.Container{
		Name:            appContainerName,
		Image:           r.image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"babysitter"},
		Env: []corev1.EnvVar{
			{Name: kubeConfigEnvKey, Value: kubeCfgStr},
		},
		Resources: corev1.ResourceRequirements{
			// NOTE: start with the smallest allowed limits, and count on autoscalers
			// doing the rest.
			//
			// NOTE: if we don't specify the minimum amount of compute resources
			// required, the autoscaler doesn't work properly, because the metric
			// server is not able to report the resource usage of the container.
			Requests: corev1.ResourceList{
				"memory": memoryUnit,
				"cpu":    cpuUnit,
			},
			// NOTE: we don't specify any limits, allowing all available node
			// resources to be used, if needed. Note that in practice, we
			// attach autoscalers to all of our containers, so the extra-usage
			// should be only for a short period of time.
		},
		Ports: ports,

		// Enabling TTY and Stdin allows the user to run a shell inside the container,
		// for debugging.
		TTY:   true,
		Stdin: true,
	}, nil
}

// hasListeners returns whether a given replica set exports any listeners.
func (r *replicaSet) hasListeners() bool {
	for _, listeners := range r.components {
		if listeners.Listeners != nil {
			return true
		}
	}
	return false
}

// GenerateYAMLs generates Kubernetes YAML configurations for a given
// application version.
//
// The following Kubernetes YAML configurations will be generated:
//
//   - For replica sets that don't host any listeners, a Kubernetes Deployment
//     YAML with a unique name.
//
//   - For replica sets that do host a network listener, a Kubernetes Deployment
//     YAML with a stable name, i.e., a name that persists across application
//     versions. This crossed-version shared naming will be used to gradually
//     roll out a new application version.
//
//     For example, let's assume that we have an app v1 with two replica sets:
//     `main` and `foo“, with `main` hosting a network listener. When we deploy
//     v2 of the app, it will be rolled out as follows:
//
//     [main v1] [main v1]     [main v1] [main v2]     [main v2] [main v2]
//     |            |          |         |             |         |
//     v            |       => v         v         =>  |         v
//     [foo v1] <---|          [foo v1]  [foo v2]      |-------> [foo v2]
//
//   - For network listeners, a Kubernetes Service with a stable name, i.e.,
//     a name that persists across application versions.
//
//   - If observability services are enabled (e.g., Prometheus, Jaeger), a
//     Kubernetes Deployment and/or a Service for each observability service.
func GenerateYAMLs(image string, dep *protos.Deployment, cfg *KubeConfig) error {
	fmt.Fprintf(os.Stderr, greenText(), "\nGenerating kube deployment info ...")

	// Generate roles and role bindings.
	var generated []byte
	content, err := generateRolesAndBindings(cfg.Namespace)
	if err != nil {
		return fmt.Errorf("unable to generate roles and bindings: %w", err)
	}
	generated = append(generated, content...)

	// Generate core YAMLs (deployments, services, autoscalers).
	content, err = generateCoreYAMLs(dep, cfg, image)
	if err != nil {
		return fmt.Errorf("unable to create kube app deployment: %w", err)
	}
	generated = append(generated, content...)

	// Generate deployment info needed to get insights into the application.
	content, err = generateObservabilityYAMLs(dep, cfg)
	if err != nil {
		return fmt.Errorf("unable to create configuration information: %w", err)
	}
	generated = append(generated, content...)

	// Write the generated kube info into a file.
	yamlFile := filepath.Join(os.TempDir(), fmt.Sprintf("kube_%s.yaml", dep.Id))
	f, err := os.OpenFile(yamlFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(generated); err != nil {
		return fmt.Errorf("unable to write the kube deployment info: %w", err)
	}
	fmt.Fprintf(os.Stderr, greenText(), "kube deployment information successfully generated")
	fmt.Println(yamlFile)
	return nil
}

// generateRolesAndBindings generates Kubernetes roles and role bindings in
// namespace that grant permissions to the appropriate service accounts.
func generateRolesAndBindings(namespace string) ([]byte, error) {
	// Grant the default service account the permission to get, list, and watch
	// pods. The babysitter watches pods to generate routing info.
	//
	// TODO(mwhittaker): This leaks permissions to the user's code. We should
	// avoid that. We might have to run the babysitter and weavelet in separate
	// containers or pods.

	role := rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pods-getter",
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}

	binding := rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "RoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-pods-getter",
			Namespace: namespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "pods-getter",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "default",
				Namespace: namespace,
			},
		},
	}

	var b bytes.Buffer
	fmt.Fprintln(&b, "# Roles and bindings.")

	bytes, err := yaml.Marshal(role)
	if err != nil {
		return nil, err
	}
	b.Write(bytes)
	fmt.Fprintf(&b, "\n---\n\n")

	bytes, err = yaml.Marshal(binding)
	if err != nil {
		return nil, err
	}
	b.Write(bytes)
	fmt.Fprintf(&b, "\n---\n\n")

	fmt.Fprintf(os.Stderr, "Generated roles and bindings\n")
	return b.Bytes(), nil
}

// generateCoreYAMLs generates the core YAMLs for the given deployment.
func generateCoreYAMLs(dep *protos.Deployment, cfg *KubeConfig, image string) ([]byte, error) {
	// Generate the kubernetes replica sets for the deployment, along with
	// their communication graph.
	replicaSets, rg, err := buildReplicaSets(dep, image, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create replica sets: %w", err)
	}

	// For each replica set, build a deployment and an autoscaler. If a replica
	// set has any listeners, build a service for each listener. We traverse
	// the graph in a deterministic order, to achieve a stable YAML file.
	var generated []byte
	for _, n := range graph.ReversePostOrder(rg) {
		rs := replicaSets[n]

		// Build a deployment.
		d, err := rs.buildDeployment(cfg)
		if err != nil {
			return nil, fmt.Errorf("unable to create kube deployment for replica set %s: %w", rs.name, err)
		}
		content, err := yaml.Marshal(d)
		if err != nil {
			return nil, err
		}
		generated = append(generated, []byte(fmt.Sprintf("# Deployment for replica set %s\n", rs.name))...)
		generated = append(generated, content...)
		generated = append(generated, []byte("\n---\n")...)
		fmt.Fprintf(os.Stderr, "Generated kube deployment for replica set %v\n", rs.name)

		// Build a horizontal pod autoscaler for the deployment.
		a, err := rs.buildAutoscaler()
		if err != nil {
			return nil, fmt.Errorf("unable to create kube autoscaler for replica set %s: %w", rs.name, err)
		}
		content, err = yaml.Marshal(a)
		if err != nil {
			return nil, err
		}
		generated = append(generated, []byte(fmt.Sprintf("\n# Autoscaler for replica set %s\n", rs.name))...)
		generated = append(generated, content...)
		generated = append(generated, []byte("\n---\n")...)
		fmt.Fprintf(os.Stderr, "Generated kube autoscaler for replica set %v\n", rs.name)

		// Build a service for each listener.
		for _, listeners := range rs.components {
			for _, lis := range listeners.Listeners {
				ls, err := rs.buildListenerService(lis)
				if err != nil {
					return nil, fmt.Errorf("unable to create kube listener service for %s: %w", lis.Name, err)
				}
				content, err = yaml.Marshal(ls)
				if err != nil {
					return nil, err
				}
				generated = append(generated, []byte(fmt.Sprintf("\n# Listener Service for replica set %s\n", rs.name))...)
				generated = append(generated, content...)
				generated = append(generated, []byte("\n---\n")...)
				fmt.Fprintf(os.Stderr, "Generated kube listener service for listener %v\n", lis.Name)
			}
		}
		generated = append(generated, []byte("\n")...)
	}
	return generated, nil
}

// buildReplicaSets returns the replica sets that will be used for the
// given deployment, along with the communication graph used between those
// replica sets.
func buildReplicaSets(dep *protos.Deployment, image string, cfg *KubeConfig) ([]*replicaSet, graph.Graph, error) {
	// Compute the URL of the export traces service.
	var traceServiceURL string
	jservice := cfg.Observability[tracesConfigKey]
	switch {
	case jservice == auto:
		// Point to the service launched by the kube deployer.
		traceServiceURL = fmt.Sprintf("http://%s:%d/api/traces", name{dep.App.Name, jaegerAppName}.DNSLabel(), defaultJaegerCollectorPort)
	case jservice != disabled:
		// Point to the service launched by the user.
		traceServiceURL = fmt.Sprintf("http://%s:%d/api/traces", jservice, defaultJaegerCollectorPort)
	default:
		// No trace to export.
	}

	// Retrieve the components information from the binary.
	components, cg, err := readBinary(dep, cfg)
	if err != nil {
		return nil, nil, err
	}

	// For all co-located components, choose a component to serve as the
	// primary. This will be the first component in each colocation group, as
	// specified in the config file.
	cmap := make(map[string]graph.Node, len(components)) // component name -> node
	cg.PerNode(func(n graph.Node) {
		cmap[components[n].Name] = n
	})
	primary := make([]graph.Node, len(components))
	cg.PerNode(func(n graph.Node) { // default: each component its own primary
		primary[n] = n
	})
	for _, group := range dep.App.Colocate {
		if len(group.Components) == 0 {
			continue
		}
		prim := cmap[group.Components[0]]
		for _, c := range group.Components {
			primary[cmap[c]] = prim
		}
	}

	// Build the replica set information, along with an associated graph
	// of replica sets.
	replicaSets := make([]*replicaSet, len(components))
	nodes := map[graph.Node]struct{}{}
	cg.PerNode(func(n graph.Node) {
		pn := primary[n]
		nodes[graph.Node(pn)] = struct{}{}
		if replicaSets[pn] == nil {
			replicaSets[pn] = &replicaSet{
				name:            components[pn].Name,
				image:           image,
				namespace:       cfg.Namespace,
				dep:             dep,
				internalPort:    internalPort,
				traceServiceURL: traceServiceURL,
			}
		}
		replicaSets[pn].components = append(replicaSets[pn].components, components[n])
	})
	edges := map[graph.Edge]struct{}{}
	graph.PerEdge(cg, func(e graph.Edge) {
		src := primary[e.Src]
		dst := primary[e.Dst]
		edges[graph.Edge{Src: src, Dst: dst}] = struct{}{}
	})
	return replicaSets, graph.NewAdjacencyGraph(maps.Keys(nodes), maps.Keys(edges)), nil
}

// readBinary returns the component and listener information embedded in the
// binary.
func readBinary(dep *protos.Deployment, cfg *KubeConfig) ([]*ReplicaSetConfig_Component, graph.Graph, error) {
	// Read the component graph from the binary.
	cs, g, err := bin.ReadComponentGraph(dep.App.Binary)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to retrieve the call graph for binary %s: %w", dep.App.Binary, err)
	}
	components := make([]*ReplicaSetConfig_Component, len(cs))
	g.PerNode(func(n graph.Node) {
		components[n] = &ReplicaSetConfig_Component{Name: cs[n]}
	})

	// Read the listeners information from the binary.
	ls, err := bin.ReadListeners(dep.App.Binary)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to retrieve the listeners for binary %s: %w", dep.App.Binary, err)
	}
	listeners := make(map[string][]string, len(ls))
	for _, l := range ls {
		listeners[l.Component] = l.Listeners
	}

	// Collate the two.
	for _, c := range components {
		for _, lis := range listeners[c.Name] {
			public := false
			if opts := cfg.Listeners[lis]; opts != nil && opts.Public {
				public = true
			}
			var port int32
			if opts := cfg.Listeners[lis]; opts != nil && opts.Port != 0 {
				port = opts.Port
			} else {
				// Pick an unused port.
				port = externalPort
				externalPort++
			}
			c.Listeners = append(c.Listeners, &ReplicaSetConfig_Listener{
				Name:         lis,
				ExternalPort: port,
				IsPublic:     public,
			})
		}
	}
	return components, g, nil
}
