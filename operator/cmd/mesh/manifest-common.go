// Copyright 2019 Istio Authors
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

package mesh

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"k8s.io/client-go/rest"

	"istio.io/api/operator/v1alpha1"
	"istio.io/istio/operator/pkg/component/controlplane"
	"istio.io/istio/operator/pkg/helm"
	"istio.io/istio/operator/pkg/kubectlcmd"
	"istio.io/istio/operator/pkg/manifest"
	"istio.io/istio/operator/pkg/name"
	"istio.io/istio/operator/pkg/tpath"
	"istio.io/istio/operator/pkg/translate"
	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/operator/pkg/validate"
	"istio.io/istio/operator/version"
)

var (
	ignoreStdErrList = []string{
		// TODO: remove when https://github.com/kubernetes/kubernetes/issues/82154 is fixed.
		"Warning: kubectl apply should be used on resource created by either kubectl create --save-config or kubectl apply",
	}
)

func genApplyManifests(setOverlay []string, inFilename []string, force bool, dryRun bool, verbose bool,
	kubeConfigPath string, context string, wait bool, waitTimeout time.Duration, l *Logger) error {
	overlayFromSet, err := MakeTreeFromSetList(setOverlay, force, l)
	if err != nil {
		return fmt.Errorf("failed to generate tree from the set overlay, error: %v", err)
	}

	opts := &kubectlcmd.Options{
		DryRun:      dryRun,
		Verbose:     verbose,
		Wait:        wait,
		WaitTimeout: waitTimeout,
		Kubeconfig:  kubeConfigPath,
		Context:     context,
	}

	kubeconfig, err := manifest.InitK8SRestClient(opts.Kubeconfig, opts.Context)
	if err != nil {
		return err
	}

	manifests, iops, err := GenManifests(inFilename, overlayFromSet, force, kubeconfig, l)
	if err != nil {
		return fmt.Errorf("failed to generate manifest: %v", err)
	}

	for _, cn := range name.DeprecatedNames {
		DeprecatedComponentManifest := fmt.Sprintf("# %s component has been deprecated.\n", cn)
		manifests[cn] = append(manifests[cn], DeprecatedComponentManifest)
	}

	out, err := manifest.ApplyAll(manifests, version.OperatorBinaryVersion, opts)
	if err != nil {
		return fmt.Errorf("failed to apply manifest with kubectl client: %v", err)
	}
	gotError := false
	skippedComponentMap := map[name.ComponentName]bool{}
	for cn := range manifests {
		enabledInSpec, err := name.IsComponentEnabledInSpec(cn, iops)
		if err != nil {
			l.logAndPrintf("failed to check if %s is enabled in IstioOperatorSpec: %v", cn, err)
		}
		// Skip the output of a component when it is disabled
		// and not pruned (indicated by applied manifest out[cn].Manifest).
		if !enabledInSpec && out[cn].Err == nil && out[cn].Manifest == "" {
			skippedComponentMap[cn] = true
		}
	}

	for cn := range manifests {
		if out[cn].Err != nil {
			cs := fmt.Sprintf("Component %s - manifest apply returned the following errors:", cn)
			l.logAndPrintf("\n%s", cs)
			l.logAndPrint("Error: ", out[cn].Err, "\n")
			gotError = true
		} else if skippedComponentMap[cn] {
			continue
		}

		if !ignoreError(out[cn].Stderr) {
			l.logAndPrint("Error detail:\n", out[cn].Stderr, "\n", out[cn].Stdout, "\n")
			gotError = true
		}
	}

	if gotError {
		l.logAndPrint("\n\n✘ Errors were logged during apply operation. Please check component installation logs above.\n")
		return fmt.Errorf("errors were logged during apply operation")
	}

	l.logAndPrint("\n\n✔ Installation complete\n")
	return nil
}

// GenManifests generate manifest from input file and setOverLay
func GenManifests(inFilename []string, setOverlayYAML string, force bool,
	kubeConfig *rest.Config, l *Logger) (name.ManifestMap, *v1alpha1.IstioOperatorSpec, error) {
	mergedYAML, err := genProfile(false, inFilename, "", setOverlayYAML, "", force, kubeConfig, l)
	if err != nil {
		return nil, nil, err
	}
	mergedIOPS, err := unmarshalAndValidateIOPS(mergedYAML, force, l)
	if err != nil {
		return nil, nil, err
	}

	t, err := translate.NewTranslator(version.OperatorBinaryVersion.MinorVersion)
	if err != nil {
		return nil, nil, err
	}

	if err := fetchInstallPackageFromURL(mergedIOPS); err != nil {
		return nil, nil, err
	}

	cp, err := controlplane.NewIstioOperator(mergedIOPS, t)
	if err != nil {
		return nil, nil, err
	}
	if err := cp.Run(); err != nil {
		return nil, nil, fmt.Errorf("failed to create Istio control plane with spec: \n%v\nerror: %s", mergedIOPS, err)
	}

	manifests, errs := cp.RenderManifest()
	if errs != nil {
		return manifests, mergedIOPS, errs.ToError()
	}
	return manifests, mergedIOPS, nil
}

func ignoreError(stderr string) bool {
	trimmedStdErr := strings.TrimSpace(stderr)
	for _, ignore := range ignoreStdErrList {
		if strings.HasPrefix(trimmedStdErr, ignore) {
			return true
		}
	}
	return trimmedStdErr == ""
}

// fetchInstallPackageFromURL downloads installation packages from specified URL.
func fetchInstallPackageFromURL(mergedIOPS *v1alpha1.IstioOperatorSpec) error {
	if util.IsHTTPURL(mergedIOPS.InstallPackagePath) {
		pkgPath, err := fetchInstallPackage(mergedIOPS.InstallPackagePath)
		if err != nil {
			return err
		}
		// TODO: replace with more robust logic to set local file path
		mergedIOPS.InstallPackagePath = filepath.Join(pkgPath, helm.ChartsFilePath)
	}
	return nil
}

// fetchInstallPackage downloads installation packages from the given url.
func fetchInstallPackage(url string) (string, error) {
	uf, err := helm.NewURLFetcher(url, "")
	if err != nil {
		return "", err
	}
	if err := uf.FetchBundles().ToError(); err != nil {
		return "", err
	}
	isp := path.Base(url)
	// get rid of the suffix, installation package is untared to folder name istio-{version}, e.g. istio-1.3.0
	idx := strings.LastIndex(isp, "-")
	return filepath.Join(uf.DestDir(), isp[:idx]), nil
}

// MakeTreeFromSetList creates a YAML tree from a string slice containing key-value pairs in the format key=value.
func MakeTreeFromSetList(setOverlay []string, force bool, l *Logger) (string, error) {
	if len(setOverlay) == 0 {
		return "", nil
	}
	tree := make(map[string]interface{})
	for _, kv := range setOverlay {
		kvv := strings.Split(kv, "=")
		if len(kvv) != 2 {
			return "", fmt.Errorf("bad argument %s: expect format key=value", kv)
		}
		k := kvv[0]
		v := util.ParseValue(kvv[1])
		if err := tpath.WriteNode(tree, util.PathFromString(k), v); err != nil {
			return "", err
		}
		// To make errors more user friendly, test the path and error out immediately if we cannot unmarshal.
		testTree, err := yaml.Marshal(tree)
		if err != nil {
			return "", err
		}
		iops := &v1alpha1.IstioOperatorSpec{}
		if err := util.UnmarshalWithJSONPB(string(testTree), iops, false); err != nil {
			return "", fmt.Errorf("bad path=value: %s", kv)
		}
		if errs := validate.CheckIstioOperatorSpec(iops, true); len(errs) != 0 {
			if !force {
				l.logAndError("Run the command with the --force flag if you want to ignore the validation error and proceed.")
				return "", fmt.Errorf("bad path=value (%s): %s", kv, errs)
			}
		}

	}
	out, err := yaml.Marshal(tree)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
