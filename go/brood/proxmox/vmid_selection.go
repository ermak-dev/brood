package proxmox

import (
	"context"
	"errors"
	"fmt"
)

// VMIDRegistry supplies suggestions and exact global absence checks, not reservations.
// Client implements this read-only subset of the PVE API.
type VMIDRegistry interface {
	NextID(context.Context) (int, error)
	ConfirmAbsent(context.Context, int) error
}

// VMIDSelection is a pre-allocation observation, never durable permission to create.
// ID is nonzero only on success. ResumeAt is a read-only scan hint on budget
// exhaustion, or zero when the range is exhausted. Neither field reserves an ID.
type VMIDSelection struct {
	ID       int
	ResumeAt int
}

// SelectAvailableVMID selects only before allocation or Create submission. It skips
// every local reservation, including uncertain-effect and known-not-issued holds.
// Callers must serialize changes to reserved and perform their own admission;
// this function neither changes reservations nor authorizes release or Create retry.
//
// A nonzero hint starts the scan without NextID. Every candidate still requires
// a fresh successful exact global ConfirmAbsent. Only ErrCollision advances the
// scan; other errors stop it with ID zero. maxChecks must be positive and bounds
// remote confirmations (not local skips). A budget error returns the next unchecked
// unreserved candidate in ResumeAt; range exhaustion returns ResumeAt zero.
func SelectAvailableVMID(ctx context.Context, registry VMIDRegistry, reserved map[int]struct{}, hint int, maxChecks int) (VMIDSelection, error) {
	if err := ctx.Err(); err != nil {
		return VMIDSelection{}, err
	}
	if registry == nil {
		return VMIDSelection{}, errors.New("PVE VMID registry is required")
	}
	if maxChecks <= 0 {
		return VMIDSelection{}, errors.New("PVE VMID confirmation budget must be positive")
	}
	if hint != 0 && !validVMID(hint) {
		return VMIDSelection{}, errors.New("invalid PVE VMID scan hint")
	}
	candidate := hint
	if candidate == 0 {
		var err error
		candidate, err = registry.NextID(ctx)
		if err != nil {
			return VMIDSelection{}, err
		}
		if err := ctx.Err(); err != nil {
			return VMIDSelection{}, err
		}
		if !validVMID(candidate) {
			return VMIDSelection{}, errors.New("invalid PVE next VMID")
		}
	}
	checks := 0
	for validVMID(candidate) {
		if err := ctx.Err(); err != nil {
			return VMIDSelection{}, err
		}
		if _, held := reserved[candidate]; held {
			candidate++
			continue
		}
		if checks == maxChecks {
			return VMIDSelection{ResumeAt: candidate}, fmt.Errorf("PVE VMID confirmation budget exhausted after %d checks", checks)
		}
		checks++
		err := registry.ConfirmAbsent(ctx, candidate)
		if canceled := ctx.Err(); canceled != nil {
			return VMIDSelection{}, canceled
		}
		if err == nil {
			return VMIDSelection{ID: candidate}, nil
		}
		if !errors.Is(err, ErrCollision) {
			return VMIDSelection{}, err
		}
		// validVMID ограничивает candidate сверху 999999999, инкремент безопасен.
		candidate++
	}
	return VMIDSelection{}, errors.New("PVE VMID scan range exhausted")
}
