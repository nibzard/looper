package ui

import (
	"strings"
	"testing"
)

func TestDashboardBeforeFirstSnapshot(t *testing.T) {
	m := model{width: 80, height: 24}
	view := m.View()
	if !strings.Contains(view, "Waiting for the controller") {
		t.Fatalf("missing waiting state: %s", view)
	}
	// The same model must render the first snapshot when it arrives.
	m.snap = &ViewSnapshot{}
	_ = m.View()
}
