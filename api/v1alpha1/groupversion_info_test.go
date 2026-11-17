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

package v1alpha1_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func TestGroupVersionIsPagerlessDev(t *testing.T) {
	if got, want := v1alpha1.GroupVersion.String(), "pagerless.dev/v1alpha1"; got != want {
		t.Fatalf("GroupVersion = %q, want %q", got, want)
	}
}

func TestAddToSchemeRegistersGroupVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if !scheme.IsVersionRegistered(v1alpha1.GroupVersion) {
		t.Fatalf("AddToScheme() did not register %s", v1alpha1.GroupVersion)
	}
}
