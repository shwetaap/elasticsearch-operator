package elasticsearch

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	api "github.com/openshift/elasticsearch-operator/apis/logging/v1"
	"github.com/openshift/elasticsearch-operator/internal/constants"
	"github.com/openshift/elasticsearch-operator/internal/manifests/persistentvolume"
	"github.com/openshift/elasticsearch-operator/internal/manifests/pod"
	"github.com/openshift/elasticsearch-operator/internal/utils"

	"github.com/ViaQ/logerr/v2/kverrors"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var excludeConfigMapKeys = []string{"index_settings"}

var defaultResources = map[string]v1.ResourceRequirements{
	"proxy": {
		Limits: v1.ResourceList{
			v1.ResourceMemory: resource.MustParse(defaultESProxyMemoryLimit),
		},
		Requests: v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse(defaultESProxyCPURequest),
			v1.ResourceMemory: resource.MustParse(defaultESProxyMemoryRequest),
		},
	},
	"elasticsearch": {
		Limits: v1.ResourceList{
			v1.ResourceMemory: resource.MustParse(defaultESMemoryLimit),
		},
		Requests: v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse(defaultESCpuRequest),
			v1.ResourceMemory: resource.MustParse(defaultESMemoryRequest),
		},
	},
}

func serviceMonitorServiceAccountName(dplName string) string {
	return fmt.Sprintf("%s-metrics", dplName)
}

func serviceMonitorServiceAccountTokenName(dplName string) string {
	return fmt.Sprintf("%s-token", serviceMonitorServiceAccountName(dplName))
}

func serviceCABundleName(dplName string) string {
	return fmt.Sprintf("%s-ca-bundle", dplName)
}

func getESImage() string {
	return utils.LookupEnvWithDefault("RELATED_IMAGE_ELASTICSEARCH", constants.ElasticsearchDefaultImage)
}

func getESProxyImage() string {
	return utils.LookupEnvWithDefault("RELATED_IMAGE_ELASTICSEARCH_PROXY", constants.ProxyDefaultImage)
}

func getNodeRoleMap(node api.ElasticsearchNode) map[api.ElasticsearchNodeRole]bool {
	isClient := false
	isData := false
	isMaster := false

	for _, role := range node.Roles {
		if role == api.ElasticsearchRoleClient {
			isClient = true
		}

		if role == api.ElasticsearchRoleData {
			isData = true
		}

		if role == api.ElasticsearchRoleMaster {
			isMaster = true
		}
	}
	return map[api.ElasticsearchNodeRole]bool{
		api.ElasticsearchRoleClient: isClient,
		api.ElasticsearchRoleData:   isData,
		api.ElasticsearchRoleMaster: isMaster,
	}
}

func isMasterNode(node api.ElasticsearchNode) bool {
	for _, role := range node.Roles {
		if role == api.ElasticsearchRoleMaster {
			return true
		}
	}

	return false
}

func isDataNode(node api.ElasticsearchNode) bool {
	for _, role := range node.Roles {
		if role == api.ElasticsearchRoleData {
			return true
		}
	}

	return false
}

func newAffinity(roleMap map[api.ElasticsearchNodeRole]bool) *v1.Affinity {
	labelSelectorReqs := []metav1.LabelSelectorRequirement{}
	if roleMap[api.ElasticsearchRoleClient] {
		labelSelectorReqs = append(labelSelectorReqs, metav1.LabelSelectorRequirement{
			Key:      "es-node-client",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{"true"},
		})
	}
	if roleMap[api.ElasticsearchRoleData] {
		labelSelectorReqs = append(labelSelectorReqs, metav1.LabelSelectorRequirement{
			Key:      "es-node-data",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{"true"},
		})
	}
	if roleMap[api.ElasticsearchRoleMaster] {
		labelSelectorReqs = append(labelSelectorReqs, metav1.LabelSelectorRequirement{
			Key:      "es-node-master",
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{"true"},
		})
	}

	return &v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: labelSelectorReqs,
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}
}

func newElasticsearchContainer(imageName string, envVars []v1.EnvVar, resourceRequirements v1.ResourceRequirements) v1.Container {
	return v1.Container{
		Name:            "elasticsearch",
		Image:           imageName,
		ImagePullPolicy: "IfNotPresent",
		Env:             envVars,
		Ports: []v1.ContainerPort{
			{
				Name:          "cluster",
				ContainerPort: 9300,
				Protocol:      v1.ProtocolTCP,
			},
			{
				ContainerPort: 9200,
				Protocol:      v1.ProtocolTCP,
			},
		},
		ReadinessProbe: &v1.Probe{
			TimeoutSeconds:      30,
			InitialDelaySeconds: 10,
			PeriodSeconds:       5,
			ProbeHandler: v1.ProbeHandler{
				Exec: &v1.ExecAction{
					Command: []string{
						"/usr/share/elasticsearch/probe/readiness.sh",
					},
				},
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "elasticsearch-storage",
				MountPath: "/elasticsearch/persistent",
			},
			{
				Name:      "elasticsearch-config",
				MountPath: elasticsearchConfigPath,
			},
			{
				Name:      "certificates",
				MountPath: elasticsearchCertsPath,
			},
		},
		Resources:       resourceRequirements,
		SecurityContext: utils.ContainerSecurityContext(),
	}
}

func newProxyContainer(imageName, clusterName, namespace string, logConfig LogConfig, resourceRequirements v1.ResourceRequirements) v1.Container {
	container := v1.Container{
		Name:            "proxy",
		Image:           imageName,
		ImagePullPolicy: "IfNotPresent",
		Ports: []v1.ContainerPort{
			{
				Name:          "restapi",
				ContainerPort: 60000,
				Protocol:      v1.ProtocolTCP,
			},
			{
				Name:          "metrics",
				ContainerPort: 60001,
				Protocol:      v1.ProtocolTCP,
			},
		},
		Env: []v1.EnvVar{
			{
				Name:  "LOG_LEVEL",
				Value: logConfig.LogLevel,
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      fmt.Sprintf("%s-%s", clusterName, "metrics"),
				MountPath: "/etc/proxy/secrets",
				ReadOnly:  true,
			},
			{
				Name:      "certificates",
				MountPath: "/etc/proxy/elasticsearch",
				ReadOnly:  true,
			},
		},
		Args: []string{
			// HTTPS default listener for Elasticsearch
			"--listening-address=:60000",
			"--tls-cert=/etc/proxy/elasticsearch/logging-es.crt",
			"--tls-key=/etc/proxy/elasticsearch/logging-es.key",
			"--tls-client-ca=/etc/proxy/elasticsearch/admin-ca",

			// HTTPs listener for metrics
			"--metrics-listening-address=:60001",
			"--metrics-tls-cert=/etc/proxy/secrets/tls.crt",
			"--metrics-tls-key=/etc/proxy/secrets/tls.key",

			"--upstream-ca=/etc/proxy/elasticsearch/admin-ca",
			"--cache-expiry=60s",
			`--auth-backend-role=admin_reader={"namespace": "default", "verb": "get", "resource": "pods/log"}`,
			fmt.Sprintf(`--auth-backend-role=prometheus={"namespace":"%s", "verb": "get", "resource": "metrics", "resourceAPIGroup": "elasticsearch.openshift.io"}`, namespace),
			`--auth-backend-role=jaeger={"verb": "get", "resource": "/jaeger", "resourceAPIGroup": "elasticsearch.jaegertracing.io"}`,
			`--auth-backend-role=elasticsearch-operator={"namespace": "*", "verb": "*", "resource": "*", "resourceAPIGroup": "logging.openshift.io"}`,
			fmt.Sprintf("--auth-backend-role=index-management={\"namespace\":\"%s\", \"verb\": \"*\", \"resource\": \"indices\", \"resourceAPIGroup\": \"elasticsearch.openshift.io\"}", namespace),
			"--auth-admin-role=admin_reader",
			"--auth-default-role=project_user",
		},
		Resources:       resourceRequirements,
		SecurityContext: utils.ContainerSecurityContext(),
	}

	container.SecurityContext.ReadOnlyRootFilesystem = pointer.BoolPtr(true)

	return container
}

func newEnvVars(nodeName, clusterName, instanceRAM string, roleMap map[api.ElasticsearchNodeRole]bool) []v1.EnvVar {
	return []v1.EnvVar{
		{
			Name:  "DC_NAME",
			Value: nodeName,
		},
		{
			Name: "NAMESPACE",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name: "POD_IP",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
		{
			Name:  "KUBERNETES_MASTER",
			Value: "https://kubernetes.default.svc",
		},
		{
			Name:  "KUBERNETES_TRUST_CERT",
			Value: "true",
		},
		{
			Name:  "SERVICE_DNS",
			Value: fmt.Sprintf("%s-cluster", clusterName),
		},
		{
			Name:  "CLUSTER_NAME",
			Value: clusterName,
		},
		{
			Name:  "INSTANCE_RAM",
			Value: instanceRAM,
		},
		{
			Name:  "HEAP_DUMP_LOCATION",
			Value: heapDumpLocation,
		},
		{
			Name:  "RECOVER_AFTER_TIME",
			Value: "5m",
		},
		{
			Name:  "READINESS_PROBE_TIMEOUT",
			Value: "30",
		},
		{
			Name:  "POD_LABEL",
			Value: fmt.Sprintf("cluster=%s", clusterName),
		},
		{
			Name:  "IS_MASTER",
			Value: strconv.FormatBool(roleMap[api.ElasticsearchRoleMaster]),
		},
		{
			Name:  "HAS_DATA",
			Value: strconv.FormatBool(roleMap[api.ElasticsearchRoleData]),
		},
	}
}

// TODO: add isChanged check for labels and label selector
func newLabels(clusterName, nodeName string, roleMap map[api.ElasticsearchNodeRole]bool) map[string]string {
	return map[string]string{
		"es-node-client": strconv.FormatBool(roleMap[api.ElasticsearchRoleClient]),
		"es-node-data":   strconv.FormatBool(roleMap[api.ElasticsearchRoleData]),
		"es-node-master": strconv.FormatBool(roleMap[api.ElasticsearchRoleMaster]),
		"cluster-name":   clusterName,
		"component":      "elasticsearch",
		"node-name":      nodeName,
	}
}

func newLabelSelector(clusterName, nodeName string, roleMap map[api.ElasticsearchNodeRole]bool) map[string]string {
	return map[string]string{
		"es-node-client": strconv.FormatBool(roleMap[api.ElasticsearchRoleClient]),
		"es-node-data":   strconv.FormatBool(roleMap[api.ElasticsearchRoleData]),
		"es-node-master": strconv.FormatBool(roleMap[api.ElasticsearchRoleMaster]),
		"cluster-name":   clusterName,
		"node-name":      nodeName,
	}
}

func newPodTemplateSpec(ctx context.Context, logger logr.Logger, nodeName, clusterName, namespace string, node api.ElasticsearchNode, commonSpec api.ElasticsearchNodeSpec, labels map[string]string, roleMap map[api.ElasticsearchNodeRole]bool, client client.Client, logConfig LogConfig) v1.PodTemplateSpec {
	resourceRequirements := newESResourceRequirements(node.Resources, commonSpec.Resources)
	proxyResourceRequirements := newESProxyResourceRequirements(node.ProxyResources, commonSpec.ProxyResources)

	selectors := mergeSelectors(node.NodeSelector, commonSpec.NodeSelector)

	tolerations := appendTolerations(node.Tolerations, commonSpec.Tolerations)
	tolerations = appendTolerations(tolerations, []v1.Toleration{
		{
			Key:      "node.kubernetes.io/disk-pressure",
			Operator: v1.TolerationOpExists,
			Effect:   v1.TaintEffectNoSchedule,
		},
	})

	containers := []v1.Container{
		newElasticsearchContainer(
			getESImage(),
			newEnvVars(nodeName, clusterName, resourceRequirements.Limits.Memory().String(), roleMap),
			resourceRequirements,
		),
		newProxyContainer(
			getESProxyImage(),
			clusterName,
			namespace,
			logConfig,
			proxyResourceRequirements,
		),
	}

	volumes := newVolumes(ctx, logger, clusterName, nodeName, namespace, node, client)

	podSpec := pod.NewSpec(clusterName, containers, volumes).
		WithAffinity(newAffinity(roleMap)).
		WithNodeSelectors(selectors).
		WithTolerations(tolerations...).
		WithSecurityContext(utils.PodSecurityContext()).
		Build()

	return v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
		},
		Spec: *podSpec,
	}
}

// createUpdatablePodTemplateSpec creates a pod template from a copy of the update with
// some aspects of the current
func createUpdatablePodTemplateSpec(current, desired v1.PodTemplateSpec) v1.PodTemplateSpec {
	desiredCopy := desired
	desiredCopy.Spec.Volumes = current.Spec.Volumes

	return desiredCopy
}

func newESResourceRequirements(nodeResRequirements, commonResRequirements v1.ResourceRequirements) v1.ResourceRequirements {
	return newResourceRequirements(nodeResRequirements, commonResRequirements, defaultResources["elasticsearch"])
}

func newESProxyResourceRequirements(nodeResRequirements, commonResRequirements v1.ResourceRequirements) v1.ResourceRequirements {
	return newResourceRequirements(nodeResRequirements, commonResRequirements, defaultResources["proxy"])
}

func newResourceRequirements(nodeResRequirements, commonResRequirements, defaultRequirements v1.ResourceRequirements) v1.ResourceRequirements {
	// if only one resource (cpu or memory) is specified as a limit/request use it for the other value as well instead of
	//  using the defaults.

	var requestMem *resource.Quantity
	var limitMem *resource.Quantity
	var requestCPU *resource.Quantity
	var limitCPU *resource.Quantity

	// first check if either limit or resource is left off
	// Mem
	nodeLimitMem := nodeResRequirements.Limits.Memory()
	nodeRequestMem := nodeResRequirements.Requests.Memory()
	commonLimitMem := commonResRequirements.Limits.Memory()
	commonRequestMem := commonResRequirements.Requests.Memory()

	if commonRequestMem.IsZero() && commonLimitMem.IsZero() {
		// no common memory settings
		if nodeRequestMem.IsZero() && nodeLimitMem.IsZero() {
			// no node settings, use defaults
			lMem := defaultRequirements.Limits[v1.ResourceMemory]
			limitMem = &lMem

			rMem, _ := defaultRequirements.Requests[v1.ResourceMemory]
			requestMem = &rMem
		} else {
			// either one is not zero or both aren't zero but common is empty
			if nodeRequestMem.IsZero() {
				// request is zero use limit for both
				requestMem = nodeLimitMem
				limitMem = nodeLimitMem
			} else {
				if nodeLimitMem.IsZero() {
					// limit is zero use request for both
					requestMem = nodeRequestMem
					limitMem = nodeRequestMem
				} else {
					// both aren't zero
					requestMem = nodeRequestMem
					limitMem = nodeLimitMem
				}
			}
		}
	} else {
		// either one is not zero or both aren't zero (common)

		// check node for override
		if nodeRequestMem.IsZero() {
			// no node request mem, check that common has it
			if commonRequestMem.IsZero() {
				requestMem = commonLimitMem
			} else {
				requestMem = commonRequestMem
			}
		} else {
			requestMem = nodeRequestMem
		}

		if nodeLimitMem.IsZero() {
			// no node request mem, check that common has it
			if commonLimitMem.IsZero() {
				limitMem = commonRequestMem
			} else {
				limitMem = commonLimitMem
			}
		} else {
			limitMem = nodeLimitMem
		}
	}

	// CPU
	nodeLimitCPU := nodeResRequirements.Limits.Cpu()
	nodeRequestCPU := nodeResRequirements.Requests.Cpu()
	commonLimitCPU := commonResRequirements.Limits.Cpu()
	commonRequestCPU := commonResRequirements.Requests.Cpu()

	if commonRequestCPU.IsZero() && commonLimitCPU.IsZero() {
		// no common memory settings
		if nodeRequestCPU.IsZero() && nodeLimitCPU.IsZero() {
			// no node settings, use defaults
			rCPU, _ := defaultRequirements.Requests[v1.ResourceCPU]
			requestCPU = &rCPU
		} else {
			// either one is not zero or both aren't zero but common is empty
			if nodeRequestCPU.IsZero() {
				// request is zero use limit for both
				requestCPU = nodeLimitCPU
				limitCPU = nodeLimitCPU
			} else {
				if nodeLimitCPU.IsZero() {
					// limit is zero use request for both
					requestCPU = nodeRequestCPU
				} else {
					// both aren't zero
					requestCPU = nodeRequestCPU
					limitCPU = nodeLimitCPU
				}
			}
		}
	} else {
		// either one is not zero or both aren't zero (common)

		// check node for override
		if nodeRequestCPU.IsZero() {
			// no node request mem, check that common has it
			if commonRequestCPU.IsZero() {
				requestCPU = commonLimitCPU
			} else {
				requestCPU = commonRequestCPU
			}
		} else {
			requestCPU = nodeRequestCPU
		}

		if nodeLimitCPU.IsZero() {
			// no node request mem, check that common has it
			if !commonLimitCPU.IsZero() {
				limitCPU = commonLimitCPU
			}
		} else {
			limitCPU = nodeLimitCPU
		}
	}

	if limitCPU == nil {
		return v1.ResourceRequirements{
			Limits: v1.ResourceList{
				"memory": *limitMem,
			},
			Requests: v1.ResourceList{
				"cpu":    *requestCPU,
				"memory": *requestMem,
			},
		}
	}

	return v1.ResourceRequirements{
		Limits: v1.ResourceList{
			"cpu":    *limitCPU,
			"memory": *limitMem,
		},
		Requests: v1.ResourceList{
			"cpu":    *requestCPU,
			"memory": *requestMem,
		},
	}
}

func newVolumes(ctx context.Context, logger logr.Logger, clusterName, nodeName, namespace string, node api.ElasticsearchNode, client client.Client) []v1.Volume {
	return []v1.Volume{
		{
			Name: "elasticsearch-config",
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: clusterName,
					},
				},
			},
		},
		{
			Name:         "elasticsearch-storage",
			VolumeSource: newVolumeSource(ctx, logger, clusterName, nodeName, namespace, node, client),
		},
		{
			Name: "certificates",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: clusterName,
				},
			},
		},
		{
			Name: fmt.Sprintf("%s-%s", clusterName, "metrics"),
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					SecretName: fmt.Sprintf("%s-%s", clusterName, "metrics"),
				},
			},
		},
	}
}

func newVolumeSource(ctx context.Context, logger logr.Logger, clusterName, nodeName, namespace string, node api.ElasticsearchNode, client client.Client) v1.VolumeSource {
	specVol := node.Storage
	volSource := v1.VolumeSource{}

	// Ephemeral storage
	emptySpecVol := api.ElasticsearchStorageSpec{}
	if reflect.DeepEqual(specVol, emptySpecVol) {
		volSource.EmptyDir = &v1.EmptyDirVolumeSource{}
		return volSource
	}

	// in the case where we do not have a size provided we need to
	// fall back to using ephemeral storage since a pvc requires a size
	if specVol.Size == nil {
		volSource.EmptyDir = &v1.EmptyDirVolumeSource{}
		return volSource
	}

	// Persistent storage
	claimName := fmt.Sprintf("%s-%s", clusterName, nodeName)
	volSource.PersistentVolumeClaim = &v1.PersistentVolumeClaimVolumeSource{
		ClaimName: claimName,
	}

	pvcLabels := map[string]string{
		"logging-cluster": clusterName,
	}
	pvc := persistentvolume.NewPVC(claimName, namespace, pvcLabels)
	pvc.Spec = v1.PersistentVolumeClaimSpec{
		AccessModes: []v1.PersistentVolumeAccessMode{
			v1.ReadWriteOnce,
		},
		Resources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceStorage: *specVol.Size,
			},
		},
		StorageClassName: specVol.StorageClassName,
	}

	// TODO: This create PVC functionality needs to move from being part of
	// the template creation. It should idealy be in where the pod template
	// (deployment/statefulset) is create or maintained.
	err := persistentvolume.CreateOrUpdatePVC(ctx, client, pvc, persistentvolume.LabelsEqual, persistentvolume.MutateLabelsOnly)
	if err != nil {
		logger.Error(err, "Unable to create PersistentVolumeClaim")
	}
	return volSource
}

/*
kind: NetworkPolicy
apiVersion: networking.k8s.io/v1
metadata:
  name: restricted-es-access
spec:
  podSelector:
    matchLabels:
      component: elasticsearch
  ingress:
  - from:
    - podSelector:
        matchLabels:
          name: elasticsearch-operator
    ports:
    - protocol: TCP
      port: 9200
*/
func newNetworkPolicy(namespace string) networking.NetworkPolicy {
	protocol := v1.ProtocolTCP
	port := intstr.FromInt(9200)
	internalPort := intstr.FromInt(9300)

	return networking.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "NetworkPolicy",
			APIVersion: networking.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restricted-es-policy",
			Namespace: namespace,
		},
		Spec: networking.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"component": "elasticsearch",
				},
			},
			Ingress: []networking.NetworkPolicyIngressRule{
				{
					From: []networking.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"name": "elasticsearch-operator",
								},
							},
							// This needs to be present but empty so it will select all namespaces
							// since we do not have a label for our operator namespace
							NamespaceSelector: &metav1.LabelSelector{},
						},
					},
					Ports: []networking.NetworkPolicyPort{
						{
							Protocol: &protocol,
							Port:     &port,
						},
					},
				},
				{
					From: []networking.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"component": "elasticsearch",
								},
							},
						},
					},
					Ports: []networking.NetworkPolicyPort{
						{
							Protocol: &protocol,
							Port:     &port,
						},
					},
				},
				{
					From: []networking.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"component": "elasticsearch",
								},
							},
						},
					},
					Ports: []networking.NetworkPolicyPort{
						{
							Protocol: &protocol,
							Port:     &internalPort,
						},
					},
				},
			},
		},
	}
}

func EnforceNetworkPolicy(namespace string, client client.Client, ownerRef []metav1.OwnerReference) error {
	policy := newNetworkPolicy(namespace)
	policy.ObjectMeta.OwnerReferences = ownerRef

	err := client.Create(context.TODO(), &policy)
	if err != nil {
		if !apierrors.IsAlreadyExists(kverrors.Root(err)) {
			return kverrors.Wrap(err, "failed to create network policy")
		}
	}

	return nil
}

func RelaxNetworkPolicy(namespace string, client client.Client) error {
	policy := newNetworkPolicy(namespace)
	err := client.Delete(context.TODO(), &policy)
	if err != nil {
		if !apierrors.IsNotFound(kverrors.Root(err)) {
			return kverrors.Wrap(err, "failed to delete network policy")
		}
	}

	return nil
}
