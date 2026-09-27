package notify

import (
	"errors"
	"testing"
)

func TestGetSettings_DefaultsWhenNoRow(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	s, err := GetSettings(db.DB(), "svc")
	if err != nil {
		t.Fatalf("GetSettings error: %v", err)
	}
	if !s.Enabled {
		t.Errorf("Enabled = false, want true (default)")
	}
	if s.HeartbeatEnabled {
		t.Errorf("HeartbeatEnabled = true, want false (default)")
	}
	if s.MaxAgeHours != nil {
		t.Errorf("MaxAgeHours = %v, want nil (no override)", *s.MaxAgeHours)
	}
}

func TestSetSettings_RoundTrip(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	maxAge := 72
	want := Settings{Service: "svc", Enabled: false, MaxAgeHours: &maxAge, HeartbeatEnabled: true}
	if err := SetSettings(db.DB(), "svc", want); err != nil {
		t.Fatalf("SetSettings error: %v", err)
	}

	got, err := GetSettings(db.DB(), "svc")
	if err != nil {
		t.Fatalf("GetSettings error: %v", err)
	}
	if got.Enabled != want.Enabled || got.HeartbeatEnabled != want.HeartbeatEnabled {
		t.Errorf("GetSettings() = %+v, want %+v", got, want)
	}
	if got.MaxAgeHours == nil || *got.MaxAgeHours != maxAge {
		t.Errorf("MaxAgeHours = %v, want %d", got.MaxAgeHours, maxAge)
	}
}

func TestSetSettings_OverwritesPreviousValue(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	first := 24
	if err := SetSettings(db.DB(), "svc", Settings{Enabled: true, MaxAgeHours: &first}); err != nil {
		t.Fatalf("SetSettings error: %v", err)
	}
	if err := SetSettings(db.DB(), "svc", Settings{Enabled: true, MaxAgeHours: nil}); err != nil {
		t.Fatalf("SetSettings error: %v", err)
	}

	got, err := GetSettings(db.DB(), "svc")
	if err != nil {
		t.Fatalf("GetSettings error: %v", err)
	}
	if got.MaxAgeHours != nil {
		t.Errorf("MaxAgeHours = %v, want nil (cleared override)", *got.MaxAgeHours)
	}
}

func TestSetSettings_RejectsNonPositiveMaxAge(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	for _, v := range []int{0, -5} {
		v := v
		if err := SetSettings(db.DB(), "svc", Settings{MaxAgeHours: &v}); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("SetSettings with max_age_hours=%d = %v, want ErrInvalidSettings", v, err)
		}
	}
}
