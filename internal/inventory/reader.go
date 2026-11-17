package inventory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	acceleratorv1alpha1 "github.com/qcrs/accelerator-control-plane/api/v1alpha1"
	"github.com/qcrs/accelerator-control-plane/internal/contracts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type KubernetesReader struct {
	Client    client.Reader
	Validator contracts.RemoteEndpointValidator
}

func NewKubernetesReader(reader client.Reader, validator contracts.RemoteEndpointValidator) *KubernetesReader {
	return &KubernetesReader{Client: reader, Validator: validator}
}

func (r *KubernetesReader) ListCandidates(ctx context.Context, request contracts.SelectionRequest) ([]contracts.Candidate, error) {
	if r.Client == nil {
		return nil, contracts.NewError(contracts.ErrorKindInvalid, "read inventory", "Kubernetes client is nil")
	}
	if r.Validator == nil {
		return nil, contracts.NewError(contracts.ErrorKindInvalid, "read inventory", "remote endpoint validator is nil")
	}

	var pools acceleratorv1alpha1.AcceleratorPoolList
	var providers acceleratorv1alpha1.AcceleratorProviderList
	var nodes corev1.NodeList
	var pods corev1.PodList
	var endpoints acceleratorv1alpha1.AcceleratorEndpointList
	var leases acceleratorv1alpha1.AcceleratorLeaseList

	objects := []client.ObjectList{&pools, &providers, &nodes, &pods, &endpoints, &leases}
	for _, object := range objects {
		if err := r.Client.List(ctx, object); err != nil {
			return nil, contracts.WrapError(contracts.ErrorKindTransient, "read inventory", fmt.Sprintf("list %T", object), err)
		}
	}

	type providerProfile struct {
		driver string
		kinds  map[acceleratorv1alpha1.DeviceKind]struct{}
		ready  bool
	}
	providerProfiles := make(map[string]providerProfile, len(providers.Items))
	for index := range providers.Items {
		provider := &providers.Items[index]
		kinds := make(map[acceleratorv1alpha1.DeviceKind]struct{}, len(provider.Spec.DeviceKinds))
		for _, kind := range provider.Spec.DeviceKinds {
			kinds[kind] = struct{}{}
		}
		providerProfiles[provider.Name] = providerProfile{
			driver: provider.Spec.Driver,
			kinds:  kinds,
			ready:  conditionReadyOrUnknown(provider.Status.Conditions, provider.Generation),
		}
	}

	poolSelector := labels.Everything()
	if request.Placement.PoolSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(request.Placement.PoolSelector)
		if err != nil {
			return nil, contracts.WrapError(contracts.ErrorKindInvalid, "read inventory", "invalid pool selector", err)
		}
		poolSelector = selector
	}

	occupied := OccupiedRemoteCapacity(leases.Items)
	localPoolResources := make(map[string]corev1.ResourceName, len(pools.Items))
	for index := range pools.Items {
		pool := &pools.Items[index]
		if pool.Spec.Location == acceleratorv1alpha1.ExecutionModeLocal && pool.Spec.Local != nil && pool.Spec.Local.ResourceName != "" {
			localPoolResources[pool.Name] = pool.Spec.Local.ResourceName
		}
	}
	candidates := make([]contracts.Candidate, 0, len(nodes.Items)+len(endpoints.Items))
	for index := range pools.Items {
		pool := &pools.Items[index]
		if pool.Spec.Unschedulable || !conditionReadyOrUnknown(pool.Status.Conditions, pool.Generation) || pool.Spec.DeviceType != request.Accelerator.DeviceType ||
			!poolSelector.Matches(labels.Set(pool.Labels)) {
			continue
		}
		profile, exists := providerProfiles[pool.Spec.ProviderRef.Name]
		if !exists || !profile.ready {
			continue
		}
		if _, supported := profile.kinds[pool.Spec.DeviceType]; !supported {
			continue
		}

		switch pool.Spec.Location {
		case acceleratorv1alpha1.ExecutionModeLocal:
			candidates = append(candidates, localCandidates(pool, &request.PodTemplate.Spec, nodes.Items, pods.Items, leases.Items, localPoolResources)...)
		case acceleratorv1alpha1.ExecutionModeRemote:
			candidates = append(candidates, remoteCandidates(pool, profile.driver, endpoints.Items, occupied, r.Validator)...)
		}
	}
	return candidates, nil
}

func conditionReadyOrUnknown(conditions []metav1.Condition, generation int64) bool {
	condition := findCondition(conditions, "Ready")
	return condition == nil || (condition.ObservedGeneration == generation && condition.Status == metav1.ConditionTrue)
}

func localCandidates(
	pool *acceleratorv1alpha1.AcceleratorPool,
	podSpec *corev1.PodSpec,
	nodes []corev1.Node,
	pods []corev1.Pod,
	leases []acceleratorv1alpha1.AcceleratorLease,
	poolResources map[string]corev1.ResourceName,
) []contracts.Candidate {
	if pool.Spec.Local == nil || pool.Spec.Local.ResourceName == "" {
		return nil
	}
	result := make([]contracts.Candidate, 0, len(nodes))
	for index := range nodes {
		node := &nodes[index]
		if !nodeReady(node) || !containsLabels(node.Labels, pool.Spec.Local.NodeSelector) || !nodeMatchesPod(node, podSpec) {
			continue
		}
		available := node.Status.Allocatable[pool.Spec.Local.ResourceName].DeepCopy()
		used := RequestedQuantityOnNode(pods, node.Name, pool.Spec.Local.ResourceName)
		reserved := ReservedQuantityOnNode(leases, pods, poolResources, pool.Spec.Local.ResourceName, node.Name)
		available.Sub(used)
		available.Sub(reserved)
		if available.Sign() < 0 {
			available = *resource.NewQuantity(0, resource.DecimalSI)
		}

		vendorFallback := pool.Spec.Local.Vendor
		if vendorFallback == "" {
			vendorFallback = vendorFromResourceName(pool.Spec.Local.ResourceName)
		}
		vendor := labelOr(node.Labels, pool.Spec.Local.VendorLabelKey, vendorFallback)
		model := labelOr(node.Labels, pool.Spec.Local.ModelLabelKey, pool.Spec.Local.Model)
		memory := quantityLabelOr(node.Labels, pool.Spec.Local.MemoryLabelKey, pool.Spec.Local.Memory)
		resourceForm := effectiveResourceForm(pool.Spec.Local.ResourceForm)
		result = append(result, contracts.Candidate{
			ID:                "local/" + pool.Name + "/" + node.Name,
			ProviderName:      pool.Spec.ProviderRef.Name,
			PoolName:          pool.Name,
			PoolPriority:      pool.Spec.Priority,
			Mode:              acceleratorv1alpha1.ExecutionModeLocal,
			DeviceType:        pool.Spec.DeviceType,
			NodeName:          node.Name,
			ResourceName:      pool.Spec.Local.ResourceName,
			ResourceForm:      resourceForm,
			Profile:           pool.Spec.Local.Profile,
			Isolation:         pool.Spec.Local.Isolation,
			Vendor:            vendor,
			Model:             model,
			DeviceMemory:      memory,
			Labels:            mergeLabels(pool.Labels, node.Labels),
			Attributes:        copyStringMap(pool.Spec.Local.Attributes),
			Available:         clampCapacity(available.Value()),
			AvailableQuantity: available,
			Ready:             true,
		})
	}
	return result
}

func remoteCandidates(
	pool *acceleratorv1alpha1.AcceleratorPool,
	providerDriver string,
	endpoints []acceleratorv1alpha1.AcceleratorEndpoint,
	occupied RemoteOccupancy,
	validator contracts.RemoteEndpointValidator,
) []contracts.Candidate {
	if pool.Spec.Remote == nil {
		return nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&pool.Spec.Remote.EndpointSelector)
	if err != nil {
		return nil
	}
	result := make([]contracts.Candidate, 0, len(endpoints))
	exclusive := pool.Spec.Remote.AllocationMode != acceleratorv1alpha1.RemoteAllocationModeDevice
	duplicateUUIDs := duplicateDeviceUUIDs(endpoints)
	for index := range endpoints {
		endpoint := &endpoints[index]
		if endpoint.Spec.PoolRef.Name != pool.Name || endpoint.Spec.Unschedulable ||
			!selector.Matches(labels.Set(endpoint.Labels)) {
			continue
		}
		ready, reason := endpointReady(endpoint, providerDriver)
		if ready {
			for _, device := range endpoint.Spec.Devices {
				if _, duplicate := duplicateUUIDs[device.UUID]; device.UUID != "" && duplicate {
					ready = false
					reason = fmt.Sprintf("device UUID %q is declared by multiple endpoints", device.UUID)
					break
				}
			}
		}
		if ready && validator != nil {
			if err := validator.ValidateEndpoint(remoteEndpoint(endpoint)); err != nil {
				ready = false
				reason = err.Error()
			}
		}
		if _, endpointInUse := occupied.Endpoints[endpoint.Name]; endpointInUse {
			ready = false
			reason = "endpoint is exclusively reserved"
		}
		if exclusive && len(occupied.Devices[endpoint.Name]) > 0 {
			ready = false
			reason = "endpoint has device reservations and cannot be reserved exclusively"
		}
		freeDevices := make([]contracts.Device, 0, len(endpoint.Spec.Devices))
		for _, device := range endpoint.Spec.Devices {
			if device.Health != acceleratorv1alpha1.DeviceHealthHealthy {
				continue
			}
			if _, inUse := occupied.Devices[endpoint.Name][device.ID]; inUse {
				continue
			}
			freeDevices = append(freeDevices, contracts.Device{
				ID:           device.ID,
				Vendor:       device.Vendor,
				Model:        device.Model,
				Memory:       device.Memory.DeepCopy(),
				ResourceForm: effectiveEndpointResourceForm(device.ResourceForm),
				Profile:      device.Profile,
				Isolation:    device.Isolation,
				Attributes:   copyStringMap(device.Attributes),
			})
		}
		sort.Slice(freeDevices, func(i, j int) bool { return freeDevices[i].ID < freeDevices[j].ID })
		result = append(result, contracts.Candidate{
			ID:           "remote/" + pool.Name + "/" + endpoint.Name,
			ProviderName: pool.Spec.ProviderRef.Name,
			PoolName:     pool.Name,
			PoolPriority: pool.Spec.Priority,
			Mode:         acceleratorv1alpha1.ExecutionModeRemote,
			DeviceType:   pool.Spec.DeviceType,
			EndpointName: endpoint.Name,
			Exclusive:    exclusive,
			Labels:       mergeLabels(pool.Labels, endpoint.Labels),
			Available:    int32(len(freeDevices)),
			Devices:      freeDevices,
			Ready:        ready,
			Reason:       reason,
		})
	}
	return result
}

func duplicateDeviceUUIDs(endpoints []acceleratorv1alpha1.AcceleratorEndpoint) map[string]struct{} {
	owners := map[string]string{}
	duplicates := map[string]struct{}{}
	for index := range endpoints {
		for _, device := range endpoints[index].Spec.Devices {
			if device.UUID == "" {
				continue
			}
			if owner, exists := owners[device.UUID]; exists && owner != endpoints[index].Name {
				duplicates[device.UUID] = struct{}{}
				continue
			}
			owners[device.UUID] = endpoints[index].Name
		}
	}
	return duplicates
}

func remoteEndpoint(endpoint *acceleratorv1alpha1.AcceleratorEndpoint) contracts.RemoteEndpoint {
	return contracts.RemoteEndpoint{
		Name:          endpoint.Name,
		PoolName:      endpoint.Spec.PoolRef.Name,
		Address:       endpoint.Spec.Address,
		Runtime:       endpoint.Spec.Runtime,
		Devices:       endpoint.Spec.Devices,
		Parameters:    endpoint.Spec.Parameters,
		Unschedulable: endpoint.Spec.Unschedulable,
	}
}

func endpointReady(
	endpoint *acceleratorv1alpha1.AcceleratorEndpoint,
	providerDriver string,
) (bool, string) {
	if endpoint.Spec.Address == "" {
		return false, "runtime address is empty"
	}
	if endpoint.Spec.Runtime.Driver == "" || endpoint.Spec.Runtime.Driver != providerDriver {
		return false, fmt.Sprintf("runtime driver %q does not match provider driver %q", endpoint.Spec.Runtime.Driver, providerDriver)
	}
	if len(endpoint.Spec.Devices) == 0 {
		return false, "no devices are declared"
	}
	deviceIDs := map[string]struct{}{}
	deviceUUIDs := map[string]struct{}{}
	for _, device := range endpoint.Spec.Devices {
		if device.ID == "" {
			return false, "device ID is empty"
		}
		if _, duplicate := deviceIDs[device.ID]; duplicate {
			return false, fmt.Sprintf("device ID %q is duplicated", device.ID)
		}
		deviceIDs[device.ID] = struct{}{}
		if device.UUID == "" {
			continue
		}
		if _, duplicate := deviceUUIDs[device.UUID]; duplicate {
			return false, fmt.Sprintf("device UUID %q is duplicated", device.UUID)
		}
		deviceUUIDs[device.UUID] = struct{}{}
	}
	if condition := findCondition(endpoint.Status.Conditions, "Configured"); condition != nil {
		if condition.ObservedGeneration != endpoint.Generation {
			return false, fmt.Sprintf("Configured condition is stale for generation %d", endpoint.Generation)
		}
		if condition.Status != metav1.ConditionTrue {
			return false, fmt.Sprintf("Configured condition is %s: %s", condition.Status, condition.Message)
		}
	}
	if condition := findCondition(endpoint.Status.Conditions, "Ready"); condition != nil {
		if condition.ObservedGeneration != endpoint.Generation {
			return false, fmt.Sprintf("Ready condition is stale for generation %d", endpoint.Generation)
		}
		if condition.Status != metav1.ConditionTrue {
			return false, fmt.Sprintf("Ready condition is %s: %s", condition.Status, condition.Message)
		}
	}
	return true, ""
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeMatchesPod(node *corev1.Node, podSpec *corev1.PodSpec) bool {
	if podSpec == nil {
		return true
	}
	if node.Spec.Unschedulable || (podSpec.NodeName != "" && podSpec.NodeName != node.Name) ||
		!containsLabels(node.Labels, podSpec.NodeSelector) || !toleratesHardTaints(node.Spec.Taints, podSpec.Tolerations) {
		return false
	}
	if podSpec.Affinity == nil || podSpec.Affinity.NodeAffinity == nil ||
		podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	selector := podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	for _, term := range selector.NodeSelectorTerms {
		if nodeSelectorTermMatches(node, term) {
			return true
		}
	}
	return false
}

func nodeSelectorTermMatches(node *corev1.Node, term corev1.NodeSelectorTerm) bool {
	for _, requirement := range term.MatchExpressions {
		if !selectorRequirementMatches(node.Labels[requirement.Key], hasKey(node.Labels, requirement.Key), requirement) {
			return false
		}
	}
	for _, requirement := range term.MatchFields {
		if requirement.Key != "metadata.name" || !selectorRequirementMatches(node.Name, true, requirement) {
			return false
		}
	}
	return true
}

func selectorRequirementMatches(value string, exists bool, requirement corev1.NodeSelectorRequirement) bool {
	switch requirement.Operator {
	case corev1.NodeSelectorOpIn:
		return exists && stringIn(value, requirement.Values)
	case corev1.NodeSelectorOpNotIn:
		return !exists || !stringIn(value, requirement.Values)
	case corev1.NodeSelectorOpExists:
		return exists
	case corev1.NodeSelectorOpDoesNotExist:
		return !exists
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		if !exists || len(requirement.Values) != 1 {
			return false
		}
		actual, actualErr := strconv.ParseInt(value, 10, 64)
		expected, expectedErr := strconv.ParseInt(requirement.Values[0], 10, 64)
		if actualErr != nil || expectedErr != nil {
			return false
		}
		if requirement.Operator == corev1.NodeSelectorOpGt {
			return actual > expected
		}
		return actual < expected
	default:
		return false
	}
}

func toleratesHardTaints(taints []corev1.Taint, tolerations []corev1.Toleration) bool {
	for _, taint := range taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for _, toleration := range tolerations {
			if toleration.Effect != "" && toleration.Effect != taint.Effect {
				continue
			}
			switch toleration.Operator {
			case "", corev1.TolerationOpEqual:
				tolerated = toleration.Key == taint.Key && toleration.Value == taint.Value
			case corev1.TolerationOpExists:
				tolerated = toleration.Key == "" || toleration.Key == taint.Key
			}
			if tolerated {
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

func hasKey(values map[string]string, key string) bool {
	_, exists := values[key]
	return exists
}

func stringIn(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}

func quantityLabel(values map[string]string, key string) resource.Quantity {
	if key == "" || values[key] == "" {
		return resource.Quantity{}
	}
	quantity, err := resource.ParseQuantity(values[key])
	if err != nil {
		return resource.Quantity{}
	}
	return quantity
}

func quantityLabelOr(values map[string]string, key string, fallback resource.Quantity) resource.Quantity {
	quantity := quantityLabel(values, key)
	if !quantity.IsZero() {
		return quantity
	}
	return fallback.DeepCopy()
}

func effectiveResourceForm(form acceleratorv1alpha1.ResourceForm) acceleratorv1alpha1.ResourceForm {
	if form == "" {
		return acceleratorv1alpha1.ResourceFormCustom
	}
	return form
}

func effectiveEndpointResourceForm(form acceleratorv1alpha1.ResourceForm) acceleratorv1alpha1.ResourceForm {
	if form == "" {
		return acceleratorv1alpha1.ResourceFormWholeDevice
	}
	return form
}

func labelOr(values map[string]string, key, fallback string) string {
	if key != "" && values[key] != "" {
		return values[key]
	}
	return fallback
}

func vendorFromResourceName(name corev1.ResourceName) string {
	prefix, _, found := strings.Cut(string(name), ".com/")
	if !found {
		return ""
	}
	return prefix
}

func containsLabels(actual, required map[string]string) bool {
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func mergeLabels(first, second map[string]string) map[string]string {
	result := make(map[string]string, len(first)+len(second))
	for key, value := range first {
		result[key] = value
	}
	for key, value := range second {
		result[key] = value
	}
	return result
}

func copyStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
