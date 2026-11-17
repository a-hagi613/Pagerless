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

// DeliveredByPullRequest reports whether the Executor delivers the Action as a
// Sandbox pull request: PatchResources, Scale, and Rollback. RestartWorkload
// executes live, and NoAction executes nothing.
func (a Action) DeliveredByPullRequest() bool {
	switch a {
	case ActionPatchResources, ActionScale, ActionRollback:
		return true
	default:
		return false
	}
}

// Reasons recorded on the Failed condition of a Triage whose execution ended it.
const (
	// ReasonExecutionRefused means the Executor refused to execute the approved Proposal.
	ReasonExecutionRefused = "ExecutionRefused"
	// ReasonPullRequestClosed means the pull request was closed without being merged.
	ReasonPullRequestClosed = "PullRequestClosed"
)
