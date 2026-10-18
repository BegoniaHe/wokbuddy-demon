package main

import "testing"

func TestNotifyConfig(t *testing.T) {
	t.Parallel()

	config := Default()
	config.Notify.WebhookURL = " https://example.com/hook "

	config.Notify.Events = []string{"session_dead", " credit_low ", "session_dead"}
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if config.Notify.WebhookURL != "https://example.com/hook" {
		t.Fatalf("url=%q", config.Notify.WebhookURL)
	}

	if len(config.Notify.Events) != 2 || config.Notify.Events[1] != "credit_low" {
		t.Fatalf("events=%v", config.Notify.Events)
	}
}

func TestNotifyConfigRejectsInvalid(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		url  string
		ev   []string
	}{
		{"非 http(s) URL", "ftp://example.com/hook", nil},
		{"缺 host", "https://", nil},
		{"未知事件", "", []string{"nope"}},
	} {
		c := Default()
		c.Notify.WebhookURL = testCase.url

		c.Notify.Events = testCase.ev
		if err := c.normalize(); err == nil {
			t.Errorf("%s: normalize 应报错", testCase.name)
		}
	}
}
