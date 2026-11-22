package metrics

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/lightwebinc/interest-relay/relay"
)

func newRelay(t *testing.T) *relay.Relay {
	t.Helper()
	r, err := relay.New(relay.Config{
		Upstreams: []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:8729")},
		Allow:     []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		SenderID:  7,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

// Every series must exist from the first scrape, at zero, so that a rate
// over it is defined before anything has happened.
func TestMetricsPresentAtZero(t *testing.T) {
	h := Handler(newRelay(t), nil)
	code, body := get(t, h, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		`bir_build_info{version="dev"} 1`,
		`bir_datagrams_total 0`,
		`bir_datagrams_ignored_total{reason="unidentified"} 0`,
		`bir_messages_total{outcome="hop_limit"} 0`,
		`bir_topic_changes_total{change="expire"} 0`,
		`bir_topics_refused_total{reason="not_served"} 0`,
		`bir_topics_live 0`,
		`bir_topics_wanted 0`,
		`bir_topics_unserved 0`,
		`bir_hops 1`,
		`bir_ready 0`,
		`bir_upstream_reachable{upstream="192.0.2.1:8729"} 0`,
		`bir_upstream_synced{upstream="192.0.2.1:8729"} 0`,
		`bir_upstream_datagrams_sent_total{type="digest",upstream="192.0.2.1:8729"} 0`,
		`bir_upstream_events_total{event="timeout",upstream="192.0.2.1:8729"} 0`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestHealthAndState(t *testing.T) {
	h := Handler(newRelay(t), nil)
	if code, _ := get(t, h, "/healthz"); code != http.StatusOK {
		t.Errorf("healthz %d", code)
	}
	if code, _ := get(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz %d for a relay whose upstream has not answered", code)
	}
	code, body := get(t, h, "/state")
	if code != http.StatusOK {
		t.Fatalf("state %d", code)
	}
	var st State
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("state is not JSON: %v", err)
	}
	if st.Ready || st.Hops != 1 || len(st.Upstreams) != 1 || st.Upstreams[0].Scope != "unknown" || st.Senders == nil {
		t.Errorf("state %+v", st)
	}
	if code, _ := get(t, h, "/loglevel"); code != http.StatusNotFound {
		t.Errorf("loglevel %d without a level to control", code)
	}
}

func TestFormat(t *testing.T) {
	if got := formatID(0x1A2B); got != "0000000000001a2b" {
		t.Errorf("id %s", got)
	}
	if formatCount(1) != "1 group" || formatCount(12) != "12 groups" {
		t.Errorf("counts %s %s", formatCount(1), formatCount(12))
	}
}
