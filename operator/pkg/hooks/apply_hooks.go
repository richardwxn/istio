package hooks

import (
	"istio.io/api/operator/v1alpha1"
	"istio.io/istio/operator/pkg/manifest"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/util"
)

// applyhook is a callout function that may be called during manifest apply to check state or modify the cluster.
// applyhook should only be used for version-specific actions.
type applyhook func(kubeClient manifest.ExecClient, params ApplyHookCommonParams) util.Errors
type applys []applyhook


// hookVersionMapping is a mapping between a hashicorp/go-version formatted constraints for the source and target
// versions and the list of hooks that should be run if the constraints match.
type hookVersionMapping struct {
	sourceVersionConstraint string
	targetVersionConstraint string
	hooks                   hooks
}

// HookCommonParams is a set of common params passed to all hooks.
type ApplyHookCommonParams struct {

}

var (
	// preUpgradeHooks is a list of hook version constraint pairs mapping to a slide of corresponding hooks to run
	// before upgrade.
	preUpgradeHooks = []hookVersionMapping{
		{
			sourceVersionConstraint: ">=1.3",
			targetVersionConstraint: ">=1.3",
			hooks:                   []hook{checkInitCrdJobs, checkMixerTelemetry},
		},
	}
	// postUpgradeHooks is a list of hook version constraint pairs mapping to a slide of corresponding hooks to run
	// before upgrade.
	postUpgradeHooks []hookVersionMapping
)

func RunPreUpgradeHooks(kubeClient manifest.ExecClient, hc *UpgradeHookCommonParams, dryRun bool) util.Errors {
	return runUpgradeHooks(preUpgradeHooks, kubeClient, hc, dryRun)
}

func RunPostUpgradeHooks(kubeClient manifest.ExecClient, hc *UpgradeHookCommonParams, dryRun bool) util.Errors {
	return runUpgradeHooks(postUpgradeHooks, kubeClient, hc, dryRun)
}