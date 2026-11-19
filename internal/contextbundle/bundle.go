package contextbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Version is the bundle format version.
const Version = 1

// Section is one component of the bundle, in fixed assembly order.
type Section struct {
	// Name identifies the component, such as target, pods, native-logs, or
	// connector:metrics:prometheus.
	Name string `json:"name"`
	// Connector names the producing Connector when the section came from one.
	Connector string `json:"connector,omitempty"`
	// Truncated reports whether the component cap cut this section.
	Truncated bool `json:"truncated,omitempty"`
	// Notes record degradation or truncation that belongs in the evidence.
	Notes []string `json:"notes,omitempty"`
	// Payload is the sanitized component content.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Bytes is the serialized payload size.
	Bytes int `json:"bytes"`
	// Redactions is the number of values the sanitizer replaced here.
	Redactions int `json:"redactions,omitempty"`
}

// Bundle is the bounded, sanitized evidence for one Page.
type Bundle struct {
	// Version is the bundle format version.
	Version int `json:"version"`
	// Page is the Page name.
	Page string `json:"page"`
	// PageUID is the Page UID, so a recreated Page does not reuse a bundle.
	PageUID string `json:"pageUID,omitempty"`
	// Namespace is the Page namespace.
	Namespace string `json:"namespace"`
	// Target is the Target the bundle describes.
	Target v1alpha1.TargetReference `json:"target"`
	// CollectedAt is when collection started.
	CollectedAt time.Time `json:"collectedAt"`
	// Sections are the components in fixed assembly order.
	Sections []Section `json:"sections"`
	// Notes are bundle-level degradation notes.
	Notes []string `json:"notes,omitempty"`
	// Truncated reports whether the total cap dropped evidence.
	Truncated bool `json:"truncated"`
	// Bytes is the serialized bundle size.
	Bytes int `json:"bytes"`
	// Redactions is the total number of sanitized values.
	Redactions int `json:"redactions,omitempty"`
	// Hash is the SHA-256 of the bundle content.
	Hash string `json:"hash"`
}

// content is the hashable part of a bundle, without the derived size and hash.
type content struct {
	Version     int                      `json:"version"`
	Page        string                   `json:"page"`
	PageUID     string                   `json:"pageUID,omitempty"`
	Namespace   string                   `json:"namespace"`
	Target      v1alpha1.TargetReference `json:"target"`
	CollectedAt time.Time                `json:"collectedAt"`
	Sections    []Section                `json:"sections"`
	Notes       []string                 `json:"notes,omitempty"`
	Truncated   bool                     `json:"truncated"`
	Redactions  int                      `json:"redactions,omitempty"`
}

// Encode serializes the bundle, filling Bytes and Hash. The same evidence
// always produces the same bytes and hash.
func (b *Bundle) Encode() ([]byte, error) {
	body := content{
		Version: b.Version, Page: b.Page, PageUID: b.PageUID, Namespace: b.Namespace, Target: b.Target,
		CollectedAt: b.CollectedAt.UTC(), Sections: b.Sections, Notes: b.Notes, Truncated: b.Truncated,
		Redactions: b.Redactions,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode the Context Bundle: %w", err)
	}
	sum := sha256.Sum256(encoded)
	b.Hash = hex.EncodeToString(sum[:])
	b.Bytes = len(encoded)
	return encoded, nil
}

// Summary returns the Page status summary for this bundle. The body never
// enters the Page object.
func (b *Bundle) Summary() *v1alpha1.EvidenceSummary {
	connectors := make([]string, 0, len(b.Sections))
	notes := make([]string, 0, len(b.Notes))
	for _, section := range b.Sections {
		if section.Connector != "" {
			connectors = append(connectors, section.Connector)
		}
		for _, note := range section.Notes {
			if len(notes) < v1alpha1.MaxEvidenceNotes {
				notes = append(notes, truncate(note, 300))
			}
		}
	}
	for _, note := range b.Notes {
		if len(notes) < v1alpha1.MaxEvidenceNotes {
			notes = append(notes, truncate(note, 300))
		}
	}
	at := metav1.NewTime(b.CollectedAt.UTC())
	return &v1alpha1.EvidenceSummary{
		BundleHash:  b.Hash,
		Bytes:       int64(b.Bytes),
		Truncated:   b.Truncated,
		CollectedAt: &at,
		Connectors:  connectors,
		Notes:       notes,
		Redactions:  int32(b.Redactions),
	}
}

// truncate bounds a note.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
