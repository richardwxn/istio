// Copyright Istio Authors
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

package helmreconciler

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"istio.io/api/label"
	"istio.io/api/operator/v1alpha1"
	"istio.io/istio/operator/pkg/cache"
	"istio.io/istio/operator/pkg/name"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/pkg/proxy"
)

const (
	istioDefaultNamespace = "istio-system"
)

var (
	// NamespacedResources orders non cluster scope resources types which should be deleted, first to last
	NamespacedResources = []schema.GroupVersionKind{
		{Group: "autoscaling", Version: "v2beta1", Kind: name.HPAStr},
		{Group: "policy", Version: "v1beta1", Kind: name.PDBStr},
		{Group: "apps", Version: "v1", Kind: name.DeploymentStr},
		{Group: "apps", Version: "v1", Kind: name.DaemonSetStr},
		{Group: "", Version: "v1", Kind: name.ServiceStr},
		{Group: "", Version: "v1", Kind: name.CMStr},
		{Group: "", Version: "v1", Kind: name.PVCStr},
		{Group: "", Version: "v1", Kind: name.PodStr},
		{Group: "", Version: "v1", Kind: name.SecretStr},
		{Group: "", Version: "v1", Kind: name.SAStr},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: name.RoleBindingStr},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: name.RoleStr},
		{Group: name.NetworkingAPIGroupName, Version: "v1alpha3", Kind: name.DestinationRuleStr},
		{Group: name.NetworkingAPIGroupName, Version: "v1alpha3", Kind: name.EnvoyFilterStr},
		{Group: name.NetworkingAPIGroupName, Version: "v1alpha3", Kind: name.GatewayStr},
		{Group: name.NetworkingAPIGroupName, Version: "v1alpha3", Kind: name.VirtualServiceStr},
		{Group: name.SecurityAPIGroupName, Version: "v1beta1", Kind: name.PeerAuthenticationStr},
	}

	// ClusterResources are resource types the operator prunes, ordered by which types should be deleted, first to last.
	ClusterResources = []schema.GroupVersionKind{
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: name.MutatingWebhookConfigurationStr},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: name.ValidatingWebhookConfigurationStr},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: name.ClusterRoleStr},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: name.ClusterRoleBindingStr},
		// Cannot currently prune CRDs because this will also wipe out user config.
		// {Group: "apiextensions.k8s.io", Version: "v1beta1", Kind: name.CRDStr},
	}
	// NonNamespacedCPResources lists cluster scope shared resources types which should be deleted during uninstall.
	NonNamespacedCPResources = []schema.GroupVersionKind{
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: name.MutatingWebhookConfigurationStr},
	}
	AllClusterResources = append(NonNamespacedCPResources,
		schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1beta1", Kind: name.CRDStr})
)

// Prune removes any resources not specified in manifests generated by HelmReconciler h.
func (h *HelmReconciler) Prune(manifests name.ManifestMap, all bool) error {
	return h.runForAllTypes(func(labels map[string]string, objects *unstructured.UnstructuredList) error {
		var errs util.Errors
		if all {
			errs = util.AppendErr(errs, h.deleteResources(nil, labels, "", objects, all))
		} else {
			for cname, manifest := range manifests.Consolidated() {
				errs = util.AppendErr(errs, h.deleteResources(object.AllObjectHashes(manifest), labels, cname, objects, all))
			}
		}
		return errs.ToError()
	})
}

// PruneControlPlaneByRevisionWithController is called to remove specific control plane revision in specific namespace
// during reconciliation process of controller.
// It returns the install status and any error encountered.
func (h *HelmReconciler) PruneControlPlaneByRevisionWithController(ns, revision string) (*v1alpha1.InstallStatus, error) {
	if ns == "" {
		ns = istioDefaultNamespace
	}
	errStatus := &v1alpha1.InstallStatus{Status: v1alpha1.InstallStatus_ERROR}
	pids, err := proxy.GetIDsFromProxyInfo("", "", revision, ns)
	if err != nil {
		return errStatus,
			fmt.Errorf("failed to check proxy infos: %v", err)
	}
	// TODO(richardwxn): add warning message together with the status
	if len(pids) != 0 {
		return errStatus,
			fmt.Errorf("there are proxies still pointing to the pruned control plane: %s",
				strings.Join(pids, " "))
	}
	uslist, _, err := h.GetPrunedResourcesByRevision(revision, false)
	if err != nil {
		return errStatus, err
	}
	if err := h.DeleteControlPlaneByRevision(revision, uslist, false); err != nil {
		return errStatus, err
	}
	return &v1alpha1.InstallStatus{Status: v1alpha1.InstallStatus_HEALTHY}, nil
}

// DeleteControlPlaneByRevision removed resources that are in the slice of UnstructuredList and match with specific control plane revision.
func (h *HelmReconciler) DeleteControlPlaneByRevision(revision string, objectsList []*unstructured.UnstructuredList, purge bool) error {
	labels := map[string]string{}
	if !purge {
		labels = map[string]string{
			label.IstioRev: revision,
		}
	}
	for _, objects := range objectsList {
		if !purge {
			if err := h.deleteResources(nil, labels, string(name.PilotComponentName), objects, false); err != nil {
				return fmt.Errorf("failed to prune resources: %v", err)
			}
		} else {
			if err := h.deleteResources(nil, labels, "", objects, true); err != nil {
				return fmt.Errorf("failed to prune resources: %v", err)
			}
		}
	}
	return nil
}

// GetPrunedResourcesByRevision get the list of resources to be removed when we prune by revision.
func (h *HelmReconciler) GetPrunedResourcesByRevision(revision string, purge bool) ([]*unstructured.UnstructuredList, []string, error) {
	var resources []string
	var usList []*unstructured.UnstructuredList
	labels := map[string]string{
		label.IstioRev: revision,
	}
	selector := klabels.Set(labels).AsSelectorPreValidated()
	gvkList := AllClusterResources
	if !purge {
		gvkList = append(NamespacedResources, NonNamespacedCPResources...)
	}
	for _, gvk := range gvkList {
		objects := &unstructured.UnstructuredList{}
		objects.SetGroupVersionKind(gvk)
		componentRequirement, err := klabels.NewRequirement(IstioComponentLabelStr, selection.Exists, nil)
		if err != nil {
			return usList, resources, err
		}
		selector = selector.Add(*componentRequirement)
		if err := h.client.List(context.TODO(), objects, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			// we only want to retrieve resources clusters
			scope.Warnf("retrieving resources to prune type %s: %s not found", gvk.String(), err)
			continue
		}
		usList = append(usList, objects)
		for _, o := range objects.Items {
			resources = append(resources, object.NewK8sObject(&o, nil, nil).Hash())
		}
	}
	return usList, resources, nil
}

// DeleteControlPlaneByManifests removed resources by manifests and revision.
func (h *HelmReconciler) DeleteControlPlaneByManifests(manifestMap name.ManifestMap, revision string, purge bool) error {
	labels := map[string]string{}
	manifests := manifestMap.String()
	// if not purge, we should only remove the control plane resources with matching label
	if !purge {
		labels = map[string]string{
			label.IstioRev:   revision,
			operatorLabelStr: operatorReconcileStr,
		}
		manifests = strings.Join(manifestMap[name.PilotComponentName], "---")
	}
	objects, err := object.ParseK8sObjectsFromYAMLManifest(manifests)
	if err != nil {
		return fmt.Errorf("failed parse k8s objects from yaml: %v", err)
	}
	unstructuredObjects := unstructured.UnstructuredList{}
	for _, obj := range objects {
		obju := obj.UnstructuredObject()
		if err := util.SetLabel(obju, operatorLabelStr, operatorReconcileStr); err != nil {
			return fmt.Errorf("failed to apply labels: %v", err)
		}
		if err := util.SetLabel(obju, IstioComponentLabelStr, string(name.PilotComponentName)); err != nil {
			return fmt.Errorf("failed to apply labels: %v", err)
		}
		unstructuredObjects.Items = append(unstructuredObjects.Items, *obju)
	}
	if !purge {
		if err := h.deleteResources(nil, labels, string(name.PilotComponentName), &unstructuredObjects, false); err != nil {
			return fmt.Errorf("failed to prune control plane resources: %v", err)
		}
		return nil
	}
	if err := h.deleteResources(nil, labels, "", &unstructuredObjects, true); err != nil {
		return fmt.Errorf("failed to prune control plane resources: %v", err)
	}
	return nil
}

// pruneAllTypes will collect all existing resource types we care about. For each type, the callback function
// will be called with the labels used to select this type, and all objects.
// This is in internal function meant to support prune and delete
func (h *HelmReconciler) runForAllTypes(callback func(labels map[string]string, objects *unstructured.UnstructuredList) error) error {
	var errs util.Errors
	// Ultimately, we want to prune based on component labels. Each of these share a common set of labels
	// Rather than do N List() calls for each component, we will just filter for the common subset here
	// and each component will do its own filtering
	// Because we are filtering by the core labels, List() will only return items that some components will care
	// about, so we are not querying for an overly broad set of resources.
	labels, err := h.getCoreOwnerLabels()
	if err != nil {
		return err
	}
	selector := klabels.Set(labels).AsSelectorPreValidated()
	for _, gvk := range append(NamespacedResources, ClusterResources...) {
		// First, we collect all objects for the provided GVK
		objects := &unstructured.UnstructuredList{}
		objects.SetGroupVersionKind(gvk)
		componentRequirement, err := klabels.NewRequirement(IstioComponentLabelStr, selection.Exists, nil)
		if err != nil {
			return err
		}
		selector = selector.Add(*componentRequirement)
		if err := h.client.List(context.TODO(), objects, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			// we only want to retrieve resources clusters
			scope.Warnf("retrieving resources to prune type %s: %s not found", gvk.String(), err)
			continue
		}

		errs = util.AppendErr(errs, callback(labels, objects))
	}
	return errs.ToError()
}

// DeleteComponent Delete removes all resources associated with componentName.
func (h *HelmReconciler) DeleteComponent(componentName string) error {
	return h.runForAllTypes(func(labels map[string]string, objects *unstructured.UnstructuredList) error {
		return h.deleteResources(map[string]bool{}, labels, componentName, objects, false)
	})
}

// deleteResources delete any resources from the given component that are not in the excluded map. Resource
// labels are used to identify the resources belonging to the component.
func (h *HelmReconciler) deleteResources(excluded map[string]bool, coreLabels map[string]string,
	componentName string, objects *unstructured.UnstructuredList, all bool) error {
	var errs util.Errors
	labels := h.addComponentLabels(coreLabels, componentName)
	selector := klabels.Set(labels).AsSelectorPreValidated()
	for _, o := range objects.Items {
		obj := object.NewK8sObject(&o, nil, nil)
		oh := obj.Hash()
		if !all {
			// Label mismatch. Provided objects don't select against the component, so this likely means the object
			// is for another component.
			if !selector.Matches(klabels.Set(o.GetLabels())) {
				continue
			}
			if excluded[oh] {
				continue
			}
		}
		if h.opts.DryRun {
			h.opts.Log.LogAndPrintf("Not pruning object %s because of dry run.", oh)
			continue
		}

		err := h.client.Delete(context.TODO(), &o, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil {
			if !strings.Contains(err.Error(), "not found") {
				errs = util.AppendErr(errs, err)
			} else {
				// do not return error if resources are not found
				h.opts.Log.LogAndPrintf("object: %s is not being deleted: %v", obj.Hash(), err)
			}
		}
		if !all {
			h.removeFromObjectCache(componentName, oh)
		}
		h.opts.Log.LogAndPrintf("  Removed %s.", oh)
	}
	if all {
		cache.FlushObjectCaches()
	}

	return errs.ToError()
}

// RemoveObject removes object with objHash in componentName from the object cache.
func (h *HelmReconciler) removeFromObjectCache(componentName, objHash string) {
	crHash, err := h.getCRHash(componentName)
	if err != nil {
		scope.Error(err.Error())
	}
	cache.RemoveObject(crHash, objHash)
	scope.Infof("Removed object %s from Cache.", objHash)
}
