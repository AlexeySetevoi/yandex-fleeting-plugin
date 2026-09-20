package yandex

import (
	"testing"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func TestMapStatus(t *testing.T) {
	tests := map[string]provider.State{
		"PROVISIONING": provider.StateCreating,
		"STARTING":     provider.StateCreating,
		"RESTARTING":   provider.StateCreating,
		"RUNNING":      provider.StateRunning,
		"UPDATING":     provider.StateRunning,
		"STOPPING":     provider.StateDeleting,
		"DELETING":     provider.StateDeleting,
		"STOPPED":      provider.StateTimeout,
		"CRASHED":      provider.StateTimeout,
		"ERROR":        provider.StateTimeout,
	}

	for status, want := range tests {
		got, ok := MapStatus(status)
		if !ok || got != want {
			t.Errorf("MapStatus(%q) = %q, %v, want %q", status, got, ok, want)
		}
	}

	if _, ok := MapStatus("STATUS_UNSPECIFIED"); ok {
		t.Error("MapStatus(STATUS_UNSPECIFIED) reported ok")
	}
}

// Новый статус в API должен попасться здесь, а не молча пропускаться в Update.
func TestMapStatusCoversAPIEnum(t *testing.T) {
	for value, name := range compute.Instance_Status_name {
		if value == int32(compute.Instance_STATUS_UNSPECIFIED) {
			continue
		}
		if _, ok := MapStatus(name); !ok {
			t.Errorf("instance status %s is not mapped", name)
		}
	}
}
