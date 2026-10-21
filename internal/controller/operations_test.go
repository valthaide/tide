package controller

import "testing"

func TestOperationStoreIdempotentStart(t *testing.T) {
	s := NewOperationStore()
	op1, existing := s.StartReconcile("app-1", "demo")
	if existing || op1.ID == "" || op1.Status != OpPending {
		t.Fatalf("unexpected first start: %+v existing=%v", op1, existing)
	}
	op2, existing := s.StartReconcile("app-1", "demo")
	if !existing || op2.ID != op1.ID {
		t.Fatalf("expected same operation, got %+v existing=%v", op2, existing)
	}
	s.MarkRunning(op1.ID)
	op3, existing := s.StartReconcile("app-1", "demo")
	if !existing || op3.ID != op1.ID || op3.Status != OpRunning {
		t.Fatalf("expected running op reused: %+v", op3)
	}
	s.Finish(op1.ID, OpSucceeded, "", "", "")
	op4, existing := s.StartReconcile("app-1", "demo")
	if existing || op4.ID == op1.ID {
		t.Fatalf("expected new operation after finish: %+v existing=%v", op4, existing)
	}
}
