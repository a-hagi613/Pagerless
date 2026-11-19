package watcher

import (
	"strings"
	"testing"
)

// testSubject returns a Subject for the tests.
func testSubject() Subject {
	return Subject{
		ClusterID:  "cluster-a",
		APIVersion: apiVersionApps,
		Kind:       testKindDeployment,
		Namespace:  testNamespace,
		Name:       testDeployment,
		UID:        testDeploymentUID,
	}
}

func TestSubjectKeyIsLabelSafeAndStable(t *testing.T) {
	t.Parallel()
	key := SubjectKey(testSubject())
	if len(key) != 52 {
		t.Fatalf("SubjectKey length = %d, want 52", len(key))
	}
	if key != strings.ToLower(key) {
		t.Fatalf("SubjectKey = %q, want lowercase", key)
	}
	for _, char := range key {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			t.Fatalf("SubjectKey = %q, want base32 characters only", key)
		}
	}
	if again := SubjectKey(testSubject()); again != key {
		t.Fatalf("SubjectKey is not stable: %q != %q", again, key)
	}
}

func TestSubjectKeySeparatesSubjects(t *testing.T) {
	t.Parallel()
	base := testSubject()
	cases := map[string]func(*Subject){
		"cluster":   func(s *Subject) { s.ClusterID = "cluster-b" },
		"api":       func(s *Subject) { s.APIVersion = "apps/v1beta1" },
		"kind":      func(s *Subject) { s.Kind = testKindStatefulSet },
		"namespace": func(s *Subject) { s.Namespace = testOtherNamespace },
		"name":      func(s *Subject) { s.Name = testOtherDeployment },
		"uid":       func(s *Subject) { s.UID = testRecreatedUID },
	}
	key := SubjectKey(base)
	for name, mutate := range cases {
		changed := base
		mutate(&changed)
		if SubjectKey(changed) == key {
			t.Errorf("changing the %s must change the SubjectKey", name)
		}
	}
}

func TestFailureFingerprintIsStable(t *testing.T) {
	t.Parallel()
	subject := testSubject()
	fingerprint := FailureFingerprint(subject, ReasonPodCrashLoop, testScope)
	if len(fingerprint) != 64 || strings.ToLower(fingerprint) != fingerprint {
		t.Fatalf("FailureFingerprint = %q, want 64 lowercase hex characters", fingerprint)
	}
	if again := FailureFingerprint(subject, ReasonPodCrashLoop, testScope); again != fingerprint {
		t.Fatalf("FailureFingerprint is not stable: %q != %q", again, fingerprint)
	}
}

func TestFailureFingerprintSeparatesReasonScopeAndSubject(t *testing.T) {
	t.Parallel()
	subject := testSubject()
	base := FailureFingerprint(subject, ReasonPodCrashLoop, testScope)
	cases := map[string]string{
		"reason": FailureFingerprint(subject, ReasonPodImagePull, testScope),
		"scope":  FailureFingerprint(subject, ReasonPodCrashLoop, "worker"),
		"recreated": FailureFingerprint(Subject{
			ClusterID: subject.ClusterID, APIVersion: subject.APIVersion, Kind: subject.Kind,
			Namespace: subject.Namespace, Name: subject.Name, UID: testRecreatedUID,
		}, ReasonPodCrashLoop, testScope),
		"namespace": FailureFingerprint(Subject{
			ClusterID: subject.ClusterID, APIVersion: subject.APIVersion, Kind: subject.Kind,
			Namespace: testOtherNamespace, Name: subject.Name, UID: subject.UID,
		}, ReasonPodCrashLoop, testScope),
		"cluster": FailureFingerprint(Subject{
			ClusterID: "cluster-b", APIVersion: subject.APIVersion, Kind: subject.Kind,
			Namespace: subject.Namespace, Name: subject.Name, UID: subject.UID,
		}, ReasonPodCrashLoop, testScope),
	}
	for name, fingerprint := range cases {
		if fingerprint == base {
			t.Errorf("changing the %s must change the Fingerprint", name)
		}
	}
}

// TestFailureFingerprintIgnoresEvidence proves that nothing a pod or an Event
// adds as evidence can change identity: the function only receives the Subject,
// the reason class, and the scope, so a message, a count, a timestamp, a pod
// name, a node name, or a pod-template hash has no way into the digest.
func TestFailureFingerprintExcludesEvidence(t *testing.T) {
	t.Parallel()
	subject := testSubject()
	fingerprint := FailureFingerprint(subject, ReasonPodCrashLoop, testScope)

	// A second Pod of the same Deployment, on another node, with a different
	// hash suffix, resolves to the same Subject and therefore the same Fingerprint.
	resolvedFromAnotherPod := Subject{
		ClusterID:  subject.ClusterID,
		APIVersion: subject.APIVersion,
		Kind:       subject.Kind,
		Namespace:  subject.Namespace,
		Name:       subject.Name,
		UID:        subject.UID,
		Annotations: map[string]string{
			"pod-template-hash": "7d9f",
		},
	}
	if got := FailureFingerprint(resolvedFromAnotherPod, ReasonPodCrashLoop, testScope); got != fingerprint {
		t.Fatalf("evidence changed the Fingerprint: %q != %q", got, fingerprint)
	}
}
