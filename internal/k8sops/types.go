package k8sops

// objectMeta is the subset of metadata the shared cache and the Console
// snapshots need. Every cached object is immutable once inserted, so snapshots
// can hand out pointers without copying.
type objectMeta struct {
	Name              string           `json:"name"`
	Namespace         string           `json:"namespace"`
	UID               string           `json:"uid"`
	Generation        int64            `json:"generation"`
	ResourceVersion   string           `json:"resourceVersion"`
	CreationTimestamp string           `json:"creationTimestamp"`
	OwnerReferences   []ownerReference `json:"ownerReferences"`
}

// ownerReference is the Kubernetes ownership edge used by the restart tracker
// and the workload/pod association.
type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller *bool  `json:"controller"`
}

// cachedObject is implemented by every cached resource kind. The cache only
// needs the identity fields.
type cachedObject interface {
	meta() *objectMeta
}

// condition is one status condition of a workload.
type condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type deploymentObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int32 `json:"replicas"`
		Paused   bool   `json:"paused"`
		Strategy struct {
			Type string `json:"type"`
		} `json:"strategy"`
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration  int64       `json:"observedGeneration"`
		Replicas            int32       `json:"replicas"`
		UpdatedReplicas     int32       `json:"updatedReplicas"`
		ReadyReplicas       int32       `json:"readyReplicas"`
		AvailableReplicas   int32       `json:"availableReplicas"`
		UnavailableReplicas int32       `json:"unavailableReplicas"`
		Conditions          []condition `json:"conditions"`
	} `json:"status"`
}

func (d *deploymentObject) meta() *objectMeta { return &d.Metadata }

type statefulSetObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Replicas       *int32 `json:"replicas"`
		UpdateStrategy struct {
			Type          string `json:"type"`
			RollingUpdate struct {
				Partition *int32 `json:"partition"`
			} `json:"rollingUpdate"`
		} `json:"updateStrategy"`
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64  `json:"observedGeneration"`
		Replicas           int32  `json:"replicas"`
		ReadyReplicas      int32  `json:"readyReplicas"`
		AvailableReplicas  int32  `json:"availableReplicas"`
		UpdatedReplicas    int32  `json:"updatedReplicas"`
		CurrentReplicas    int32  `json:"currentReplicas"`
		CurrentRevision    string `json:"currentRevision"`
		UpdateRevision     string `json:"updateRevision"`
	} `json:"status"`
}

func (s *statefulSetObject) meta() *objectMeta { return &s.Metadata }

type daemonSetObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		UpdateStrategy struct {
			Type string `json:"type"`
		} `json:"updateStrategy"`
		Template struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration     int64 `json:"observedGeneration"`
		DesiredNumberScheduled int32 `json:"desiredNumberScheduled"`
		CurrentNumberScheduled int32 `json:"currentNumberScheduled"`
		UpdatedNumberScheduled int32 `json:"updatedNumberScheduled"`
		NumberReady            int32 `json:"numberReady"`
		NumberAvailable        int32 `json:"numberAvailable"`
		NumberMisscheduled     int32 `json:"numberMisscheduled"`
	} `json:"status"`
}

func (d *daemonSetObject) meta() *objectMeta { return &d.Metadata }

type replicaSetObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration   int64 `json:"observedGeneration"`
		Replicas             int32 `json:"replicas"`
		ReadyReplicas        int32 `json:"readyReplicas"`
		AvailableReplicas    int32 `json:"availableReplicas"`
		FullyLabeledReplicas int32 `json:"fullyLabeledReplicas"`
	} `json:"status"`
}

func (r *replicaSetObject) meta() *objectMeta { return &r.Metadata }

type replicationControllerObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int32 `json:"replicas"`
		ReadyReplicas      int32 `json:"readyReplicas"`
		AvailableReplicas  int32 `json:"availableReplicas"`
	} `json:"status"`
}

func (r *replicationControllerObject) meta() *objectMeta { return &r.Metadata }

type jobObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Completions *int32 `json:"completions"`
		Parallelism *int32 `json:"parallelism"`
		Suspend     *bool  `json:"suspend"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64       `json:"observedGeneration"`
		Active             int32       `json:"active"`
		Succeeded          int32       `json:"succeeded"`
		Failed             int32       `json:"failed"`
		Conditions         []condition `json:"conditions"`
	} `json:"status"`
}

func (j *jobObject) meta() *objectMeta { return &j.Metadata }

type cronJobObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Schedule string `json:"schedule"`
		Suspend  *bool  `json:"suspend"`
	} `json:"spec"`
	Status struct {
		Active           []struct{} `json:"active"`
		LastScheduleTime string     `json:"lastScheduleTime"`
	} `json:"status"`
}

func (c *cronJobObject) meta() *objectMeta { return &c.Metadata }

type containerStatus struct {
	LastState struct{ Terminated *struct{ Reason string } }

	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Terminated *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"terminated"`
	} `json:"state"`
}

type memoryResources struct {
	Requests map[string]string
	Limits   map[string]string
}
type podContainer struct {
	Name      string
	Resources memoryResources
}
type podObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName            string `json:"nodeName"`
		Containers          []podContainer
		InitContainers      []podContainer
		EphemeralContainers []podContainer
		Priority            *int32
		PriorityClassName   string
		Resources           memoryResources
	} `json:"spec"`
	Status struct {
		Phase                      string            `json:"phase"`
		Reason                     string            `json:"reason"`
		Message                    string            `json:"message"`
		Conditions                 []condition       `json:"conditions"`
		ContainerStatuses          []containerStatus `json:"containerStatuses"`
		InitContainerStatuses      []containerStatus
		EphemeralContainerStatuses []containerStatus
	} `json:"status"`
}

func (p *podObject) meta() *objectMeta { return &p.Metadata }

// resourceSpec describes one of the eight cluster-wide list/watch streams.
type resourceSpec struct {
	// kind is the Kubernetes kind shown on the Console row.
	kind string
	// resource is the plural lowercase resource name, also the JSON field the
	// Console posts back for restarts.
	resource string
	// apiPath is the API group prefix ("" -> core, "/apis/apps/v1", ...).
	apiPath string
	// newObject allocates the typed decoding target.
	newObject func() cachedObject
}

var (
	specDeployments = resourceSpec{
		kind: "Deployment", resource: ResourceDeployments, apiPath: "/apis/apps/v1",
		newObject: func() cachedObject { return &deploymentObject{} },
	}
	specStatefulSets = resourceSpec{
		kind: "StatefulSet", resource: ResourceStatefulSets, apiPath: "/apis/apps/v1",
		newObject: func() cachedObject { return &statefulSetObject{} },
	}
	specDaemonSets = resourceSpec{
		kind: "DaemonSet", resource: ResourceDaemonSets, apiPath: "/apis/apps/v1",
		newObject: func() cachedObject { return &daemonSetObject{} },
	}
	specReplicaSets = resourceSpec{
		kind: "ReplicaSet", resource: "replicasets", apiPath: "/apis/apps/v1",
		newObject: func() cachedObject { return &replicaSetObject{} },
	}
	specReplicationControllers = resourceSpec{
		kind: "ReplicationController", resource: "replicationcontrollers", apiPath: "/api/v1",
		newObject: func() cachedObject { return &replicationControllerObject{} },
	}
	specPods = resourceSpec{
		kind: "Pod", resource: "pods", apiPath: "/api/v1",
		newObject: func() cachedObject { return &podObject{} },
	}
	specJobs = resourceSpec{
		kind: "Job", resource: "jobs", apiPath: "/apis/batch/v1",
		newObject: func() cachedObject { return &jobObject{} },
	}
	specCronJobs = resourceSpec{
		kind: "CronJob", resource: "cronjobs", apiPath: "/apis/batch/v1",
		newObject: func() cachedObject { return &cronJobObject{} },
	}

	// resourceSpecs is the complete watch set (workloads + pods).
	resourceSpecs = []resourceSpec{
		specDeployments,
		specStatefulSets,
		specDaemonSets,
		specReplicaSets,
		specReplicationControllers,
		specJobs,
		specCronJobs,
		specPods,
	}
	// workloadSpecs is the subset rendered by Workloads (pods are nested).
	workloadSpecs = []resourceSpec{
		specDeployments,
		specStatefulSets,
		specDaemonSets,
		specReplicaSets,
		specReplicationControllers,
		specJobs,
		specCronJobs,
	}
	// resourceSpecByName maps the Console-facing resource name to its stream.
	resourceSpecByName = func() map[string]resourceSpec {
		lookup := make(map[string]resourceSpec, len(resourceSpecs))
		for _, spec := range resourceSpecs {
			lookup[spec.resource] = spec
		}
		return lookup
	}()
)

// specForResource resolves the plural resource name accepted by the restart
// endpoint and the snapshots.
func specForResource(resource string) (resourceSpec, bool) {
	spec, ok := resourceSpecByName[resource]
	return spec, ok
}

// desiredReplicas applies the Kubernetes default of one replica when
// spec.replicas is omitted.
func desiredReplicas(value *int32) int32 {
	if value == nil {
		return 1
	}
	return *value
}

// controllerOwner returns the controller owner reference, falling back to the
// first reference (some controllers omit the controller flag).
func controllerOwner(references []ownerReference) *ownerReference {
	if len(references) == 0 {
		return nil
	}
	for index := range references {
		reference := &references[index]
		if reference.Controller != nil && *reference.Controller {
			return reference
		}
	}
	return &references[0]
}

// condition returns the named condition when present.
func findCondition(conditions []condition, conditionType string) *condition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}
