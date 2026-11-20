// Package dispatch enforces session authority at the sole external write boundary.
package dispatch

import (
	"context"
	"release-assistant/internal/release"
)

type (
	// Identity is a fresh permission-relevant target observation.
	Identity struct {
		Target release.Target
		SHA    string
		// Base binds PR permission to its observed target branch; tags leave it empty.
		Base         string
		Open, OptOut bool
		// CommentLabel is valid only with fresh PR identity evidence.
		CommentLabel bool
		Evidence     release.Evidence
	}
	// Reader verifies current target identity and write-related labels.
	Reader interface {
		ReadTarget(context.Context, release.Target) (Identity, error)
	}
	// Planner supplies fresh library-approved run proposals.
	Planner interface {
		Plan(context.Context, release.Target) (release.Plan, error)
	}
	// Delivery distinguishes known rejection from transport uncertainty.
	Delivery struct{ Status, URL string }
	// Writer posts one comment without automatic write retries.
	Writer interface {
		Write(context.Context, release.Proposal) (Delivery, error)
	}
)
