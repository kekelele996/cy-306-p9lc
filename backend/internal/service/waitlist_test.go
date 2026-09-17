package service

import (
	"testing"

	"gbevent/internal/constants"
	"gbevent/internal/model"
)

func TestHasFreeCapacity(t *testing.T) {
	cases := []struct {
		name     string
		capacity int
		occupied int64
		want     bool
	}{
		{"unlimited always free", 0, 99999, true},
		{"zero occupied", 2, 0, true},
		{"one seat left", 2, 1, true},
		{"exactly full", 2, 2, false},
		{"over capacity", 2, 3, false},
	}
	for _, tc := range cases {
		a := &model.Activity{Capacity: tc.capacity}
		if got := HasFreeCapacity(a, tc.occupied); got != tc.want {
			t.Errorf("%s: HasFreeCapacity(cap=%d, occupied=%d) = %v, want %v",
				tc.name, tc.capacity, tc.occupied, got, tc.want)
		}
	}
}

// TestWaitlistStatusConstants 候补/正式状态常量的稳定性是全链路（计数 SQL、前后端枚举）的前提。
func TestWaitlistStatusConstants(t *testing.T) {
	if constants.RegistrationStatusWaitlisted != "waitlisted" {
		t.Fatalf("waitlisted status changed: %q", constants.RegistrationStatusWaitlisted)
	}
	if !constants.IsValidRegistrationStatus(constants.RegistrationStatusWaitlisted) {
		t.Fatal("waitlisted must be a valid registration status")
	}
	if constants.NotificationWaitlistJoined != "waitlist_joined" ||
		constants.NotificationWaitlistPromoted != "waitlist_promoted" {
		t.Fatal("waitlist notification type constants changed")
	}
}
