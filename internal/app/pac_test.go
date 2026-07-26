package app

import (
	"strings"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

func TestRenderPAC_ContainsListenProxyDirective(t *testing.T) {
	t.Parallel()
	pac := string(RenderPAC(port.PACInput{
		Listen:    "127.0.0.1:9090",
		Whitelist: []string{"*.a.com"},
		Blacklist: []string{"*.b.com"},
	}))
	// The listen address is injected as the LOCAL_PROXY var and used in
	// "PROXY " + LOCAL_PROXY expressions. Assert the address appears and the
	// PROXY directive form is present.
	if !strings.Contains(pac, `var LOCAL_PROXY = "127.0.0.1:9090"`) {
		t.Errorf("PAC missing LOCAL_PROXY assignment for 127.0.0.1:9090:\n%s", pac)
	}
	if !strings.Contains(pac, `"PROXY " + LOCAL_PROXY`) {
		t.Errorf("PAC missing PROXY + LOCAL_PROXY directive form:\n%s", pac)
	}
}

func TestRenderPAC_IsES3Subset(t *testing.T) {
	t.Parallel()
	pac := string(RenderPAC(port.PACInput{
		Listen:    "127.0.0.1:8080",
		Whitelist: []string{"*.a.com"},
		Blacklist: []string{"*.b.com"},
		Ranges:    port.RURanges{Ranges: []port.Range{{Low: 10, High: 20}}},
	}))

	forbidden := []string{
		"=>",        // arrow functions
		"`",         // template literals (backticks)
		" const ",   // const (with spaces to avoid substring in comments)
		" let ",     // let
		"\"",        // double-quoted strings are fine; this check is for backticks above
	}
	for _, f := range forbidden {
		if f == "\"" {
			continue // double quotes are valid ES3
		}
		if strings.Contains(pac, f) {
			t.Errorf("PAC contains forbidden ES3 token %q:\n%s", f, pac)
		}
	}
	// Sanity: must contain var and function declarations (ES3 idioms).
	if !strings.Contains(pac, "var ") {
		t.Errorf("PAC missing 'var' declarations:\n%s", pac)
	}
	if !strings.Contains(pac, "function ") {
		t.Errorf("PAC missing 'function' declarations:\n%s", pac)
	}
}

func TestRenderPAC_RURangesRendered(t *testing.T) {
	t.Parallel()
	pac := string(RenderPAC(port.PACInput{
		Listen: "127.0.0.1:8080",
		Ranges: port.RURanges{Ranges: []port.Range{{Low: 167772160, High: 184549375}}},
	}))
	// 1.2.3.5 = 16909057; if the range 167772160..184549375 is in RU_RANGES,
	// the harness test below will assert inRu. Here we just check the literal
	// pair appears in the rendered JS.
	if !strings.Contains(pac, "[167772160, 184549375]") {
		t.Errorf("PAC missing expected range [167772160, 184549375]:\n%s", pac)
	}
}

func TestRenderPAC_EmptyRanges(t *testing.T) {
	t.Parallel()
	pac := string(RenderPAC(port.PACInput{
		Listen: "127.0.0.1:8080",
	}))
	// Empty ranges must render as [] not [undefined] or null.
	if !strings.Contains(pac, "[]") {
		t.Errorf("PAC missing empty array for empty ranges:\n%s", pac)
	}
}

func TestRenderPAC_WhitelistBlacklistRendered(t *testing.T) {
	t.Parallel()
	pac := string(RenderPAC(port.PACInput{
		Listen:    "127.0.0.1:8080",
		Whitelist: []string{"*.force.com", "exact.example.org"},
		Blacklist: []string{"*.direct.io"},
	}))
	if !strings.Contains(pac, "*.force.com") {
		t.Errorf("PAC missing whitelist entry *.force.com:\n%s", pac)
	}
	if !strings.Contains(pac, "exact.example.org") {
		t.Errorf("PAC missing whitelist entry exact.example.org:\n%s", pac)
	}
	if !strings.Contains(pac, "*.direct.io") {
		t.Errorf("PAC missing blacklist entry *.direct.io:\n%s", pac)
	}
}
