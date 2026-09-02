package kernel

import (
	"context"
	"fmt"
	"strings"
)

// Steering lets a client inject additional user input INTO a running turn
// (codex/Claude-style mid-turn guidance) instead of rejecting it as "busy" or
// dropping it. The UI owns a channel and pushes the user's follow-up text onto
// it while the agent loop is running; the loop drains it at each iteration
// boundary and appends it as a user message before the next model call, so the
// model adjusts course in the same turn.

type steeringCtxKey struct{}

// SteeringInput is one durable piece of mid-turn guidance. ID and ContentHash
// are mailbox correlation fields; Content is the only field shown to the
// model. Keeping the durable ID through the loop lets the gateway acknowledge
// the exact mailbox row even when two messages contain identical text.
type SteeringInput struct {
	ID          string
	Content     string
	ContentHash string
}

type steeringChannels struct {
	inputs <-chan SteeringInput
	legacy <-chan string
}

// WithSteering attaches a steering channel to ctx. A nil channel is a no-op.
func WithSteering(ctx context.Context, ch <-chan string) context.Context {
	if ch == nil {
		return ctx
	}
	return context.WithValue(ctx, steeringCtxKey{}, steeringChannels{legacy: ch})
}

// WithSteeringInputs attaches the daemon's durable steering channel. New
// gateway code should use this form; WithSteering remains for embedders that
// have not adopted mailbox correlation metadata yet.
func WithSteeringInputs(ctx context.Context, ch <-chan SteeringInput) context.Context {
	if ch == nil {
		return ctx
	}
	return context.WithValue(ctx, steeringCtxKey{}, steeringChannels{inputs: ch})
}

func steeringFromContext(ctx context.Context) steeringChannels {
	if ctx == nil {
		return steeringChannels{}
	}
	if ch, ok := ctx.Value(steeringCtxKey{}).(steeringChannels); ok {
		return ch
	}
	return steeringChannels{}
}

// drainSteering non-blockingly collects all pending steering messages.
func drainSteering(ch steeringChannels) []SteeringInput {
	if ch.inputs == nil && ch.legacy == nil {
		return nil
	}
	var out []SteeringInput
	for {
		if ch.inputs != nil {
			select {
			case m := <-ch.inputs:
				m.Content = strings.TrimSpace(m.Content)
				if m.Content != "" {
					out = append(out, m)
				}
				continue
			default:
			}
		}
		if ch.legacy == nil {
			return out
		}
		select {
		case m := <-ch.legacy:
			if s := strings.TrimSpace(m); s != "" {
				out = append(out, SteeringInput{Content: s})
			}
		default:
			return out
		}
	}
}

// steeringContentForMain keeps legacy embedders byte-compatible while giving
// durable gateway input a server-issued handle. Main uses that handle only to
// separate genuinely independent work through queue_user_input; related input
// is applied normally to the current objective. The user's text remains a user
// message and is never promoted into system authority.
func steeringContentForMain(input SteeringInput) string {
	content := strings.TrimSpace(input.Content)
	if content == "" || strings.TrimSpace(input.ID) == "" {
		return content
	}
	return fmt.Sprintf(`[SelfMind live user input]
input_id: %s
content:
%s
[/SelfMind live user input]

Decide using the current Run context: if this input is related, incorporate it into the current work and plan. If it corrects a work_select proposal already made in this interaction, inspect the exact replacement and call work_select again before any material effect. If it is independent, call queue_user_input with this input_id before taking action on it; do not add independent work to the current plan. If it clearly continues another historical run, inspect that work and call queue_user_input with this input_id plus the exact parent_run_id; never switch the active run in place. If it explicitly asks to send the final result to this endpoint and set_delivery_target is available, call set_delivery_target with this exact input_id; do not infer a delivery move from an ordinary progress question.`, strings.TrimSpace(input.ID), content)
}
