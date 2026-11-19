package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/cli"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// Names of the Targets and namespaces that these tests use.
const (
	sandbox  = "sandbox"
	checkout = "checkout"
	cart     = "cart"
	ledger   = "ledger"
	vault    = "vault"

	// labelKey labels the pods of the Targets that these tests create.
	labelKey      = "app"
	containerName = "app"

	// newCommand is the verb that opens a Page.
	newCommand = "new"
)

var (
	logFinding = v1alpha1.Finding{
		Kind:    v1alpha1.FindingKindLog,
		Source:  `loki: {namespace="sandbox",app="checkout"}`,
		Summary: "the container was killed with exit code 137 shortly after start",
	}
	createdPattern = regexp.MustCompile(`page\.pagerless\.dev/(\S+) created`)
)

func TestStartTargetsTheDeploymentThatOwnsAPod(t *testing.T) {
	env, kubeconfig := startCluster(t)
	deployment := createDeployment(t, env, checkout)
	replicaSet := createReplicaSet(t, env, deployment, "checkout-7d9f")
	createPod(t, env, "checkout-7d9f-x2x4q", ownedBy(replicaSet, "ReplicaSet"))

	out, err := execute(t.Context(), kubeconfig,
		"new", "-n", sandbox, "pod/checkout-7d9f-x2x4q", "--hint", "started after the last release")
	if err != nil {
		t.Fatalf("start a Page for a pod: %v", err)
	}

	tr := onlyPage(t, env)
	env.WaitForPhase(t, tr.Name, v1alpha1.PhaseInvestigating)
	want := v1alpha1.TargetReference{
		APIVersion: "apps/v1", Kind: v1alpha1.TargetKindDeployment, Namespace: sandbox, Name: checkout,
	}
	if tr.Spec.Target.Kind != want.Kind || tr.Spec.Target.Namespace != want.Namespace ||
		tr.Spec.Target.Name != want.Name || tr.Spec.Target.APIVersion != want.APIVersion ||
		tr.Spec.Target.UID == "" {
		t.Errorf("target = %+v, want %+v with a UID", tr.Spec.Target, want)
	}
	if tr.Spec.Fingerprint != v1alpha1.Fingerprint(tr.Spec.Target, "ManualRequest") {
		t.Errorf("fingerprint = %q, want the canonical identity of the Target", tr.Spec.Fingerprint)
	}
	if want := (v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI, Pod: "checkout-7d9f-x2x4q"}); tr.Spec.Trigger != want {
		t.Errorf("trigger = %+v, want %+v", tr.Spec.Trigger, want)
	}
	if tr.Spec.Hint != "started after the last release" {
		t.Errorf("hint = %q", tr.Spec.Hint)
	}
	if want := "page.pagerless.dev/" + tr.Name + " created\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestStartResolvesEachTargetToItsTarget(t *testing.T) {
	env, kubeconfig := startCluster(t)
	createDeployment(t, env, checkout)
	createStatefulSet(t, env, ledger)
	vaultSet := createStatefulSet(t, env, vault)
	createPod(t, env, vault+"-0", ownedBy(vaultSet, "StatefulSet"))

	cases := []struct {
		name    string
		args    []string
		want    v1alpha1.TargetReference
		wantPod string
	}{
		{
			name: "deploy names the Deployment directly",
			args: []string{"deploy/" + checkout},
			want: v1alpha1.TargetReference{Kind: v1alpha1.TargetKindDeployment, Namespace: sandbox, Name: checkout},
		},
		{
			name: "sts names the StatefulSet directly",
			args: []string{"sts/" + ledger},
			want: v1alpha1.TargetReference{Kind: v1alpha1.TargetKindStatefulSet, Namespace: sandbox, Name: ledger},
		},
		{
			name:    "pod of a StatefulSet resolves to it",
			args:    []string{"pod/" + vault + "-0"},
			want:    v1alpha1.TargetReference{Kind: v1alpha1.TargetKindStatefulSet, Namespace: sandbox, Name: vault},
			wantPod: vault + "-0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{newCommand, "-n", sandbox}, tc.args...)
			if _, err := execute(t.Context(), kubeconfig, args...); err != nil {
				t.Fatalf("start: %v", err)
			}
			tr := pageFor(t, env, tc.want)
			env.WaitForPhase(t, tr.Name, v1alpha1.PhaseInvestigating)
			if tr.Spec.Trigger.Pod != tc.wantPod {
				t.Errorf("trigger pod = %q, want %q", tr.Spec.Trigger.Pod, tc.wantPod)
			}
		})
	}
}

func TestStartRefusesATargetWithoutATarget(t *testing.T) {
	env, kubeconfig := startCluster(t)
	createPod(t, env, "orphan", nil)

	cases := []struct {
		name string
		arg  string
		want string
	}{
		{"missing Deployment", "deploy/missing", `deployment "missing" not found in namespace "sandbox"`},
		{"pod without an owner", "pod/orphan", "pod sandbox/orphan has no owner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execute(t.Context(), kubeconfig, newCommand, "-n", sandbox, tc.arg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("start %s: error = %v, want it to contain %q", tc.arg, err, tc.want)
			}
		})
	}
	if n := len(pagesIn(t, env)); n != 0 {
		t.Errorf("%d Pages were created for targets without a Target, want none", n)
	}
}

func TestStartPrintsTheNameOfTheActivePage(t *testing.T) {
	env, kubeconfig := startCluster(t)
	createDeployment(t, env, checkout)

	first, err := execute(t.Context(), kubeconfig, newCommand, "-n", sandbox, "deploy/"+checkout)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	name := createdName(t, first)
	env.WaitForPhase(t, name, v1alpha1.PhaseInvestigating)

	second, err := execute(t.Context(), kubeconfig, newCommand, "-n", sandbox, "deploy/"+checkout)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if want := "page.pagerless.dev/" + name + " already active\n"; second != want {
		t.Errorf("second start printed %q, want %q", second, want)
	}
	if n := len(pagesIn(t, env)); n != 1 {
		t.Errorf("%d Pages exist after two starts for one Target, want 1", n)
	}
}

func TestListShowsEachPageInEveryFormat(t *testing.T) {
	env, kubeconfig := startCluster(t)
	createDeployment(t, env, checkout)
	createDeployment(t, env, cart)
	names := map[string]v1alpha1.TargetReference{}
	for _, target := range []string{checkout, cart} {
		out, err := execute(t.Context(), kubeconfig, newCommand, "-n", sandbox, "deploy/"+target)
		if err != nil {
			t.Fatalf("start %s: %v", target, err)
		}
		name := createdName(t, out)
		env.WaitForPhase(t, name, v1alpha1.PhaseInvestigating)
		names[name] = v1alpha1.TargetReference{Kind: v1alpha1.TargetKindDeployment, Namespace: sandbox, Name: target}
	}

	table, err := execute(t.Context(), kubeconfig, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	header := strings.Join(strings.Fields(strings.SplitN(table, "\n", 2)[0]), " ")
	if want := "NAME TARGET PHASE TRIGGER PR AGE"; header != want {
		t.Errorf("table header = %q, want %q", header, want)
	}
	for name, target := range names {
		row := lineContaining(table, name)
		for _, want := range []string{target.Name, "Investigating", "CLI", "-"} {
			if !strings.Contains(row, want) {
				t.Errorf("row %q lacks %q", row, want)
			}
		}
	}

	jsonOut, err := execute(t.Context(), kubeconfig, "list", "-o", "json")
	if err != nil {
		t.Fatalf("list -o json: %v", err)
	}
	var fromJSON v1alpha1.PageList
	if err := json.Unmarshal([]byte(jsonOut), &fromJSON); err != nil {
		t.Fatalf("decode list -o json: %v", err)
	}
	checkListed(t, fromJSON.Items, names)

	yamlOut, err := execute(t.Context(), kubeconfig, "list", "-o", "yaml")
	if err != nil {
		t.Fatalf("list -o yaml: %v", err)
	}
	var fromYAML v1alpha1.PageList
	if err := yaml.Unmarshal([]byte(yamlOut), &fromYAML); err != nil {
		t.Fatalf("decode list -o yaml: %v", err)
	}
	checkListed(t, fromYAML.Items, names)
}

func TestShowPresentsTheReportedDiagnosisAndProposal(t *testing.T) {
	env, kubeconfig := startCluster(t)
	createDeployment(t, env, checkout)
	out, err := execute(t.Context(), kubeconfig, newCommand, "-n", sandbox, "deploy/"+checkout)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	name := createdName(t, out)
	agent := env.StartAgent(t, name)
	report := noActionReport()
	if _, err := agent.API.PostReport(t.Context(), report); err != nil {
		t.Fatalf("post report: %v", err)
	}
	env.WaitForPhase(t, name, v1alpha1.PhaseProposed)

	shown, err := execute(t.Context(), kubeconfig, "show", name)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{
		"Name:       " + name,
		"Phase:      Proposed",
		"Target:     Deployment/sandbox/checkout",
		report.Diagnosis,
		logFinding.Source,
		logFinding.Summary,
		"Action:     NoAction",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("show output lacks %q:\n%s", want, shown)
		}
	}

	shownJSON, err := execute(t.Context(), kubeconfig, "show", name, "-o", "json")
	if err != nil {
		t.Fatalf("show -o json: %v", err)
	}
	var fromJSON v1alpha1.Page
	if err := json.Unmarshal([]byte(shownJSON), &fromJSON); err != nil {
		t.Fatalf("decode show -o json: %v", err)
	}
	if fromJSON.Name != name || fromJSON.Status.Phase != v1alpha1.PhaseProposed ||
		fromJSON.Status.Proposal == nil || fromJSON.Status.Proposal.Action != v1alpha1.ActionNoAction {
		t.Errorf("show -o json = %+v, want the Proposed Page %s", fromJSON.Status, name)
	}

	shownYAML, err := execute(t.Context(), kubeconfig, "show", name, "-o", "yaml")
	if err != nil {
		t.Fatalf("show -o yaml: %v", err)
	}
	var fromYAML v1alpha1.Page
	if err := yaml.Unmarshal([]byte(shownYAML), &fromYAML); err != nil {
		t.Fatalf("decode show -o yaml: %v", err)
	}
	if fromYAML.Status.Diagnosis != report.Diagnosis {
		t.Errorf("show -o yaml diagnosis = %q, want %q", fromYAML.Status.Diagnosis, report.Diagnosis)
	}
}

func TestShowWatchFollowsTheStepsUntilTheProposal(t *testing.T) {
	env, kubeconfig := startCluster(t)
	createDeployment(t, env, checkout)
	out, err := execute(t.Context(), kubeconfig, newCommand, "-n", sandbox, "deploy/"+checkout)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	name := createdName(t, out)
	agent := env.StartAgent(t, name)

	type outcome struct {
		out string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		watched, err := execute(t.Context(), kubeconfig, "show", name, "--watch")
		done <- outcome{watched, err}
	}()
	events := []agentapi.StepEvent{
		{Kind: v1alpha1.StepKindToolCall, Tool: "kubernetes.pods_log", Summary: "read the last 20 log lines"},
		{Kind: v1alpha1.StepKindFinding, Summary: "recorded the exit code", Finding: &logFinding},
	}
	for _, event := range events {
		if result, err := agent.API.PostEvent(t.Context(), event); err != nil || result.StatusCode != http.StatusAccepted {
			t.Fatalf("post step %q: status %d, error %v", event.Summary, result.StatusCode, err)
		}
	}
	if _, err := agent.API.PostReport(t.Context(), noActionReport()); err != nil {
		t.Fatalf("post report: %v", err)
	}

	var result outcome
	select {
	case result = <-done:
	case <-time.After(time.Minute):
		t.Fatal("show --watch did not return after the Page was Proposed")
	}
	if result.err != nil {
		t.Fatalf("show --watch: %v", result.err)
	}
	watched := result.out
	first := strings.Index(watched, "ToolCall kubernetes.pods_log: read the last 20 log lines")
	second := strings.Index(watched, "Finding: recorded the exit code")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("show --watch printed the steps out of order or not at all:\n%s", watched)
	}
	phase := strings.Index(watched, "phase: Proposed")
	presentation := strings.Index(watched, "\nName:")
	if phase < second || presentation < phase {
		t.Errorf("show --watch printed the Proposed phase and the presentation in the wrong place:\n%s", watched)
	}
	if !strings.Contains(watched, "Action:     NoAction") {
		t.Errorf("show --watch lacks the Proposal:\n%s", watched)
	}
}

func TestShowRefusesAnUnknownPage(t *testing.T) {
	_, kubeconfig := startCluster(t)
	_, err := execute(t.Context(), kubeconfig, "show", "missing")
	if err == nil || !strings.Contains(err.Error(), `no Page named "missing" in namespace "pagerless"`) {
		t.Fatalf("show missing: error = %v", err)
	}
}

// TestShowDisplaysTheNarrative writes the status that Verification and the
// Narrative Job leave behind, as the server does, and checks how the CLI
// presents it. The server's own write is covered by the envtest seam.
func TestShowDisplaysTheNarrative(t *testing.T) {
	env, kubeconfig := startCluster(t)
	createDeployment(t, env, checkout)
	out, err := execute(t.Context(), kubeconfig, newCommand, "-n", sandbox, "deploy/"+checkout)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	name := createdName(t, out)
	env.WaitForPhase(t, name, v1alpha1.PhaseInvestigating)

	narrative := "The checkout Deployment recovered after the approved memory change."
	writeServerStatus(t, env, name, func(tr *v1alpha1.Page) {
		tr.Status.Phase = v1alpha1.PhaseRecovered
		tr.Status.Diagnosis = "The container exceeded its 128Mi memory limit."
		tr.Status.Narrative = narrative
	})

	shown, err := execute(t.Context(), kubeconfig, "show", name)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(shown, "Narrative\n  "+narrative) {
		t.Errorf("show output lacks the Narrative:\n%s", shown)
	}

	shownJSON, err := execute(t.Context(), kubeconfig, "show", name, "-o", "json")
	if err != nil {
		t.Fatalf("show -o json: %v", err)
	}
	var fromJSON v1alpha1.Page
	if err := json.Unmarshal([]byte(shownJSON), &fromJSON); err != nil {
		t.Fatalf("decode show -o json: %v", err)
	}
	if fromJSON.Status.Narrative != narrative {
		t.Errorf("show -o json narrative = %q, want %q", fromJSON.Status.Narrative, narrative)
	}

	shownYAML, err := execute(t.Context(), kubeconfig, "show", name, "-o", "yaml")
	if err != nil {
		t.Fatalf("show -o yaml: %v", err)
	}
	var fromYAML v1alpha1.Page
	if err := yaml.Unmarshal([]byte(shownYAML), &fromYAML); err != nil {
		t.Fatalf("decode show -o yaml: %v", err)
	}
	if fromYAML.Status.Narrative != narrative {
		t.Errorf("show -o yaml narrative = %q, want %q", fromYAML.Status.Narrative, narrative)
	}
}

func TestCommandLineRefusesInvalidArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown output format", []string{"list", "-o", "xml"}, "unsupported output format"},
		{"watch with machine output", []string{"show", "checkout", "--watch", "-o", "json"}, "--watch"},
		{"resource without a Target kind", []string{"new", "pods/checkout"}, "unknown Target kind"},
		{"bare Target name", []string{"new", "checkout"}, "must name its kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := execute(t.Context(), "", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// execute runs kubectl-page with the arguments against the cluster that the
// kubeconfig names, and returns what it printed to standard output.
func execute(ctx context.Context, kubeconfig string, args ...string) (string, error) {
	root := cli.NewRootCommand()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"--kubeconfig", kubeconfig}, args...))
	err := root.ExecuteContext(ctx)
	return stdout.String(), err
}

// startCluster starts the harness and creates the sandbox namespace, in which the Targets of these tests run.
// The tests that call it run one after another. Each start runs a control plane, and when several packages start
// control planes at once, the API servers time out while they start.
func startCluster(t testing.TB) (*testenv.Env, string) {
	t.Helper()
	env := testenv.Start(t)
	return env, kubeconfigFor(t, env.Config)
}

// writeServerStatus changes a Page's status with the server's identity, the
// only one that the status guard admits. It arranges a state that the server
// would have written.
func writeServerStatus(t testing.TB, env *testenv.Env, name string, mutate func(*v1alpha1.Page)) {
	t.Helper()
	scheme, err := server.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(env.ServerConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create a client with the server's identity: %v", err)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var tr v1alpha1.Page
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: testenv.Namespace, Name: name}, &tr); err != nil {
			return err
		}
		mutate(&tr)
		return c.Status().Update(t.Context(), &tr)
	})
	if err != nil {
		t.Fatalf("write the status of Page %s: %v", name, err)
	}
}

// kubeconfigFor writes a kubeconfig that reaches the harness API server as its admin user.
func kubeconfigFor(t testing.TB, cfg *rest.Config) string {
	t.Helper()
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["envtest"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kubeconfig.AuthInfos["admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	kubeconfig.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "admin"}
	kubeconfig.CurrentContext = "envtest"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeconfig, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func mustCreate(t testing.TB, env *testenv.Env, obj client.Object) {
	t.Helper()
	if err := env.Client.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func selectorFor(name string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{labelKey: name}}
}

func podTemplate(name string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Labels: map[string]string{labelKey: name},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: containerName, Image: "registry.invalid/" + name + ":1"}},
		},
	}
}

func createDeployment(t testing.TB, env *testenv.Env, name string) *appsv1.Deployment {
	t.Helper()
	deployment := &appsv1.Deployment{
		Name:      name,
		Namespace: sandbox,
		Spec:      appsv1.DeploymentSpec{Selector: selectorFor(name), Template: podTemplate(name)},
	}
	mustCreate(t, env, deployment)
	return deployment
}

func createStatefulSet(t testing.TB, env *testenv.Env, name string) *appsv1.StatefulSet {
	t.Helper()
	statefulSet := &appsv1.StatefulSet{
		Name:      name,
		Namespace: sandbox,
		Spec: appsv1.StatefulSetSpec{
			Selector:    selectorFor(name),
			Template:    podTemplate(name),
			ServiceName: name,
		},
	}
	mustCreate(t, env, statefulSet)
	return statefulSet
}

func createReplicaSet(t testing.TB, env *testenv.Env, deployment *appsv1.Deployment, name string) *appsv1.ReplicaSet {
	t.Helper()
	replicaSet := &appsv1.ReplicaSet{
		Name:            name,
		Namespace:       sandbox,
		OwnerReferences: ownedBy(deployment, "Deployment"),
		Spec: appsv1.ReplicaSetSpec{
			Selector: selectorFor(deployment.Name),
			Template: podTemplate(deployment.Name),
		},
	}
	mustCreate(t, env, replicaSet)
	return replicaSet
}

// createPod creates a pod in the sandbox namespace. A nil owners slice creates a pod with no owner.
func createPod(t testing.TB, env *testenv.Env, name string, owners []metav1.OwnerReference) {
	t.Helper()
	pod := &corev1.Pod{
		Name:            name,
		Namespace:       sandbox,
		OwnerReferences: owners,
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: containerName, Image: "registry.invalid/app:1"}},
		},
	}
	mustCreate(t, env, pod)
}

// ownedBy returns a controller reference to the object, of the given kind of the apps group.
func ownedBy(owner metav1.Object, kind string) []metav1.OwnerReference {
	gvk := appsv1.SchemeGroupVersion.WithKind(kind)
	return []metav1.OwnerReference{*metav1.NewControllerRef(owner, gvk)}
}

// pagesIn returns every Page in the pagerless namespace.
func pagesIn(t testing.TB, env *testenv.Env) []v1alpha1.Page {
	t.Helper()
	var list v1alpha1.PageList
	if err := env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatalf("list Pages: %v", err)
	}
	return list.Items
}

// onlyPage returns the single Page in the pagerless namespace.
func onlyPage(t testing.TB, env *testenv.Env) v1alpha1.Page {
	t.Helper()
	pages := pagesIn(t, env)
	if len(pages) != 1 {
		t.Fatalf("%d Pages exist, want 1", len(pages))
	}
	return pages[0]
}

// pageFor returns a Page for the Target, and fails the test when there is none.
func pageFor(t testing.TB, env *testenv.Env, target v1alpha1.TargetReference) v1alpha1.Page {
	t.Helper()
	for _, tr := range pagesIn(t, env) {
		if sameTarget(tr.Spec.Target, target) {
			return tr
		}
	}
	t.Fatalf("no Page exists for %+v", target)
	return v1alpha1.Page{}
}

// sameTarget reports whether two Target references name the same object, ignoring
// the API version and UID that the CLI records and a test fixture may omit.
func sameTarget(a, b v1alpha1.TargetReference) bool {
	return a.Kind == b.Kind && a.Namespace == b.Namespace && a.Name == b.Name
}

// createdName returns the name of the Page that a start command reports as created.
func createdName(t testing.TB, output string) string {
	t.Helper()
	match := createdPattern.FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("start printed %q, want the name of the created Page", output)
	}
	return match[1]
}

// lineContaining returns the first line of the output that contains the text.
func lineContaining(output, text string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, text) {
			return line
		}
	}
	return ""
}

// checkListed verifies that the listed Pages are exactly the named ones, each with its Target.
func checkListed(t testing.TB, items []v1alpha1.Page, want map[string]v1alpha1.TargetReference) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("listed %d Pages, want %d", len(items), len(want))
	}
	for _, tr := range items {
		target, ok := want[tr.Name]
		if !ok {
			t.Errorf("listed unexpected Page %s", tr.Name)
			continue
		}
		if !sameTarget(tr.Spec.Target, target) || tr.Status.Phase != v1alpha1.PhaseInvestigating {
			t.Errorf("listed %s as %+v in phase %s, want %+v in Investigating",
				tr.Name, tr.Spec.Target, tr.Status.Phase, target)
		}
	}
}

func noActionReport() agentapi.Report {
	return agentapi.Report{
		Diagnosis: "The checkout container exceeds its memory limit and is OOMKilled.",
		Findings:  []v1alpha1.Finding{logFinding},
		Proposal:  v1alpha1.ProposedAction{Action: v1alpha1.ActionNoAction},
	}
}
