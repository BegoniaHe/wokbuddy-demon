package main

import "testing"

func TestNotifyConfig(t *testing.T) {
	c := Default()
	c.Notify.WebhookURL = " https://example.com/hook "
	c.Notify.Events = []string{"session_dead", " credit_low ", "session_dead"}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Notify.WebhookURL != "https://example.com/hook" {
		t.Fatalf("url=%q", c.Notify.WebhookURL)
	}
	if len(c.Notify.Events) != 2 || c.Notify.Events[1] != "credit_low" {
		t.Fatalf("events=%v", c.Notify.Events)
	}
}

func TestNotifyConfigRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ev   []string
	}{
		{"非 http(s) URL", "ftp://example.com/hook", nil},
		{"缺 host", "https://", nil},
		{"未知事件", "", []string{"nope"}},
	} {
		c := Default()
		c.Notify.WebhookURL = tc.url
		c.Notify.Events = tc.ev
		if err := c.normalize(); err == nil {
			t.Errorf("%s: normalize 应报错", tc.name)
		}
	}
}
