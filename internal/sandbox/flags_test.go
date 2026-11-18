package sandbox_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	// flagdConfigPath is the Sandbox manifest that holds the demo's feature flags.
	// The cart, payment, product-catalog and email Workloads declare it in
	// pagerless.dev/depends-on-paths, so a Rollback may revert a commit that
	// changes it (ADR-0021).
	flagdConfigPath = "workloads/configmaps/flagd-config.yaml"
	// flagdConfigKey is the ConfigMap key that flagd reads.
	flagdConfigKey = "demo.flagd.json"
	// variantOff is the variant every flag under test carries before its Scenario.
	variantOff = "off"
)

// knownGoodFlagdConfig is the Sandbox's workloads/configmaps/flagd-config.yaml at
// the known-good tag (85f6090), byte for byte, so that these tests apply the
// Scenario patches to the state that `make break` applies them to.
var knownGoodFlagdConfig = filepath.Join("testdata", "known-good-flagd-config.yaml")

// flagdFlag is one feature flag of the flagd configuration.
type flagdFlag struct {
	DefaultVariant string         `json:"defaultVariant"`
	Description    string         `json:"description"`
	State          string         `json:"state"`
	Targeting      map[string]any `json:"targeting"`
	Variants       map[string]any `json:"variants"`
}

// flagdDocument is the flagd flag file that the ConfigMap carries under the
// demo.flagd.json key.
type flagdDocument struct {
	Schema string               `json:"$schema"`
	Flags  map[string]flagdFlag `json:"flags"`
}

// readFlagdConfig parses the flag file out of a flagd-config ConfigMap manifest.
func readFlagdConfig(t *testing.T, manifest []byte) flagdDocument {
	t.Helper()
	var configMap struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := yaml.UnmarshalStrict(manifest, &configMap); err != nil {
		t.Fatalf("the flagd ConfigMap is not valid YAML: %v", err)
	}
	if configMap.APIVersion != "v1" || configMap.Kind != "ConfigMap" {
		t.Fatalf("the manifest is %s %s, want a v1 ConfigMap", configMap.APIVersion, configMap.Kind)
	}
	if configMap.Metadata.Name != "flagd-config" || configMap.Metadata.Namespace != "sandbox" {
		t.Fatalf("the manifest is ConfigMap %s/%s, want flagd-config/sandbox",
			configMap.Metadata.Namespace, configMap.Metadata.Name)
	}
	text, ok := configMap.Data[flagdConfigKey]
	if !ok {
		t.Fatalf("the ConfigMap has no %s key", flagdConfigKey)
	}
	var document flagdDocument
	if err := json.Unmarshal([]byte(text), &document); err != nil {
		t.Fatalf("%s is not valid JSON: %v", flagdConfigKey, err)
	}
	if len(document.Flags) == 0 {
		t.Fatalf("%s defines no flags", flagdConfigKey)
	}
	return document
}

// applyFlagdScenarioPatch applies a flag Scenario's inject.patch to the
// known-good flagd ConfigMap and returns the flags that result.
func applyFlagdScenarioPatch(t *testing.T, scenario string, patch []byte) flagdDocument {
	t.Helper()
	knownGood := mustReadFile(t, knownGoodFlagdConfig)
	return readFlagdConfig(t, applyScenarioPatch(t, scenario, flagdConfigPath, knownGood, patch))
}

// The four flag Scenarios change the flagd ConfigMap, and each one must both
// apply to the known-good file and make its flag take effect: the variant it
// selects must exist, and no other flag may change.
func TestFlagScenarioPatchesEnableTheirFlagInTheFlagdConfigMap(t *testing.T) {
	t.Parallel()
	knownGood := readFlagdConfig(t, mustReadFile(t, knownGoodFlagdConfig))
	cases := []struct {
		scenario string
		flag     string
		variant  string
		// targeting is whether the shipped flag keeps a per-product targeting rule.
		targeting bool
	}{
		{scenario: "cart-flag", flag: "cartFailure", variant: "100%"},
		{scenario: "payment-flag", flag: "paymentFailure", variant: "100%"},
		{scenario: "product-catalog-flag", flag: "productCatalogFailure", variant: "on"},
		{scenario: "email-memory-leak", flag: "emailMemoryLeak", variant: "100x"},
	}
	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join("..", "..", "scenarios", tc.scenario, "inject.patch")
			patch := mustReadFile(t, path)
			if got := patchTarget(t, tc.scenario, string(patch)); got != flagdConfigPath {
				t.Fatalf("the patch changes %q, want %q", got, flagdConfigPath)
			}
			patched := applyFlagdScenarioPatch(t, tc.scenario, patch)

			injected, ok := patched.Flags[tc.flag]
			if !ok {
				t.Fatalf("the patched flag file no longer defines %s", tc.flag)
			}
			if injected.DefaultVariant != tc.variant {
				t.Errorf("%s defaultVariant = %q, want %q", tc.flag, injected.DefaultVariant, tc.variant)
			}
			if _, ok := injected.Variants[tc.variant]; !ok {
				t.Errorf("%s has no %q variant, so the flag could not resolve to it", tc.flag, tc.variant)
			}
			if injected.State != "ENABLED" {
				t.Errorf("%s state = %q, want ENABLED", tc.flag, injected.State)
			}
			// productCatalogFailure ships with a targeting rule whose branches both
			// return off, so it fails one product at most and is inert as shipped.
			// Every other flag under test has no targeting rule to widen.
			if tc.targeting && len(injected.Targeting) != 0 {
				t.Errorf("%s has a targeting rule, want none", tc.flag)
			}
			// A Scenario commits one flag and nothing else: the same document with the
			// one flag replaced by its injected form must equal the patched document.
			want := make(map[string]flagdFlag, len(knownGood.Flags))
			maps.Copy(want, knownGood.Flags)
			want[tc.flag] = injected
			if !reflect.DeepEqual(patched.Flags, want) {
				t.Errorf("the patch changes more than the %s flag", tc.flag)
			}
		})
	}
}

// productCatalogFailure is the one flag whose injection must widen the targeting
// rule: as shipped it matches a single product and both branches return off, so a
// Scenario that only flipped a branch would still fail at most one product and
// could never raise an error rate. The injection removes the rule, so every
// GetProduct call fails.
func TestTheProductCatalogFailureInjectionWidensTargetingToEveryProduct(t *testing.T) {
	t.Parallel()
	knownGood := readFlagdConfig(t, mustReadFile(t, knownGoodFlagdConfig)).Flags["productCatalogFailure"]
	if len(knownGood.Targeting) == 0 {
		t.Fatal("the known-good productCatalogFailure has no targeting rule, so the Scenario would inject nothing")
	}
	if knownGood.DefaultVariant != variantOff {
		t.Fatalf("known-good productCatalogFailure defaultVariant = %q, want off", knownGood.DefaultVariant)
	}
	patched := applyFlagdScenarioPatch(t, "product-catalog-flag",
		mustReadFile(t, filepath.Join("..", "..", "scenarios", "product-catalog-flag", "inject.patch")))
	flag := patched.Flags["productCatalogFailure"]
	if len(flag.Targeting) != 0 {
		t.Errorf("the injected flag still targets products: %v", flag.Targeting)
	}
	if flag.DefaultVariant != "on" {
		t.Errorf("the injected flag defaultVariant = %q, want on for every product", flag.DefaultVariant)
	}
}

// The recorded known-good file must be the state before any Scenario, so that a
// patch that changes nothing is caught rather than passing on an injected file.
func TestTheRecordedFlagdConfigurationIsTheKnownGoodState(t *testing.T) {
	t.Parallel()
	flags := readFlagdConfig(t, mustReadFile(t, knownGoodFlagdConfig)).Flags
	for _, name := range []string{"cartFailure", "paymentFailure", "emailMemoryLeak"} {
		flag, ok := flags[name]
		if !ok {
			t.Fatalf("the known-good flag file does not define %s", name)
		}
		if flag.DefaultVariant != variantOff {
			t.Errorf("known-good %s defaultVariant = %q, want off", name, flag.DefaultVariant)
		}
	}
	catalog, ok := flags["productCatalogFailure"]
	if !ok {
		t.Fatal("the known-good flag file does not define productCatalogFailure")
	}
	if catalog.DefaultVariant != variantOff || len(catalog.Targeting) == 0 {
		t.Errorf("known-good productCatalogFailure = %+v, want the shipped inert targeting", catalog)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
