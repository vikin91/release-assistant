// Package app coordinates one in-memory release session.
package app

import (
	"context"
	"release-assistant/internal/release"
)

type (
	// DiscoveryRequest scopes reads and preserves ignored and previously selected targets.
	DiscoveryRequest struct {
		Source           release.Source
		ConfirmedTag     string
		Tracked, Ignored []release.Target
		PinningPR        int
		Version          string
	}
	// Discoverer returns partial observations alongside errors safe for diagnostic display.
	Discoverer interface {
		Discover(context.Context, DiscoveryRequest) (release.Repository, error)
	}
	// Planner delegates all retry eligibility to its adapter.
	Planner interface {
		Plan(context.Context, release.Target) (release.Plan, error)
	}
	// Inspector checks release images and reported build outputs without source reads.
	Inspector interface {
		Inspect(context.Context, release.Config, map[string]string) (release.Inventory, error)
		InspectReference(context.Context, string) (release.Image, error)
	}
)
