package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newReplyCommand adds a turn to an existing conversation.
func newReplyCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reply <pageId> <message...>",
		Short: "Add a follow-up turn to an investigation",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 2 {
				return usagef("reply needs a page ID and a message")
			}
			return nil
		},
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.ask(ctx, strings.Join(args[1:], " "), args[0])
		}),
	}
	cmd.Flags().StringVar(&a.opts.targetHint, "target", "",
		"optional Kubernetes target, as kind/name or namespace/kind/name")
	return cmd
}

// ask opens a page, or adds a turn when pageID is set, then streams the turn.
func (a *app) ask(ctx context.Context, message, pageID string) error {
	request, err := a.requestFor(message)
	if err != nil {
		return err
	}
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	var created createResponse
	if pageID == "" {
		created, err = api.createPage(ctx, request)
	} else {
		created, err = api.createTurn(ctx, pageID, request)
	}
	if err != nil {
		return err
	}
	if created.PageID == "" || created.TurnID == "" {
		return &ExitError{Code: ExitFailure, Err: errors.New(
			"the server accepted the request without returning a page and turn ID")}
	}
	style := newStyler(a.errOut())
	a.progressf("%s %s %s %s\n",
		style.dim("page"), style.cyan(created.PageID), style.dim("turn"), style.cyan(created.TurnID))
	outcome, err := a.consume(ctx, api, created.PageID, 0, false)
	if err != nil {
		return err
	}
	if outcome.Detached {
		return a.reportDetach(created.PageID, created.TurnID, outcome)
	}
	detail, err := api.page(ctx, created.PageID)
	if err != nil {
		return err
	}
	if a.opts.json {
		if err := writeJSON(a.out(), detail); err != nil {
			return err
		}
	} else if err := a.printDetail(detail); err != nil {
		return err
	}
	return turnExit(outcome.Status)
}

// requestFor validates the message and the optional hints.
func (a *app) requestFor(message string) (createRequest, error) {
	request := createRequest{Message: strings.TrimSpace(message)}
	if request.Message == "" {
		return request, usagef("the question must not be empty")
	}
	hint, err := parseTarget(a.opts.targetHint)
	if err != nil {
		return request, err
	}
	namespace := strings.TrimSpace(a.opts.namespace)
	if hint != nil && hint.Namespace != "" && namespace != "" && hint.Namespace != namespace {
		return request, usagef("--namespace %q contradicts target namespace %q", namespace, hint.Namespace)
	}
	request.Namespace = namespace
	request.Target = hint
	return request, nil
}

// parseTarget reads kind/name or namespace/kind/name.
func parseTarget(value string) (*target, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return nil, usagef("target %q has an empty part; use kind/name", value)
		}
	}
	switch len(parts) {
	case 2:
		return &target{Kind: parts[0], Name: parts[1]}, nil
	case 3:
		return &target{Namespace: parts[0], Kind: parts[1], Name: parts[2]}, nil
	default:
		return nil, usagef("target %q is not kind/name or namespace/kind/name", value)
	}
}

// turnExit maps a terminal turn status to the CLI's exit contract.
func turnExit(status string) error {
	switch status {
	case statusCompleted, "":
		return nil
	case statusStopped:
		return &ExitError{Code: ExitFailure, Err: errors.New(
			"the turn was stopped before it completed; the transcript keeps what was recorded")}
	case statusInterrupted:
		return &ExitError{Code: ExitFailure, Err: errors.New(
			"the turn was interrupted; reply to continue, or start a new page")}
	case statusFailed:
		return &ExitError{Code: ExitFailure, Err: errors.New("the turn failed; inspect its trace for the cause")}
	default:
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("the turn ended with unexpected status %q", status)}
	}
}

// consume follows one turn, saves the resume cursor, and renders progress.
func (a *app) consume(ctx context.Context, api *client, pageID string, after int64, trace bool) (streamOutcome, error) {
	streamCtx, cancel := context.WithTimeout(ctx, a.opts.followTimeout)
	defer cancel()
	presentation := newPrinter(a.out())
	handle := func(current event) {
		_ = saveCursor(pageID, current.Seq)
		if trace {
			if a.opts.json {
				if presentation.err == nil {
					presentation.err = writeJSONLine(a.out(), current)
				}
			} else {
				writeEvent(presentation, current)
			}
			return
		}
		a.writeProgress(current)
	}
	outcome, err := api.follow(streamCtx, pageID, after, handle)
	if presentation.err != nil {
		return outcome, presentation.err
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return outcome, &ExitError{Code: ExitFailure, Err: fmt.Errorf(
				"stopped waiting after %s; the turn may still be running: "+
					"kubectl page follow %s --after %d",
				a.opts.followTimeout, pageID, outcome.LastSeq)}
		}
		return outcome, err
	}
	return outcome, nil
}

// reportDetach explains that Ctrl-C left the turn running.
func (a *app) reportDetach(pageID, turnID string, outcome streamOutcome) error {
	if a.opts.json {
		return writeJSON(a.out(), struct {
			PageID      string `json:"pageId"`
			TurnID      string `json:"turnId,omitempty"`
			Detached    bool   `json:"detached"`
			LastEventID int64  `json:"lastEventId"`
		}{PageID: pageID, TurnID: turnID, Detached: true, LastEventID: outcome.LastSeq})
	}
	a.progressf("\nDetached from page %s at event %d; the turn is still running.\n", pageID, outcome.LastSeq)
	a.progressf("  Resume: kubectl page follow %s\n", pageID)
	a.progressf("  Stop:   kubectl page stop %s\n", pageID)
	return nil
}

// writeProgress renders one event on standard error while the answer is still
// being prepared. Standard output stays reserved for the result.
func (a *app) writeProgress(current event) {
	var data eventData
	_ = json.Unmarshal(current.Data, &data)
	style := newStyler(a.errOut())
	switch current.Type {
	case eventToolStarted:
		a.progressf("%s %s %s%s\n",
			style.cyan("->"), style.bold(sanitize(orPlaceholder(data.Tool))),
			style.dim("(call "+sanitize(orPlaceholder(data.CallID))+")"),
			style.dim(argumentLine(data.Arguments)))
	case eventToolFinished:
		arrow := style.green("<-")
		if status := sanitize(orPlaceholder(data.Status)); status != statusOK && status != statusCompleted {
			arrow = style.red("<-")
		}
		a.progressf("%s %s %s %s %s%s\n",
			arrow, style.bold(sanitize(orPlaceholder(data.Tool))),
			style.dim("(call "+sanitize(orPlaceholder(data.CallID))+")"),
			style.status(sanitize(orPlaceholder(data.Status))),
			style.dim(formatDuration(data.DurationMS)), flags(data))
	case eventProposal:
		a.progressf("%s %d file(s)%s\n", style.magenta("proposal:"), len(data.Files), style.dim(urlSuffix(data.PRURL)))
	case eventTurnFinished:
		a.progressf("%s %s\n", style.dim("turn finished:"),
			style.status(sanitize(orPlaceholder(terminalStatus(current)))))
	}
}

// writeEvent renders one replayed event as the command's primary output.
func writeEvent(presentation *printer, current event) {
	var data eventData
	_ = json.Unmarshal(current.Data, &data)
	header := fmt.Sprintf("[%d] %s", current.Seq, formatTime(current.Time))
	switch current.Type {
	case eventToolStarted:
		presentation.printf("%s tool.started %s %s\n",
			presentation.style.dim(header), presentation.style.bold(sanitize(orPlaceholder(data.Tool))),
			presentation.style.dim("(call "+sanitize(orPlaceholder(data.CallID))+")"))
		if arguments := compactJSON(data.Arguments); arguments != "" {
			presentation.printf("  %s\n", presentation.style.dim(arguments))
		}
	case eventToolFinished:
		presentation.printf("%s tool.finished %s %s %s %s%s\n",
			presentation.style.dim(header), presentation.style.bold(sanitize(orPlaceholder(data.Tool))),
			presentation.style.dim("(call "+sanitize(orPlaceholder(data.CallID))+")"),
			presentation.style.status(sanitize(orPlaceholder(data.Status))),
			presentation.style.dim(formatDuration(data.DurationMS)), flags(data))
	case eventAnswer:
		presentation.printf("%s %s\n", presentation.style.dim(header), presentation.style.cyan("answer"))
		presentation.markdown("  ", sanitize(data.Content))
	case eventProposal:
		presentation.printf("%s %s %d file(s)%s\n", presentation.style.dim(header),
			presentation.style.magenta("proposal"), len(data.Files), presentation.style.dim(urlSuffix(data.PRURL)))
	case eventTurnFinished:
		presentation.printf("%s turn.finished %s\n", presentation.style.dim(header),
			presentation.style.status(sanitize(orPlaceholder(terminalStatus(current)))))
	default:
		presentation.printf("%s %s\n", presentation.style.dim(header), sanitize(current.Type))
		if detail := compactJSON(current.Data); detail != "" {
			presentation.printf("  %s\n", presentation.style.dim(detail))
		}
	}
}

// printDetail renders one conversation for a person.
func (a *app) printDetail(detail pageDetail) error {
	presentation := newPrinter(a.out())
	presentation.field("ID", presentation.style.cyan(sanitize(detail.ID)))
	presentation.field("Title", presentation.style.bold(sanitize(orPlaceholder(detail.Title))))
	presentation.field("Status", presentation.style.status(sanitize(orPlaceholder(detail.Status))))
	presentation.field("Updated", presentation.style.dim(formatTime(detail.UpdatedAt)))
	presentation.field("Active turn", presentation.style.cyan(sanitize(activeTurn(detail.ActiveTurnID))))
	presentation.field("Last event", fmt.Sprintf("%d", detail.LastEventID))
	presentation.section("Messages")
	if len(detail.Messages) == 0 {
		presentation.printf("  %s\n", placeholder)
	}
	for _, item := range detail.Messages {
		role := sanitize(orPlaceholder(item.Role))
		if item.Role == roleAssistant {
			role = presentation.style.cyan(role)
		} else {
			role = presentation.style.bold(role)
		}
		presentation.printf("\n[%s] %s\n", role, presentation.style.dim(formatTime(item.CreatedAt)))
		content := sanitize(item.Content)
		if item.Role == roleAssistant {
			presentation.markdown("  ", content)
		} else {
			presentation.block("  ", content)
		}
		for _, cite := range item.Citations {
			line := presentation.style.cyan(sanitize(cite.CallID))
			if cite.Pointer != "" {
				line += " " + presentation.style.dim(sanitize(cite.Pointer))
			}
			presentation.printf("  %s %s\n", presentation.style.dim("citation:"), line)
		}
	}
	presentation.printf("\n%s %s %s\n", presentation.style.dim("Resume:"),
		presentation.style.cyan("kubectl page reply "+sanitize(detail.ID)), presentation.style.dim("\"...\""))
	presentation.printf("%s %s\n", presentation.style.dim("Trace: "),
		presentation.style.cyan("kubectl page activity "+sanitize(detail.ID)))
	return presentation.err
}

// argumentLine renders a started tool's arguments for a progress line.
func argumentLine(raw json.RawMessage) string {
	arguments := compactJSON(raw)
	if arguments == "" {
		return ""
	}
	return " " + arguments
}

// flags renders the redaction and truncation hints of a finished tool.
func flags(data eventData) string {
	var notes []string
	if data.Redacted {
		notes = append(notes, "redacted")
	}
	if data.Truncated {
		notes = append(notes, "truncated")
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, ", ") + ")"
}

// urlSuffix renders a proposal address after a colon.
func urlSuffix(address string) string {
	if address == "" {
		return ""
	}
	return " " + sanitize(address)
}

// orPlaceholder substitutes the placeholder for an empty value.
func orPlaceholder(value string) string {
	if value == "" {
		return placeholder
	}
	return value
}
