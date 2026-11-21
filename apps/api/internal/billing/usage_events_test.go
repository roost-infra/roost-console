package billing

import (
	"errors"
	"strings"
	"testing"
)

func int64Ptr(v int64) *int64 { return &v }

func validUsageEvent() UsageEvent {
	return UsageEvent{
		Model:             "deepseek-v4-pro",
		Requests:          1,
		InputTokens:       33404,
		OutputTokens:      256,
		CacheReadTokens:   8192,
		CacheWriteTokens:  0,
		UpstreamCostMinor: int64Ptr(1616),
	}
}

func TestNormalizeUsageValid(t *testing.T) {
	in := validUsageEvent()
	got, err := NormalizeUsage(&in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("valid usage normalized to nil")
	}
	if got.Model != "deepseek-v4-pro" || got.Requests != 1 || got.InputTokens != 33404 ||
		got.OutputTokens != 256 || got.CacheReadTokens != 8192 || got.CacheWriteTokens != 0 {
		t.Fatalf("normalized=%+v", got)
	}
	if got.UpstreamCostMinor == nil || *got.UpstreamCostMinor != 1616 {
		t.Fatalf("upstream cost=%v", got.UpstreamCostMinor)
	}
	// The store keeps its own copy; the caller's object is not aliased.
	in.Model = "changed"
	in.InputTokens = 1
	if got.Model != "deepseek-v4-pro" || got.InputTokens != 33404 {
		t.Fatalf("normalized object aliases input: %+v", got)
	}
}

func TestNormalizeUsageAbsentOrZero(t *testing.T) {
	if got, err := NormalizeUsage(nil); err != nil || got != nil {
		t.Fatalf("absent: got=%v err=%v", got, err)
	}
	zeroCost := int64(0)
	for name, in := range map[string]UsageEvent{
		"all zero":             {},
		"model only":           {Model: "deepseek-v4-pro"},
		"zero cost pointer":    {UpstreamCostMinor: &zeroCost},
		"zero requests tokens": {Requests: 0},
	} {
		if got, err := NormalizeUsage(&in); err != nil || got != nil {
			t.Fatalf("%s: got=%v err=%v", name, got, err)
		}
	}
}

func TestNormalizeUsageDefaultsRequests(t *testing.T) {
	in := UsageEvent{InputTokens: 10}
	got, err := NormalizeUsage(&in)
	if err != nil || got == nil {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got.Requests != 1 {
		t.Fatalf("requests=%d want default 1", got.Requests)
	}

	// A request count on its own is real usage and is kept as sent.
	in = UsageEvent{Requests: 3}
	got, err = NormalizeUsage(&in)
	if err != nil || got == nil || got.Requests != 3 {
		t.Fatalf("requests-only got=%v err=%v", got, err)
	}

	// Cost-only usage is real usage too.
	in = UsageEvent{UpstreamCostMinor: int64Ptr(7)}
	got, err = NormalizeUsage(&in)
	if err != nil || got == nil || got.Requests != 1 || *got.UpstreamCostMinor != 7 {
		t.Fatalf("cost-only got=%v err=%v", got, err)
	}
}

func TestNormalizeUsageTrimsModel(t *testing.T) {
	in := UsageEvent{Model: "  deepseek-v4-pro  ", Requests: 1}
	got, err := NormalizeUsage(&in)
	if err != nil || got == nil {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got.Model != "deepseek-v4-pro" {
		t.Fatalf("model=%q", got.Model)
	}

	// Exactly 200 characters is accepted after trimming; 201 is rejected.
	body := strings.Repeat("m", UsageEventMaxModelLen)
	in = UsageEvent{Model: "  " + body + "  ", Requests: 1}
	if got, err := NormalizeUsage(&in); err != nil || got == nil || got.Model != body {
		t.Fatalf("200 chars: got=%v err=%v", got, err)
	}
	in = UsageEvent{Model: strings.Repeat("m", UsageEventMaxModelLen+1), Requests: 1}
	if _, err := NormalizeUsage(&in); !errors.Is(err, ErrInvalidUsage) {
		t.Fatalf("201 chars err=%v want ErrInvalidUsage", err)
	}
	// Unicode characters count as characters, not bytes.
	in = UsageEvent{Model: strings.Repeat("模", UsageEventMaxModelLen), Requests: 1}
	if got, err := NormalizeUsage(&in); err != nil || got == nil {
		t.Fatalf("200 multibyte chars: got=%v err=%v", got, err)
	}
	in = UsageEvent{Model: strings.Repeat("模", UsageEventMaxModelLen+1), Requests: 1}
	if _, err := NormalizeUsage(&in); !errors.Is(err, ErrInvalidUsage) {
		t.Fatalf("201 multibyte chars err=%v want ErrInvalidUsage", err)
	}
}

func TestNormalizeUsageRejectsNegative(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*UsageEvent)
	}{
		{"requests", func(u *UsageEvent) { u.Requests = -1 }},
		{"input", func(u *UsageEvent) { u.InputTokens = -1 }},
		{"output", func(u *UsageEvent) { u.OutputTokens = -1 }},
		{"cache read", func(u *UsageEvent) { u.CacheReadTokens = -1 }},
		{"cache write", func(u *UsageEvent) { u.CacheWriteTokens = -1 }},
		{"upstream cost", func(u *UsageEvent) { u.UpstreamCostMinor = int64Ptr(-1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := validUsageEvent()
			tc.mutate(&u)
			if _, err := NormalizeUsage(&u); !errors.Is(err, ErrInvalidUsage) {
				t.Fatalf("err=%v want ErrInvalidUsage", err)
			}
		})
	}
}

func TestUsageEventIsZero(t *testing.T) {
	if !(UsageEvent{}).IsZero() {
		t.Fatal("zero value is not zero")
	}
	zeroCost := int64(0)
	if !(UsageEvent{UpstreamCostMinor: &zeroCost}).IsZero() {
		t.Fatal("zero cost pointer is not zero")
	}
	u := validUsageEvent()
	if u.IsZero() {
		t.Fatal("nonzero usage reported as zero")
	}
	u = UsageEvent{Requests: 1}
	if u.IsZero() {
		t.Fatal("requests-only usage reported as zero")
	}
}

func TestNormalizeUsageSessionID(t *testing.T) {
	t.Parallel()
	u := &UsageEvent{Model: "m", SessionID: "  sess-1  ", InputTokens: 1}
	got, err := NormalizeUsage(u)
	if err != nil || got.SessionID != "sess-1" {
		t.Fatalf("session trim: got=%+v err=%v", got, err)
	}
	tooLong := &UsageEvent{Model: "m", SessionID: strings.Repeat("s", UsageEventMaxSessionLen+1), InputTokens: 1}
	if _, err := NormalizeUsage(tooLong); !errors.Is(err, ErrInvalidUsage) {
		t.Fatalf("over-long session err=%v", err)
	}
}
