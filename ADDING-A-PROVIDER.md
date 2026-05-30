# Adding a provider or model to Lens

Model facts and provider dispatch are data-driven (Upgrade 16). Adding a
model or a whole provider is additive — no edits scattered across the proxy.

## Add a model to an existing provider

Add one entry to `internal/catalog/seed.go` (`seedModels()`):

```go
{ID: "new-model", Provider: "openai", DisplayName: "New Model",
 InputPer1M: 1.00, OutputPer1M: 3.00,
 Capabilities: Capabilities{Vision: true},
 ContextTokens: 128000, MaxOutput: 16384,
 Aliases: []string{"new-model-2026-01-01"}}, // dated snapshots → canonical
```

That single entry feeds **all** consumers automatically:

- **Pricing** — `alerts.CostUSD` reads `catalog.Price`, so spend
  attribution, budgets, forecasting, anomaly detection, and the ROI report
  all price the model. (The price-parity test in
  `internal/catalog/catalog_test.go` guards against silent re-pricing.)
- **Capabilities** — `modality.Supports`/`Get`/`CapabilityMap` read
  `catalog.CapabilitiesOf`, so capability-aware (vision) routing and the
  `/v1/models/capabilities` endpoint pick it up.
- **Introspection** — it appears in `GET /v1/catalog/models[/:id]` and the
  dashboard catalog panel.

No rebuild needed for an operator: `catalog.Override` /
`catalog.LoadOverrides` layer runtime additions/re-prices on top of the
embedded default (wire from config or a DB table).

> Routing **policy** stays in its packages, not the catalog: if the new
> model should be auto-selected, add it to the router's cost-tier ranks
> (`internal/router/router.go`) and/or the cheap/mid/premium tiers; if it
> should be a fallback target, add it to `internal/fallback`. The catalog
> owns model *facts*, the router owns *which model to pick*.

## Add a new provider

1. **Catalog** — add the provider's models (step above), with
   `Provider: "newprovider"`.
2. **Dispatch** — add one case to `configForProvider(name)` in
   `internal/proxy/proxy.go` returning a `providerConfig`:

   ```go
   case "newprovider":
       return providerConfig{
           name:          "newprovider",
           upstreamURLFn: func(model string) string { return baseURL + "/v1/chat/completions" },
           setAuth:       func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+key) },
           // translateRequest/translateResponse: omit if the provider is
           // OpenAI-compatible; supply them otherwise (see the Gemini case).
       }
   ```

   `providerConfig` is the `Provider` plugin seam (`internal/proxy/provider.go`):
   it implements `Provider` (ProviderName / UpstreamURL / ApplyAuth /
   BuildRequest / ParseResponse). OpenAI-compatible providers need only the
   URL + auth; non-compatible ones supply the two translators.
3. **Routing/fallback (optional policy)** — add fallback targets in
   `internal/fallback` and tier placement in `internal/router` if you want
   the provider's models auto-selected.

That's it — the rest of the system (pricing, capabilities, introspection,
metrics) is driven by the catalog + the `Provider` seam.
