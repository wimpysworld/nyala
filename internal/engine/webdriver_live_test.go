//go:build live

package engine

import (
	"testing"

	"github.com/chromedp/chromedp"
)

func TestLiveNavigatorWebdriverIsFalse(t *testing.T) {
	_, ctx := liveEngineOn(t, liveTestPage)

	var webdriverIsFalse bool
	if err := chromedp.Run(ctx, chromedp.Evaluate("navigator.webdriver === false", &webdriverIsFalse)); err != nil {
		t.Fatalf("read navigator.webdriver: %v", err)
	}
	if !webdriverIsFalse {
		t.Fatal("navigator.webdriver is not false")
	}
}
