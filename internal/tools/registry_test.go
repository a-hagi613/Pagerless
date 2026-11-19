package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
	"github.com/a-hagi613/pagerless/internal/history"
)

// fakeLogConnector is one configured log Connector that records its query.
type fakeLogConnector struct {
	query contract.Query
}

func (f *fakeLogConnector) Name() string { return testConnector }

func (f *fakeLogConnector) QueryLogs(_ context.Context, query contract.Query) (contract.LogPage, error) {
	f.query = query
	return contract.LogPage{Entries: []contract.LogEntry{{Line: testKilledLine, Severity: "error"}}}, nil
}

// TestTheRegistryConnectorGetsThePagesNamespaceAndTarget checks that the
// adapter passes the broker's injected scope, never a model-supplied one.
func TestTheRegistryConnectorGetsThePagesNamespaceAndTarget(t *testing.T) {
	t.Parallel()
	connector := &fakeLogConnector{}
	source := RegistryConnectors{Registry: &connectors.Registry{Logs: []contract.LogConnector{connector}}}
	if names := source.Names(); len(names) != 1 || names[0] != testConnector {
		t.Errorf("names = %v, want the configured Connector", names)
	}
	page := pageFixture()
	query := LogQuery{
		Namespace: page.Spec.Target.Namespace, Subject: page.Spec.Target,
		Since: time.Unix(0, 0), Until: time.Unix(60, 0), Limit: 10,
	}
	if _, err := source.Logs(t.Context(), testConnector, query); err != nil {
		t.Fatal(err)
	}
	if connector.query.Namespace != page.Spec.Target.Namespace || connector.query.Subject != page.Spec.Target {
		t.Errorf("Connector query = %+v, want the Page's namespace and Target", connector.query)
	}
	if _, err := source.Logs(t.Context(), "not-configured", query); err == nil {
		t.Error("an unconfigured Connector was queried")
	}
	if _, err := source.Health(t.Context(), testConnector); err != nil {
		t.Errorf("Health of a configured Connector failed: %v", err)
	}
}

// TestTheManifestServesTheStoredContextBundle checks that the manifest carries
// the bundle the server assembled, not an empty object.
func TestTheManifestServesTheStoredContextBundle(t *testing.T) {
	t.Parallel()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	page := pageFixture()
	const body = `{"target":{"kind":"Deployment"},"logs":[{"pod":"checkout","lines":["Killed"]}]}`
	if err := store.PutBundle(t.Context(), history.Bundle{
		PageUID: string(page.UID), Namespace: page.Namespace, Page: page.Name,
		Hash: "0f3a1b2c", Bytes: int64(len(body)), CollectedAt: time.Now(), Body: body,
	}); err != nil {
		t.Fatal(err)
	}
	broker, _, _, _ := brokerFixture(t)
	broker.Context = &HistoryContext{Store: store}
	manifest, err := broker.Manifest(t.Context(), page)
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest.Context) != body {
		t.Errorf("Context = %s, want the stored bundle", manifest.Context)
	}
	var decoded map[string]any
	if err := json.Unmarshal(manifest.Context, &decoded); err != nil {
		t.Errorf("the Context Bundle is not JSON: %v", err)
	}
	if _, ok := decoded["logs"]; !ok {
		t.Errorf("the Context Bundle lost its evidence: %s", manifest.Context)
	}
}

// TestTheManifestWithoutABundleReportsAnEmptyObject keeps an unassembled Page
// readable.
func TestTheManifestWithoutABundleReportsAnEmptyObject(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, _, _ := brokerFixture(t)
	broker.Context = &HistoryContext{}
	manifest, err := broker.Manifest(t.Context(), page)
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest.Context) != `{}` {
		t.Errorf("Context = %s, want an empty object", manifest.Context)
	}
}
