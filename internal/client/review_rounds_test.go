package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func roundInt(n int64) *int64 { return &n }
func roundControlSample() client.ReviewRoundControl {
	return client.ReviewRoundControl{ProjectID: "project-one", PullNumber: 2, Channel: "cli", Configured: true, Revision: 4, Epoch: 2, Closed: true, Minimum: 2, Maximum: 8, Taper: 3, Used: 4, Dry: 1, ActorID: "principal-one", CreatedAt: "2026-10-18T01:00:00Z", UpdatedAt: "2026-10-18T00:59:00Z"}
}
func TestReviewRoundsReadStrictBindingAndShape(t *testing.T) {
	for _, mode := range []string{"configured", "unconfigured", "foreign", "wrong-pull", "wrong-channel", "missing", "unknown", "null", "invalid-taper", "invalid-counts", "unconfigured-progress", "invalid-latch", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/rounds/2/cli" || r.URL.RawQuery != "" || r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("lost bound read")
				}
				value := roundControlSample()
				switch mode {
				case "unconfigured", "unconfigured-progress":
					value = client.ReviewRoundControl{ProjectID: "project-one", PullNumber: 2, Channel: "cli"}
					if mode == "unconfigured-progress" {
						value.Dry = 1
					}
				case "foreign":
					value.ProjectID = "other"
				case "wrong-pull":
					value.PullNumber = 3
				case "wrong-channel":
					value.Channel = "hosted"
				case "invalid-taper":
					value.Taper = 1
				case "invalid-counts":
					value.Dry = 5
				case "invalid-latch":
					value.Tapered = true
				}
				raw, _ := json.Marshal(value)
				switch mode {
				case "missing":
					var m map[string]any
					_ = json.Unmarshal(raw, &m)
					delete(m, "closed")
					raw, _ = json.Marshal(m)
				case "unknown":
					raw = append(raw[:len(raw)-1], []byte(`,"extra":false}`)...)
				case "null":
					raw = []byte(`null`)
				case "oversized":
					raw = []byte(strings.Repeat(" ", 64<<10) + string(raw))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			got, err := c.ReviewRoundControl(context.Background(), 2, "cli")
			if mode == "configured" || mode == "unconfigured" {
				if err != nil || got.Configured != (mode == "configured") {
					t.Fatal(got, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}

func TestReviewRoundsMutationValidationAndUnknownResponse(t *testing.T) {
	var calls atomic.Int64
	var mode atomic.Value
	mode.Store("valid")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/instance") {
			handshake(w)
			return
		}
		calls.Add(1)
		currentMode := mode.Load().(string)
		if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "round-key" || r.Header.Get("X-FC-Worker") != "worker-one" || r.URL.RawQuery != "" {
			t.Error("mutation binding changed")
		}
		var request client.ConfigureReviewRoundControlRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.ExpectedRevision == nil || *request.ExpectedRevision != 3 {
			t.Error("bad mutation body")
		}
		outcome := "granted"
		if request.Action == "close" {
			outcome = "closed"
			if request.Minimum != nil || request.Maximum != nil {
				t.Error("close included grant bounds")
			}
		}
		receipt := client.ReviewRoundControlReceipt{PullNumber: 2, Channel: "cli", Revision: 4, Epoch: 2, Outcome: outcome}
		switch currentMode {
		case "foreign":
			receipt.PullNumber = 3
		case "revision":
			receipt.Revision = 3
		case "outcome":
			receipt.Outcome = "closed"
		case "lost":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		raw, _ := json.Marshal(receipt)
		if currentMode == "missing" {
			raw = []byte(`{"pull_number":2,"channel":"cli","revision":4,"outcome":"granted"}`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	c := newClient(t, srv)
	grant := client.ConfigureReviewRoundControlRequest{ExpectedRevision: roundInt(3), Action: "grant", Minimum: roundInt(0), Maximum: roundInt(8)}
	invalid := []client.ConfigureReviewRoundControlRequest{{}, {ExpectedRevision: roundInt(-1), Action: "close"}, {ExpectedRevision: roundInt(0), Action: "grant", Maximum: roundInt(8)}, {ExpectedRevision: roundInt(0), Action: "grant", Minimum: roundInt(2), Maximum: roundInt(1)}, {ExpectedRevision: roundInt(0), Action: "grant", Minimum: roundInt(0), Maximum: roundInt(1001)}, {ExpectedRevision: roundInt(0), Action: "close", Minimum: roundInt(0)}}
	for _, input := range invalid {
		_, err := c.ConfigureReviewRoundControl(context.Background(), 2, "cli", "round-key", input)
		if err == nil {
			t.Fatal("accepted invalid request")
		}
	}
	for _, target := range []struct {
		pull         int64
		channel, key string
	}{{0, "cli", "round-key"}, {2, "other", "round-key"}, {2, "cli", ""}} {
		_, err := c.ConfigureReviewRoundControl(context.Background(), target.pull, target.channel, target.key, grant)
		if err == nil {
			t.Fatal("accepted invalid target")
		}
	}
	for _, target := range []struct {
		pull    int64
		channel string
	}{{0, "cli"}, {2147483648, "cli"}, {2, "bad"}} {
		_, err := c.ReviewRoundControl(context.Background(), target.pull, target.channel)
		if err == nil {
			t.Fatal("invalid read target accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached HTTP")
	}
	for _, m := range []string{"valid", "foreign", "revision", "outcome", "missing", "lost"} {
		mode.Store(m)
		before := calls.Load()
		got, err := c.ConfigureReviewRoundControl(context.Background(), 2, "cli", "round-key", grant)
		if m == "valid" {
			if err != nil || got.Outcome != "granted" {
				t.Fatal(got, err)
			}
		} else {
			var failure *client.Error
			if !errors.As(err, &failure) || !failure.OutcomeUnknown {
				t.Fatal("lost mutation certainty", m, err)
			}
		}
		if calls.Load() != before+1 {
			t.Fatal("mutation retried")
		}
	}
	mode.Store("valid")
	_, err := c.ConfigureReviewRoundControl(context.Background(), 2, "cli", "round-key", client.ConfigureReviewRoundControlRequest{ExpectedRevision: roundInt(3), Action: "close"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls.Load()
	_, err = c.ConfigureReviewRoundControl(ctx, 2, "cli", "round-key", grant)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.OutcomeUnknown || calls.Load() != before {
		t.Fatal("unsent cancellation became uncertain", err)
	}
}

func TestReviewRoundsReadUnboundedSafeCounters(t *testing.T) {
	for _, mode := range []string{"unbounded", "wrong-placeholder", "unsafe-used", "unproven-unbounded", "null-flag"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				c := roundControlSample()
				c.Closed = false
				c.MaximumUnbounded = true
				c.Maximum = 0
				c.Minimum = 9007199254740991
				c.Used = 9007199254740990
				c.Dry = 0
				c.OriginSource = "source"
				c.OriginKey = "2-cli"
				c.OriginSHA256 = strings.Repeat("a", 64)
				switch mode {
				case "wrong-placeholder":
					c.Maximum = 5
				case "unsafe-used":
					c.Used = 9007199254740992
				case "unproven-unbounded":
					c.OriginSource = ""
					c.OriginKey = ""
					c.OriginSHA256 = ""
				}
				raw, _ := json.Marshal(c)
				if mode == "null-flag" {
					raw = []byte(strings.Replace(string(raw), `"maximum_unbounded":true`, `"maximum_unbounded":null`, 1))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			c, err := newClient(t, server).ReviewRoundControl(context.Background(), 2, "cli")
			if mode == "unbounded" {
				if err != nil || !c.MaximumUnbounded || c.Maximum != 0 {
					t.Fatal("unbounded read lost", c, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}

func TestReviewRoundsCumulativeFiniteBoundsAndAcceptedReset(t *testing.T) {
	for _, mode := range []string{"cumulative", "safe-edge", "accepted-reset", "legacy-carried", "unsafe-maximum", "minimum-over-maximum", "used-over-maximum", "unproven-taper"} {
		t.Run(mode, func(t *testing.T) {
			value := roundControlSample()
			value.Closed = false
			value.Minimum, value.Maximum, value.Used, value.Dry = 1001, 2000, 1000, 2
			switch mode {
			case "safe-edge":
				value.Minimum, value.Maximum, value.Used, value.Dry = 9007199254740990, 9007199254740991, 9007199254740990, 0
			case "accepted-reset", "legacy-carried":
				value.OriginSource, value.OriginKey, value.OriginSHA256 = "original-controller", "2-cli", strings.Repeat("a", 64)
				value.InitiallyTapered = true
				value.InitialDry = 2
				value.Minimum, value.Used, value.Dry = 1, 1, 0
				if mode == "legacy-carried" {
					value.Tapered = true
				}
			case "unsafe-maximum":
				value.Maximum = 9007199254740992
			case "minimum-over-maximum":
				value.Minimum = value.Maximum + 1
			case "used-over-maximum":
				value.Used = value.Maximum + 1
			case "unproven-taper":
				value.Minimum = 0
				value.Dry = 0
				value.Tapered = true
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			got, err := newClient(t, server).ReviewRoundControl(context.Background(), 2, "cli")
			switch mode {
			case "cumulative", "safe-edge", "accepted-reset", "legacy-carried":
				if err != nil || got != value {
					t.Fatal("lost valid cumulative or carried control", got, err)
				}
			default:
				checkError(t, err, "invalid_response")
			}
		})
	}
}
