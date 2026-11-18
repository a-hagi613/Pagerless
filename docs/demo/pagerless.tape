# Records the terminal demo that the README shows, with VHS 0.12.1.
#
# The demo is one change of state, end to end: a Scenario breaks the Workload, a
# Trigger opens a Triage, the agent proposes one Action, a human approves it, the
# Executor applies it as a pull request, and Verification ends in Recovered.
#
# Record it against a live session, from the repository root (on Ubuntu 24.04
# with AppArmor userns restrictions, set VHS_NO_SANDBOX=1):
#
#   VHS_NO_SANDBOX=1 vhs docs/demo/pagerless.tape
#
# VHS needs ttyd and ffmpeg on PATH. Wait+Screen synchronizes with the real
# Scenario injection, agent run, pull request merge, 5-minute Verification
# window, and Narrative; Set PlaybackSpeed keeps the published GIF short without
# cutting the evidence out of it.
Output docs/demo/pagerless.gif

Set Shell "bash"
Set FontSize 15
Set FontFamily "JetBrains Mono, Consolas, monospace"
Set Width 1280
Set Height 720
Set Padding 16
Set TypingSpeed 45ms
Set PlaybackSpeed 6
Set Theme "Catppuccin Mocha"
Set CursorBlink false

Hide
Type "export PAGERLESS_KUBE_CONTEXT=pagerless-eks"
Enter
Type 'export PATH="$PWD/bin:/usr/local/bin:$PATH"'
Enter
Type "clear"
Enter
Sleep 500ms
Show

# A Scenario opens a pull request in the Sandbox and waits for the Workload to
# break and for the alert to fire.
Type "make break SCENARIO=oom"
Enter
Wait+Screen@600s /Restore the Sandbox with: make reset/
Sleep 5s

# One Trigger, one Active Triage. The newest Triage is the one the alert opened.
Type "kubectl triage list"
Enter
Wait+Screen@20s /Deployment\/sandbox\/checkout/
Sleep 4s
Type "TRIAGE=$(kubectl triage list | awk 'NR==2 {print $1}')"
Enter
Sleep 2s

# Follow it until the agent proposes an Action.
Type 'kubectl triage show "$TRIAGE" --watch'
Enter
Wait+Screen@360s /Narrative/
Sleep 4s

# The rendered change, then the approval bound to the Proposal's hash.
Type 'kubectl triage show "$TRIAGE"'
Enter
Sleep 6s
Type 'kubectl triage approve "$TRIAGE" --yes'
Enter
Wait+Screen@30s /created \(approved by/
Sleep 5s

# Wait for the pull request merge, Argo CD sync, 5-minute Verification window,
# and Narrative before showing the concluded Triage.
Hide
Type@1ms `deadline=$((SECONDS + 720)); nar_wait=0; while [ "$SECONDS" -lt "$deadline" ]; do out="$(kubectl triage show "$TRIAGE" 2>/dev/null || true)"; phase="$(awk '/^Phase:/ {print $2}' <<<"$out")"; nar="$(awk '/^Narrative$/ {getline; print $0}' <<<"$out")"; if [[ "$phase" =~ ^(Recovered|NotRecovered|Diagnosed|Rejected|Expired|Invalid|Failed)$ ]]; then if [ -n "$nar" ] && [ "$nar" != "  none" ]; then break; fi; nar_wait=$((nar_wait + 1)); [ "$nar_wait" -ge 12 ] && break; fi; sleep 10; done; echo "__PAGERLESS_READY__"`
Enter
Wait+Screen@750s /__PAGERLESS_READY__/
Type "clear"
Enter
Sleep 500ms
Show

# Verification decides, in code, whether the Workload recovered.
Type 'kubectl triage show "$TRIAGE"'
Enter
Wait+Screen@20s /Verification/
Sleep 8s
Type "kubectl triage list"
Enter
Wait+Screen@20s /Deployment\/sandbox\/checkout/
Sleep 6s
