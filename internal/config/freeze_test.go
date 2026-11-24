package config

import (
	"testing"
	"time"
)

func TestWeeklyFreezeAcrossWeekend(t *testing.T) {
	c := Default()
	c.Freezes = []Freeze{{Name: "weekend", Envs: []string{"prod"}, Weekly: &Weekly{From: "fri 16:00", To: "mon 08:00", Timezone: "Asia/Dubai"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	dubai, _ := time.LoadLocation("Asia/Dubai")
	cases := []struct {
		at     time.Time
		active bool
	}{
		{time.Date(2026, 11, 27, 15, 59, 0, 0, dubai), false}, // Friday before
		{time.Date(2026, 11, 27, 16, 0, 0, 0, dubai), true},   // Friday start
		{time.Date(2026, 11, 29, 12, 0, 0, 0, dubai), true},   // Sunday
		{time.Date(2026, 11, 30, 7, 59, 0, 0, dubai), true},   // Monday early
		{time.Date(2026, 11, 30, 8, 0, 0, 0, dubai), false},   // Monday end
		{time.Date(2026, 11, 25, 12, 0, 0, 0, dubai), false},  // Wednesday
	}
	for _, tc := range cases {
		f, until, on := c.ActiveFreeze(tc.at, "prod", "checkout-api")
		if on != tc.active {
			t.Errorf("%v: active=%v want %v", tc.at, on, tc.active)
		}
		if on && (f.Name != "weekend" || !until.Equal(time.Date(2026, 11, 30, 8, 0, 0, 0, dubai))) {
			t.Errorf("%v: until=%v", tc.at, until.In(dubai))
		}
	}
	if _, _, on := c.ActiveFreeze(time.Date(2026, 11, 29, 12, 0, 0, 0, dubai), "staging", "x"); on {
		t.Error("staging is not frozen")
	}
}

func TestOneOffFreeze(t *testing.T) {
	c := Default()
	c.Freezes = []Freeze{{Name: "year-end", Services: []string{"payments-api"}, Start: "2026-12-20T00:00:00Z", End: "2027-01-03T00:00:00Z"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	in := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
	if _, _, on := c.ActiveFreeze(in, "prod", "payments-api"); !on {
		t.Error("should be frozen")
	}
	if _, _, on := c.ActiveFreeze(in, "prod", "cart"); on {
		t.Error("cart is not in the freeze")
	}
	if _, _, on := c.ActiveFreeze(time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC), "prod", "payments-api"); on {
		t.Error("end is exclusive")
	}
}

func TestFreezeValidation(t *testing.T) {
	for _, f := range []Freeze{
		{Name: "x"},
		{Name: "x", Start: "2026-12-20", End: "2027-01-03"},
		{Name: "x", Start: "2027-01-03T00:00:00Z", End: "2026-12-20T00:00:00Z"},
		{Name: "x", Weekly: &Weekly{From: "friday 4pm", To: "mon 08:00"}},
		{Name: "x", Weekly: &Weekly{From: "fri 16:00", To: "mon 08:00", Timezone: "Mars/Olympus"}},
	} {
		c := Default()
		c.Freezes = []Freeze{f}
		if err := c.Validate(); err == nil {
			t.Errorf("%+v should be invalid", f)
		}
	}
}
