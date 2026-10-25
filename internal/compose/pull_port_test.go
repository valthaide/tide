package compose

import (
	"context"
	"testing"
	"time"
)

type oneSlot struct {
	sem     chan struct{}
	waiting chan struct{}
}

func (g *oneSlot) Acquire(ctx context.Context) error {
	select {
	case g.sem <- struct{}{}:
		return nil
	default:
	}
	if g.waiting != nil {
		select {
		case g.waiting <- struct{}{}:
		default:
		}
	}
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *oneSlot) Release() {
	select {
	case <-g.sem:
	default:
	}
}

func TestPullIdleStartsAfterGate(t *testing.T) {
	gate := &oneSlot{sem: make(chan struct{}, 1), waiting: make(chan struct{}, 1)}
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &ComposeExecutor{PullGate: gate}
	done := make(chan error, 1)
	go func() {
		_, release, _, err := e.beginPull(context.Background(), 200*time.Millisecond)
		if release != nil {
			release()
		}
		done <- err
	}()
	select {
	case <-gate.waiting:
	case <-time.After(time.Second):
		t.Fatal("second pull did not reach the queue")
	}
	select {
	case err := <-done:
		t.Fatalf("idle fired while waiting for the pull slot: %v", err)
	case <-time.After(450 * time.Millisecond):
	}
	gate.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pull did not start after the slot was free")
	}
}

func TestReleasePortsAllowsReuse(t *testing.T) {
	e := &ComposeExecutor{}
	if err := e.claimPorts("old", []int{59991}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.claimPorts("new", []int{59991}, nil); err == nil {
		t.Fatal("expected the removed project to still own the port")
	}
	e.ReleasePorts("old")
	if err := e.claimPorts("new", []int{59991}, nil); err != nil {
		t.Fatal(err)
	}
}
