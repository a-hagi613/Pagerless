/*
Copyright 2026 Abdullahi Hagi.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
)

// fingerprintDomain separates a Fingerprint digest from every other SHA-256
// digest Pagerless computes, so a Fingerprint can never collide with, for
// example, a Proposal hash.
const fingerprintDomain = "pagerless.dev/fingerprint/v1"

// Fingerprint returns the stable failure identity of one Target for one reason.
// It hashes only identity-bearing fields: the kind, namespace, name, and UID,
// and the reason class. Messages, timestamps, pod names, and repetition counts
// are deliberately absent, so one crash loop is one Fingerprint (see
// GLOSSARY.md).
func Fingerprint(target TargetReference, reason string) string {
	digest := sha256.New()
	for _, field := range []string{
		fingerprintDomain,
		target.APIVersion,
		string(target.Kind),
		target.Namespace,
		target.Name,
		target.UID,
		reason,
	} {
		digest.Write([]byte(field))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// PageName returns a readable, DNS-safe Page name for a Target and
// Fingerprint. A recreated Target with the same name and a new UID gets a
// different suffix, because the UID is part of the Fingerprint.
func PageName(target TargetReference, fingerprint string) string {
	suffix := fingerprint
	if len(suffix) > 10 {
		suffix = suffix[:10]
	}
	name := target.Name + "-" + suffix
	if len(name) > 253 {
		name = name[len(name)-253:]
	}
	return name
}
