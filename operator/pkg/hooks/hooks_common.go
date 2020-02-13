package hooks

import (
	"fmt"

	"github.com/ghodss/yaml"

  "istio.io/istio/operator/pkg/manifest"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/util"
)

var (
	// TODO: add full list
	CRKindNamesMap = map[string][]string{
		"instance": {"requestsize", "requestcount, requestduration", "attributes"},
		"rule":     {"promhttp", "kubeattrgenrulerule"},
		"handler":  {"prometheus", "kubernetesenv"},
	}

	KindResourceMap = map[string]string{
		"instance": "instances",
		"rule":     "rules",
		"handler":  "handlers",
	}
)

// checkMixerTelemetry compares default mixer telemetry configs with corresponding in-cluster configs
// consider these cases with difference:
// 1. new in-cluster CR which does not exists in default configs
// 2. same CR but with difference in fields
// 3. remove CR from default configs(upgrade can proceed for this case)
func checkMixerTelemetry(kubeClient manifest.ExecClient, params HookCommonParams) util.Errors {
	knMapDefault, err := extractTargetKNMapFromDefault(params.DefaultTelemetryManifest)
	if err != nil {
		return util.NewErrs(err)
	}
	knMapCluster, err := extractKNMapFromCluster(kubeClient)
	if err != nil {
		return util.NewErrs(err)
	}

	for nk, inclusterCR := range knMapCluster {
		defaultCR, ok := knMapDefault[nk]
		if !ok {
			// for case 1
			return util.NewErrs(fmt.Errorf("there are extra mixer configs in cluster"))
		}
		// for case 2
		diff := util.YAMLDiff(defaultCR, inclusterCR)
		if diff != "" {
			return util.NewErrs(fmt.Errorf("customized config exists for %s,"+
					" diff is: %s. please check existing mixer config first before upgrade", nk, diff))
		}
	}
	return nil
}

func extractTargetKNMapFromDefault(nkMap map[string]*object.K8sObject) (map[string]string, error) {
	checkMap := make(map[string]string)
	for kind, names := range CRKindNamesMap {
		for _, name := range names {
			knKey := kind + ":" + name
			msObject, ok := nkMap[knKey]
			if !ok {
				continue
			}
			item := msObject.GetObject()
			spec, ok := item.UnstructuredContent()["spec"].(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("failed to get spec from unstructured item"+
						" of kind: %s, name: %s", kind, name)
			}
			specYAML, err := yaml.Marshal(spec)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal spec of kind: %s, name: %s", kind, name)
			}
			checkMap[knKey] = string(specYAML)
		}
	}
	return checkMap, nil
}

func extractKNMapFromCluster(kubeClient manifest.ExecClient) (map[string]string, error) {
	knYAMLMap := make(map[string]string)
	for kind := range CRKindNamesMap {
		uls, err := kubeClient.GetGroupVersionResource(configAPIGroup, configAPIVersion, KindResourceMap[kind], istioNamespace, "")
		if err != nil {
			return nil, err
		}
		for _, item := range uls.Items {
			meta, _ := item.UnstructuredContent()["metadata"].(map[string]interface{})
			name, _ := meta["name"].(string)
			spec, ok := item.UnstructuredContent()["spec"].(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("failed to get spec from unstructured item"+
						" of kind: %s, name: %s", kind, name)
			}
			specYAML, err := yaml.Marshal(spec)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal spec of kind: %s, name: %s", kind, name)
			}
			key := kind + ":" + name
			knYAMLMap[key] = string(specYAML)
		}
	}
	return knYAMLMap, nil
}
