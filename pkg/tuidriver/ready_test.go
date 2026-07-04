package tuidriver

import (
	"context"
	"testing"
	"time"
)

func TestWaitReady(t *testing.T) {
	idle := []byte(idleGlyphTest + " ") // ❯ present, no spinner → IsIdle

	tests := []struct {
		name string
		snap []byte
		want Readiness
	}{
		{
			name: "clean idle",
			snap: idle,
			want: Readiness{Idle: true},
		},
		{
			name: "trust modal at idle",
			snap: append([]byte("Quicksafetycheck"), idle...),
			want: Readiness{Idle: true, TrustModal: true},
		},
		{
			name: "mcp failure banner at idle",
			snap: append([]byte("2 MCP servers failed "), idle...),
			want: Readiness{Idle: true, McpFailure: true, FailedMcpCount: 2},
		},
		{
			name: "network failure at idle",
			snap: append([]byte("FailedToOpenSocket "), idle...),
			want: Readiness{Idle: true, NetworkFailure: true},
		},
		{
			// A recognized modal that no other Readiness field surfaces (here a
			// permission prompt) sets UnknownModal.
			name: "unrecognized modal at idle",
			snap: append([]byte("Doyouwanttoproceed"), idle...),
			want: Readiness{Idle: true, UnknownModal: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{buffer: NewBuffer(0)}
			s.buffer.Append(tt.snap)
			got, err := s.WaitReady(context.Background())
			if err != nil {
				t.Fatalf("WaitReady: %v", err)
			}
			if got != tt.want {
				t.Errorf("WaitReady = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWaitReadyContextCancelled(t *testing.T) {
	s := &Session{buffer: NewBuffer(0)} // never idle
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, err := s.WaitReady(ctx)
	if err == nil {
		t.Fatal("WaitReady = nil error, want ctx error")
	}
	if (got != Readiness{}) {
		t.Errorf("WaitReady = %+v on error, want zero Readiness", got)
	}
}
