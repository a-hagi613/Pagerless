package proposal_test

// Values shared by the Proposal tests, so that a fixture change is made once.
const (
	checkoutName   = "checkout"
	pageNamespace  = "pagerless"
	deploymentPath = "apps/checkout/deployment.yaml"
	overlayPath    = "apps/checkout/overlays/prod/replicas.yaml"
	patchPath      = "apps/checkout/overlays/prod/patch.yaml"
)
