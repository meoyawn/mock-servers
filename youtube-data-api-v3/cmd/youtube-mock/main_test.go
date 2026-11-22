package main

import "testing"

func TestLoadConfigFromEnvironment(t *testing.T) {
	t.Setenv("YOUTUBE_MOCK_SERVER__ADDRESS", "127.0.0.1:19090")
	t.Setenv("YOUTUBE_MOCK_STATE__PROCESSING_POLLS", "3")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != "127.0.0.1:19090" || cfg.ProcessingPolls != 3 {
		t.Fatalf("loaded config = %#v", cfg)
	}
}
