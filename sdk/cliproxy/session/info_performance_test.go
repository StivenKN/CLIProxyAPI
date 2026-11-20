package session

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// fastestRatio times measured and baseline alternately and compares their
// fastest runs. Machine load only adds time to a run, so the fastest runs stay
// stable on a busy machine, while rescanning the payload per candidate key
// still multiplies the measured cost.
func fastestRatio(measured, baseline func()) (ratio float64, fastestMeasured, fastestBaseline time.Duration) {
	const samples = 200
	for i := 0; i < samples; i++ {
		start := time.Now()
		baseline()
		if d := time.Since(start); i == 0 || d < fastestBaseline {
			fastestBaseline = d
		}
		start = time.Now()
		measured()
		if d := time.Since(start); i == 0 || d < fastestMeasured {
			fastestMeasured = d
		}
	}
	return float64(fastestMeasured) / float64(fastestBaseline), fastestMeasured, fastestBaseline
}

func TestHasExplicitSessionDoesNotRescanLargePayload(t *testing.T) {
	payload := []byte(`{"messages":["` + strings.Repeat("x", 1<<20) + `"]}`)

	singleLookup := func() {
		root := util.ParseGJSONBytesNoCopy(payload)
		if root.Get("missing_session_id").Exists() {
			t.Fatal("unexpected session ID")
		}
	}
	detection := func() {
		if hasExplicitSession(nil, payload) {
			t.Fatal("unexpected explicit session")
		}
	}

	if ratio, got, base := fastestRatio(detection, singleLookup); ratio > 12 {
		t.Fatalf("explicit session detection rescans large payload: detection %v, single lookup %v (%.1fx)", got, base, ratio)
	}
}

func TestExtractSessionInfoDoesNotRescanLargePayload(t *testing.T) {
	payload := []byte(`{"messages":["` + strings.Repeat("x", 1<<20) + `"]}`)

	singleLookup := func() {
		root := util.ParseGJSONBytesNoCopy(payload)
		if root.Get("missing_session_id").Exists() {
			t.Fatal("unexpected session ID")
		}
	}
	extraction := func() {
		if info, ok := ExtractSessionInfo(nil, payload, nil); ok {
			t.Fatalf("unexpected session: %q", info.SessionID)
		}
	}

	// A request without session fields must not rescan a large message for every candidate key.
	if ratio, got, base := fastestRatio(extraction, singleLookup); ratio > 12 {
		t.Fatalf("session extraction rescans large payload: extraction %v, single lookup %v (%.1fx)", got, base, ratio)
	}
}
