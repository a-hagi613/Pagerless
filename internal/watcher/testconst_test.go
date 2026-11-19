package watcher

// Values the watcher tests share. A kind, namespace, or name that identifies the
// same object in several tests is written once, so a test cannot drift from
// another by a typo and the suite stays readable.
const (
	testNamespace        = "shop"
	testOtherNamespace   = "payments"
	testReleaseNamespace = "pagerless"

	testKindPod         = "Pod"
	testKindDeployment  = "Deployment"
	testKindStatefulSet = "StatefulSet"
	testKindCronJob     = "CronJob"
	testKindNode        = "Node"

	testDeployment      = "checkout"
	testOtherDeployment = "cart"
	testPodName         = "checkout-abc"
	testNodeName        = "node-1"
	testEventReason     = "BackOff"
	testScope           = "app"

	testDeploymentUID = "d-1"
	testRecreatedUID  = "d-2"
	testPodUID        = "p-1"
)
