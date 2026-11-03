package main

import (
	"encoding/json"
	"testing"
)

func TestUnifiedDelayConfigAndMigration(t *testing.T) {
	c := defaults()
	if err := json.Unmarshal([]byte("{}"), &c); err != nil {
		t.Fatal(err)
	}
	if !c.UnifiedDelay || buildCoreConfig(c, "")["unified-delay"] != true {
		t.Fatal("existing config should default to unified delay")
	}
	previous := c
	c.UnifiedDelay = false
	if buildCoreConfig(c, "")["unified-delay"] != false {
		t.Fatal("disabled setting not passed to core")
	}
	if sameCore(previous, c) {
		t.Fatal("changing measurement mode must restart core and clear old samples")
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	loaded := defaults()
	if err := json.Unmarshal(b, &loaded); err != nil || loaded.UnifiedDelay {
		t.Fatal("disabled setting must persist")
	}
}
