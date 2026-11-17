package v1alpha1

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// TriageApprovalName is the one resource name on which decisions for this exact Proposal contend. The digest
// binds the complete namespace, name, UID, and Proposal hash, so long names cannot collide through truncation
// and a recreated Triage cannot reuse an earlier decision. The result fits a single 63-character DNS label.
func TriageApprovalName(tr *Triage, proposalHash string) string {
	identity := tr.Namespace + "\x00" + tr.Name + "\x00" + string(tr.UID) + "\x00" + proposalHash
	digest := sha256.Sum256([]byte(identity))
	return "decision-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
}
