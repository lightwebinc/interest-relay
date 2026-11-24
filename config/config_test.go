package config

import (
	"flag"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, env map[string]string, args ...string) (*Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("interest-relay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return parse(fs, args, func(k string) string { return env[k] })
}

func TestDefaults(t *testing.T) {
	cfg, err := load(t, nil, "-upstream", "[2001:db8::1]:8729", "-allow", "2001:db8:1::/64")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != netip.MustParseAddrPort("[::]:8729") {
		t.Errorf("listen %v", cfg.Listen)
	}
	if cfg.Lifetimes.Min != 30 || cfg.Lifetimes.Default != 300 || cfg.Lifetimes.Max != 3600 {
		t.Errorf("lifetimes %+v", cfg.Lifetimes)
	}
	if cfg.RestartHold >= 0 {
		t.Errorf("restart hold %s, want automatic", cfg.RestartHold)
	}
	if cfg.MaxHops != 16 || cfg.MetricsAddr != ":9600" || cfg.WithdrawOnExit || cfg.SenderLabel != "interest-relay" {
		t.Errorf("%+v", cfg)
	}
	if len(cfg.Upstreams) != 1 || len(cfg.Allow) != 1 {
		t.Errorf("upstreams %v allow %v", cfg.Upstreams, cfg.Allow)
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	env := map[string]string{
		"UPSTREAM":         "192.0.2.1:8729",
		"ALLOW":            "198.51.100.0/24",
		"LIFETIME_DEFAULT": "120s",
		"MAX_SENDERS":      "50",
		"WITHDRAW_ON_EXIT": "true",
	}
	cfg, err := load(t, env, "-lifetime-default", "90s", "-upstream", "192.0.2.1:8729, 192.0.2.2:8729,192.0.2.1:8729")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lifetimes.Default != 90 {
		t.Errorf("a flag must win over the environment: %d", cfg.Lifetimes.Default)
	}
	if cfg.MaxSenders != 50 || !cfg.WithdrawOnExit {
		t.Errorf("environment not applied: %+v", cfg)
	}
	if len(cfg.Upstreams) != 2 {
		t.Errorf("duplicate upstreams must collapse: %v", cfg.Upstreams)
	}
	if cfg.Allow[0] != netip.MustParsePrefix("198.51.100.0/24") {
		t.Errorf("allow %v", cfg.Allow)
	}
}

func TestAllowForms(t *testing.T) {
	cfg, err := load(t, nil, "-upstream", "192.0.2.1:8729", "-allow", "2001:db8::5, 192.0.2.77/24")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Allow[0] != netip.MustParsePrefix("2001:db8::5/128") {
		t.Errorf("a bare address is a prefix of one: %v", cfg.Allow[0])
	}
	if cfg.Allow[1] != netip.MustParsePrefix("192.0.2.0/24") {
		t.Errorf("host bits must be masked: %v", cfg.Allow[1])
	}
}

func TestRejects(t *testing.T) {
	base := []string{"-upstream", "192.0.2.1:8729", "-allow", "192.0.2.0/24"}
	cases := map[string][]string{
		"upstream is required":     {"-allow", "192.0.2.0/24"},
		"allow is required":        {"-upstream", "192.0.2.1:8729"},
		"need a unicast address":   {"-upstream", "[ff05::1]:8729", "-allow", "::/0"},
		"-allow":                   {"-upstream", "192.0.2.1:8729", "-allow", "not-a-prefix"},
		"-listen":                  append([]string{"-listen", "nonsense"}, base...),
		"fixed port":               append([]string{"-listen", "[::]:0"}, base...),
		"min <= default <= max":    append([]string{"-lifetime-min", "10m"}, base...),
		"whole seconds":            append([]string{"-lifetime-min", "1500ms"}, base...),
		"-max-hops":                append([]string{"-max-hops", "0"}, base...),
		"must not be negative":     append([]string{"-max-senders", "-1"}, base...),
		"-upstream-lifetime":       append([]string{"-upstream-lifetime", "-5s"}, base...),
		"-sender-label must not":   append([]string{"-sender-label", ""}, base...),
		"flag provided but not":    append([]string{"-no-such-flag"}, base...),
		"lifetimes are whole seco": append([]string{"-lifetime-max", "0s"}, base...),
	}
	for want, args := range cases {
		_, err := load(t, nil, args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: error %v, want it to mention %q", args, err, want)
		}
	}
}

func TestDurations(t *testing.T) {
	cfg, err := load(t, nil, "-upstream", "192.0.2.1:8729", "-allow", "192.0.2.0/24",
		"-restart-hold", "0s", "-upstream-lifetime", "10m")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RestartHold != 0 || cfg.UpstreamLifetime != 10*time.Minute {
		t.Errorf("hold %s, upstream lifetime %s", cfg.RestartHold, cfg.UpstreamLifetime)
	}
}

// The distributor is off unless -distribute-listen is set, and then needs
// the address the upstream reaches it at.
func TestDistributeFlags(t *testing.T) {
	base := []string{"-upstream", "[2001:db8::1]:8729", "-allow", "2001:db8:1::/64"}
	cfg, err := load(t, nil, base...)
	if err != nil || cfg.DistributeListen != "" {
		t.Fatalf("off by default: %+v %v", cfg, err)
	}
	cfg, err = load(t, map[string]string{"DISTRIBUTE_QUEUE_MIB": "8"}, append(base,
		"-distribute-listen", "[::]:9171", "-distribute-advertise", "[2001:db8:1::5]:9171", "-distribute-max-object-mib", "4")...)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DistributeListen != "[::]:9171" || cfg.DistributeAdvertise != netip.MustParseAddrPort("[2001:db8:1::5]:9171") ||
		cfg.DistributeQueueBytes != 8<<20 || cfg.DistributeMaxRecord != 36+4<<20 || cfg.DistributeQueue < 1 {
		t.Fatalf("parsed %+v", cfg)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-distribute-listen", "[::]:9171"}, "-distribute-advertise"},
		{[]string{"-distribute-listen", "[::]:9171", "-distribute-advertise", "[::]:9171"}, "-distribute-advertise"},
		{[]string{"-distribute-advertise", "[2001:db8:1::5]:9171"}, "without -distribute-listen"},
		{[]string{"-distribute-listen", "[::]:9171", "-distribute-advertise", "[2001:db8:1::5]:9171", "-distribute-queue", "0"}, "at least 1"},
	} {
		if _, err := load(t, nil, append(base, c.args...)...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err %v, want it to mention %q", c.args, err, c.want)
		}
	}
}

func TestPublish(t *testing.T) {
	base := []string{"-upstream", "[2001:db8::a]:8729", "-allow", "2001:db8:1::/64",
		"-distribute-listen", "[::]:9171", "-distribute-advertise", "[2001:db8:1::5]:9171"}
	cfg, err := load(t, nil, append(base, "-publish-listen", "[::]:8725",
		"-publish-ingress", "[2001:db8::a]:8725, [2001:db8::b]:8725", "-publish-source", "2001:db8:1::5")...)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublishListen != "[::]:8725" || len(cfg.PublishIngress) != 2 || cfg.PublishSource != netip.MustParseAddr("2001:db8:1::5") {
		t.Fatalf("%+v", cfg)
	}
	if cfg.PublishMaxObject != 8<<20 || cfg.PublishObjects != 2000 || cfg.PublishBytes != 80<<20 || cfg.DeliverTopics != 3 {
		t.Fatalf("defaults %+v", cfg)
	}
	for name, args := range map[string][]string{
		"no ingress":     append(base, "-publish-listen", "[::]:8725", "-publish-source", "2001:db8:1::5"),
		"no source":      append(base, "-publish-listen", "[::]:8725", "-publish-ingress", "[2001:db8::a]:8725"),
		"no distributor": {"-upstream", "[2001:db8::a]:8729", "-allow", "2001:db8:1::/64", "-publish-listen", "[::]:8725", "-publish-ingress", "[2001:db8::a]:8725", "-publish-source", "2001:db8:1::5"},
		"no listen":      append(base, "-publish-source", "2001:db8:1::5"),
		"bad ingress":    append(base, "-publish-listen", "[::]:8725", "-publish-ingress", "2001:db8::a", "-publish-source", "2001:db8:1::5"),
		"deliver 0":      append(base, "-deliver-topics", "0"),
	} {
		if _, err := load(t, nil, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
