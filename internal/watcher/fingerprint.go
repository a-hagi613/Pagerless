package watcher

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// fingerprintDomain separates the failure Fingerprint from every other SHA-256
// digest Pagerless computes. It ends in a newline so it is a prefix-free domain
// string, exactly as the v2 specification writes it.
const fingerprintDomain = "pagerless.dev/page-fingerprint/v1\n"

// subjectKeyDomain separates the Subject Key from the failure Fingerprint.
const subjectKeyDomain = "pagerless.dev/subject-key/v1"

// SubjectKey returns the canonical, label-safe identity of a Subject:
// lowercase, unpadded base32 of the SHA-256 over the Cluster identifier, API
// version, kind, namespace, name, and UID. It is 52 characters, so it fits a
// Kubernetes label value and can be selected on with one list.
func SubjectKey(subject Subject) string {
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	sum := hashFields(subjectKeyDomain, subject.FieldValues()...)
	return strings.ToLower(encoding.EncodeToString(sum))
}

// FailureFingerprint returns the stable identity of one failure: a
// domain-separated SHA-256 over the Subject, the reason class, and the scope.
// The fields are NUL-separated so no two different triples can produce the same
// canonical string. The message, note, timestamps, restart count, Event count,
// Pod name, node name, and pod-template hash are deliberately absent, so a
// crash storm is one Fingerprint rather than four hundred.
//
// A recreated Target has a new UID, which is part of the hash, so it opens a new
// Page instead of inheriting the old Cooldown.
func FailureFingerprint(subject Subject, class ReasonClass, scope string) string {
	sum := hashFields(fingerprintDomain, append(subject.FieldValues(), string(class), scope)...)
	return hex.EncodeToString(sum)
}

// hashFields hashes domain and fields with a NUL after each field, so that
// ("ab", "c") and ("a", "bc") produce different digests.
func hashFields(domain string, fields ...string) []byte {
	digest := sha256.New()
	digest.Write([]byte(domain))
	for _, field := range fields {
		digest.Write([]byte(field))
		digest.Write([]byte{0})
	}
	return digest.Sum(nil)
}
