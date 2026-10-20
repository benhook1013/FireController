package client

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReviewRoundControl is the scoped retained allowance and progress for one channel.
type ReviewRoundControl struct {
	ProjectID        string `json:"project_id"`
	PullNumber       int64  `json:"pull_number"`
	Channel          string `json:"channel"`
	Configured       bool   `json:"configured"`
	Revision         int64  `json:"revision"`
	Epoch            int64  `json:"epoch"`
	Closed           bool   `json:"closed"`
	Minimum          int64  `json:"minimum"`
	Maximum          int64  `json:"maximum"`
	MaximumUnbounded bool   `json:"maximum_unbounded,omitempty"`
	Taper            int64  `json:"taper"`
	Used             int64  `json:"used"`
	Dry              int64  `json:"dry"`
	Tapered          bool   `json:"tapered"`
	ActorID          string `json:"actor_id"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	InitialDry       int64  `json:"initial_dry,omitempty"`
	InitiallyTapered bool   `json:"initially_tapered,omitempty"`
	OriginSource     string `json:"origin_source,omitempty"`
	OriginKey        string `json:"origin_key,omitempty"`
	OriginSHA256     string `json:"origin_sha256,omitempty"`
}

type ConfigureReviewRoundControlRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	Action           string `json:"action"`
	Minimum          *int64 `json:"minimum,omitempty"`
	Maximum          *int64 `json:"maximum,omitempty"`
}

type ReviewRoundControlReceipt struct {
	PullNumber int64  `json:"pull_number"`
	Channel    string `json:"channel"`
	Revision   int64  `json:"revision"`
	Epoch      int64  `json:"epoch"`
	Outcome    string `json:"outcome"`
}

func ValidateReviewRoundControlRequest(r ConfigureReviewRoundControlRequest) error {
	if r.ExpectedRevision == nil || *r.ExpectedRevision < 0 || *r.ExpectedRevision == math.MaxInt64 {
		return mutationValidation()
	}
	switch r.Action {
	case "grant":
		if r.Minimum == nil || r.Maximum == nil || *r.Maximum < 1 || *r.Maximum > 1000 || *r.Minimum < 0 || *r.Minimum > *r.Maximum {
			return mutationValidation()
		}
	case "close":
		if r.Minimum != nil || r.Maximum != nil {
			return mutationValidation()
		}
	default:
		return mutationValidation()
	}
	return nil
}

func (c *Client) ReviewRoundControl(ctx context.Context, pull int64, channel string) (ReviewRoundControl, error) {
	var result ReviewRoundControl
	if !validPullNumber(pull) || !oneReviewOf(channel, "cli", "hosted") {
		return result, validation()
	}
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "rounds", strconv.FormatInt(pull, 10), channel), nil, true, "", nil, http.StatusOK, false, &result, 64<<10, decodeReviewResponse); err != nil {
		return ReviewRoundControl{}, err
	}
	if !c.validRoundControl(result, pull, channel) {
		return ReviewRoundControl{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) validRoundControl(r ReviewRoundControl, pull int64, channel string) bool {
	if r.ProjectID != c.identity.ProjectID || r.PullNumber != pull || r.Channel != channel {
		return false
	}
	if !r.Configured {
		return r == (ReviewRoundControl{ProjectID: r.ProjectID, PullNumber: pull, Channel: channel})
	}
	taper := int64(3)
	if channel == "hosted" {
		taper = 1
	}
	origin := r.OriginSource != "" || r.OriginKey != "" || r.OriginSHA256 != ""
	maximumMinimum := int64(1)
	if origin {
		text := func(s string) bool {
			return s != "" && s == strings.TrimSpace(s) && len(s) <= 200 && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
		}
		if !text(r.OriginSource) || !text(r.OriginKey) || !controlHex(r.OriginSHA256, 64) || r.InitialDry < 0 || r.InitialDry > 1000 || r.Taper < 1 || r.Taper > 1000 || r.InitialDry >= r.Taper && !r.InitiallyTapered {
			return false
		}
		maximumMinimum = 0
	} else if r.MaximumUnbounded || r.InitialDry != 0 || r.InitiallyTapered || r.Taper != taper {
		return false
	}
	return r.Revision >= 1 && r.Epoch >= 1 && r.Epoch <= r.Revision && r.Minimum >= 0 && r.Used >= 0 && r.Used <= 9007199254740991 && r.Dry >= 0 && r.Dry <= 9007199254740991 && r.Dry <= r.Used+r.InitialDry && (r.MaximumUnbounded && r.Maximum == 0 && r.Minimum <= 9007199254740991 || !r.MaximumUnbounded && r.Maximum >= maximumMinimum && r.Maximum <= 1000 && r.Minimum <= r.Maximum && r.Used <= r.Maximum) && (!r.Tapered || r.Used >= r.Minimum && (r.InitiallyTapered || r.Dry >= r.Taper)) && (!r.InitiallyTapered || r.Used < r.Minimum || r.Tapered) && validID(r.ActorID) && controlTime(r.CreatedAt) && controlTime(r.UpdatedAt)
}

func (c *Client) ConfigureReviewRoundControl(ctx context.Context, pull int64, channel, key string, input ConfigureReviewRoundControlRequest) (ReviewRoundControlReceipt, error) {
	var result ReviewRoundControlReceipt
	if !validPullNumber(pull) || !oneReviewOf(channel, "cli", "hosted") || !validMutationKey(key) {
		return result, mutationValidation()
	}
	if err := ValidateReviewRoundControlRequest(input); err != nil {
		return result, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return result, mutationValidation()
	}
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "rounds", strconv.FormatInt(pull, 10), channel), nil, true, key, body, http.StatusOK, true, &result, 64<<10, decodeReviewResponse); err != nil {
		return ReviewRoundControlReceipt{}, err
	}
	outcome := "granted"
	if input.Action == "close" {
		outcome = "closed"
	}
	if result.PullNumber != pull || result.Channel != channel || result.Revision != *input.ExpectedRevision+1 || result.Epoch < 1 || result.Epoch > result.Revision || result.Outcome != outcome {
		return ReviewRoundControlReceipt{}, unknownMutation("invalid_response", "The round-control receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}
