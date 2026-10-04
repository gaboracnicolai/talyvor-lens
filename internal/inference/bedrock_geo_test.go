package inference

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// B27.10 — every Region on AWS's Haiku 4.5 model card, and the profile Lens must send from it. Transcribed
// from docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-claude-haiku-4-5.html (fetched
// 2026-10-04): the "Geo inference details" tables give the us/eu/au/jp/in source Regions; every other
// Region in the bedrock-runtime availability table has only the global profile.
var haiku45PublishedProfiles = map[string]string{
	"us-east-1": "us", "us-east-2": "us", "us-west-1": "us", "us-west-2": "us", "ca-central-1": "us",
	"eu-central-1": "eu", "eu-central-2": "eu", "eu-north-1": "eu", "eu-south-1": "eu",
	"eu-south-2": "eu", "eu-west-1": "eu", "eu-west-2": "eu", "eu-west-3": "eu",
	"ap-southeast-2": "au", "ap-southeast-4": "au", "ap-southeast-6": "au",
	"ap-northeast-1": "jp", "ap-northeast-3": "jp",
	"ap-south-1": "in", "ap-south-2": "in",
	"ca-west-1": "global", "sa-east-1": "global", "mx-central-1": "global",
	"il-central-1": "global", "me-central-1": "global", "me-south-1": "global", "af-south-1": "global",
	"ap-east-2": "global", "ap-northeast-2": "global", "ap-southeast-1": "global",
	"ap-southeast-3": "global", "ap-southeast-5": "global", "ap-southeast-7": "global",
}

func TestBedrockModelIDForRegion_Haiku45UsesTheGeoProfileAWSPublishes(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	globalOnlyLogged.Range(func(k, _ any) bool { globalOnlyLogged.Delete(k); return true })

	for region, profile := range haiku45PublishedProfiles {
		want := profile + ".anthropic.claude-haiku-4-5-20251001-v1:0"
		got, ok := BedrockModelIDForRegion("claude-haiku-4-5", region)
		if !ok || got != want {
			t.Errorf("region %s: got %q (ok=%v), want %q", region, got, ok, want)
		}
		logged := strings.Contains(logs.String(), "region="+region+" ")
		if logged != (profile == "global") {
			t.Errorf("region %s (%s): global-profile log line present=%v", region, profile, logged)
		}
	}
	// The code's table holds nothing the page does not.
	for region, geo := range bedrockGeoPrefix["global.anthropic.claude-haiku-4-5-20251001-v1:0"] {
		if haiku45PublishedProfiles[region] != geo {
			t.Errorf("code maps %s to %q; AWS's page says %q", region, geo, haiku45PublishedProfiles[region])
		}
	}
	// A model with an in-Region id is untouched.
	if got, _ := BedrockModelIDForRegion("claude-sonnet-4-6", "eu-west-1"); got != "anthropic.claude-sonnet-4-6" {
		t.Errorf("claude-sonnet-4-6 in eu-west-1: got %q", got)
	}
}
