# Records the terminal demo that the README shows, with VHS 0.12.1.
#
# The demo is one change of state, end to end: a Scenario breaks the Workload, a
# Trigger opens a Triage, the agent proposes one Action, a human approves it, the
# Executor applies it as a pull request, and Verification ends in Recovered.
#
# Record it against a live session, from the repository root:
#
#   export PAGERLESS_KUBE_CONTEXT=pagerless-eks
#   vhs docs/demo/pagerless.tape
#
# VHS needs ttyd and ffmpeg on PATH. The sleeps are generous because the agent
# run, the pull request merge and the observation window are real; Set
# PlaybackSpeed keeps the published GIF short without cutting the evidence out
# of it.
Output docs/demo/pagerless.gif

Set Shell "bash"
Set FontSize 15
Set FontFamily "JetBrains Mono, Consolas, monospace"
Set Width 1280
Set Height 720
Set Padding 16
Set TypingSpeed 45ms
Set PlaybackSpeed 3
Set Theme "Catppuccin Mocha"
Set CursorBlink false

Hide
Type "export PAGERLESS_KUBE_CONTEXT=pagerless-eks"
Enter
Type "clear"
Enter
Show

# A Scenario opens a pull request in the Sandbox and waits for the Workload to
# break and for the alert to fire.
Type "make break SCENARIO=oom"
Enter
Sleep 60s

# One Trigger, one Active Triage. The newest Triage is the one the alert opened.
Type "kubectl triage list"
Enter
Sleep 6s
Type "TRIAGE=$(kubectl triage list | awk 'NR==2 {print $1}')"
Enter

# Follow it until the agent proposes an Action.
Type "kubectl triage show \"$TRIAGE\" --watch"
Enter
Sleep 120s

# The rendered change, then the approval bound to the Proposal's hash.
Type "kubectl triage show \"$TRIAGE\""
Enter
Sleep 8s
Type "kubectl triage approve \"$TRIAGE\" --yes"
Enter
Sleep 120s

# Verification decides, in code, whether the Workload recovered.
Type "kubectl triage show \"$TRIAGE\""
Enter
Sleep 10s
