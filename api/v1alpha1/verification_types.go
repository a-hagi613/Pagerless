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

// Names of the deterministic Verification checks recorded in a Page's status (ADR-0006).
const (
	// CheckArgoCDSynced passes once Argo CD reports the application Synced and Healthy at the merge commit.
	CheckArgoCDSynced = "ArgoCDSynced"
	// CheckRolloutAvailable passes when the Target's rollout is complete and available.
	CheckRolloutAvailable = "RolloutAvailable"
	// CheckNoNewRestarts passes when no container of the Target restarted or was OOMKilled during the window.
	CheckNoNewRestarts = "NoNewRestarts"
	// CheckAlertResolved passes when the triggering alert has resolved in Alertmanager. It is skipped for human
	// Triggers.
	CheckAlertResolved = "AlertResolved"
)
