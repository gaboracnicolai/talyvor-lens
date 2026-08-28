// Package tenantdata declares, in one place, every table that holds a customer's data and what
// must happen to it when that customer leaves.
//
// WHY THIS EXISTS WITHOUT A DELETION ENDPOINT. Deletion is done by hand today, and the policy says
// so. This is what makes a by-hand deletion possible at all: `workspace_id` has NO foreign key
// anywhere in the schema (`REFERENCES workspaces` appears zero times across 111 migrations), so
// there is no ON DELETE CASCADE to lean on. Removing a customer means an explicit DELETE against
// every table carrying the column — and the only way to know that list is to write it down and
// keep it honest.
//
// THE GUARD IS THE POINT, not the list. manifest_test.go reads the LIVE SCHEMA and fails when a
// tenant-keyed table is not classified here. Without it this file decays the moment someone adds a
// migration, and a by-hand deletion silently misses a table — which is worse than having no
// procedure, because it produces a confident claim that someone's data is gone.
//
// ⚠ W6.26 — AND THE GUARD'S POPULATION WAS THE THING THAT WAS WRONG. It asked the schema for
// tables with a column named exactly `workspace_id`. 58 of the 99 tables answer to that. But the
// tenant key is spelled SIX other ways in this schema — measured, not guessed:
//
//	workspace_id              58   owner_workspace_id      2
//	contributor_workspace_id   8   attester_workspace_id   1
//	requester_workspace_id     5   author_workspace_id     1
//	                               source_workspace        1
//
// ⚠ Those are TABLE counts, and the first draft of this comment had 9 and 6 rather than 8 and 5
// because it counted two VIEWS — distill_royalty_margin and pool_royalty_margin — as tables. The
// widened guard listed them as unclassified tenant tables too, which would have been a false
// finding of exactly the flattering kind: a view holds no rows and a DELETE against one fails.
// TestPopulationExcludesViews is that mistake, kept.
//
// Twelve tables carry one of the other six and NO bare workspace_id, so the guard could not see
// them, so nobody classified them, so a by-hand erasure misses them. A thirteenth — workspace_configs
// — is keyed by `id` like `workspaces` is, and the guard hand-exempted `workspaces` by name rather
// than admitting the category existed.
//
// ⚠ THE SHAPE OF THE MISS IS WORTH MORE THAN THE COUNT. tenant.Store owns exactly two tables and
// says so in its own comment: `workspace_configs` and `workspace_api_keys`. One of them was
// classified and one was not, and the only difference between them is how the column is spelled.
// A population boundary drawn on a NAME will always cut somewhere the data does not.
//
// The population is now: relkind r/p (not views — see the control in manifest_test.go), not a
// partition, and either a column matching %workspace% or membership of MappingTables below.
package tenantdata

// MappingTables are the tables whose PRIMARY KEY *is* the workspace id, so no column matching
// %workspace% exists to discover them by. The schema cannot tell us that `id` means a workspace
// here and a row id elsewhere, so this is declared — but it is declared, checked (every name must
// exist and must be classified), and no longer a silent exemption inside a test.
//
// ⚠ `workspaces` was already exempt, by name, in the staleness check. `workspace_configs` was not,
// and that is the whole finding: the exemption named one member of a category instead of naming
// the category.
var MappingTables = []string{"workspaces", "workspace_configs"}

// Disposition is what happens to a table's rows when a customer leaves.
type Disposition int

const (
	// Delete — rows are removed. This is the default for customer data and covers the bulk of
	// what is actually sensitive: cached answers, embeddings, prompts, keys, sessions, config.
	Delete Disposition = iota

	// Retain — rows stay, with a legal reason recorded below. These are audit-guarded: migration
	// 0055's audit_block_mutation raises on UPDATE **and** DELETE, so they can be neither removed
	// nor rewritten. That is deliberate and this manifest does not propose changing it.
	Retain

	// NotTenantScoped — the table carries a workspace_id column but holds no customer content:
	// operational bookkeeping that references a workspace without describing anybody. Classified
	// explicitly so the guard cannot be satisfied by silence.
	NotTenantScoped
)

// String names the disposition. Without it the guards' failure messages print the raw iota —
// "classified 0" — which reads as a missing value rather than as Delete. Control T5 produced
// exactly that message and it was the message that was wrong, not the guard.
func (d Disposition) String() string {
	switch d {
	case Delete:
		return "Delete"
	case Retain:
		return "Retain"
	case NotTenantScoped:
		return "NotTenantScoped"
	}
	return "Disposition(unknown)"
}

// Entry is one table's classification and the reason for it.
type Entry struct {
	Disposition Disposition
	// Why is required for Retain and NotTenantScoped: a classification without a reason is an
	// assertion, and the next person cannot tell a decision from an oversight.
	Why string
}

// Manifest maps table name → disposition. Partitioned tables appear ONCE, under the parent:
// a DELETE against the parent reaches every partition, and listing the eight children of each
// would be noise that rots the moment the partition count changes.
var Manifest = map[string]Entry{
	// ─── RETAIN: audit-guarded (migration 0055). Cannot be deleted OR updated. ────────────────
	//
	// These are the financial and provenance record. UK GDPR erasure is not absolute — it yields
	// to legal obligation and to the establishment or defence of legal claims, and accounting
	// records are the standard example. Deleting audit_block_mutation to satisfy an erasure
	// request would trade a working integrity control for the appearance of compliance, and would
	// leave us unable to prove what we charged anyone.
	//
	// ⚠ Note these cannot be ANONYMISED either: the trigger blocks UPDATE, so the workspace_id
	// column cannot be rewritten. Unlinkability comes from deleting the MAPPING instead — the
	// workspace row, its keys and its sessions — after which these rows reference a derived id
	// (u + base32(sha256(issuer‖sub))) that resolves to nobody. That is pseudonymisation, not
	// anonymisation, and the distinction is a lawyer's to draw.
	"lens_token_ledger":   {Retain, "audit-guarded (0055): LENS mint/spend record; financial-record retention"},
	"lxc_ledger":          {Retain, "audit-guarded (0055): LXC money ledger; financial-record retention"},
	"request_attribution": {Retain, "audit-guarded (0055): provenance of what was served and to whom"},
	"povi_receipts":       {Retain, "audit-guarded (0055): signed compute receipts; tamper-evidence record"},

	// token_events is audit-guarded TOO, but migration 0055's trigger carries a sanctioned
	// exception: DELETE is permitted while the retention-bypass session flag is set — the flag the
	// retention sweeper already uses. So it is deletable through a path that exists and is already
	// trusted, and it matters most, because under a `full` logging policy this is the table that
	// holds prompt text.
	//
	// ⚠ The flag is NOT named here on purpose. internal/audit's integrity guard asserts it appears
	// in exactly one non-test file — internal/audit/retention.go — because every extra reference is
	// another place that can delete audit rows. That guard caught this comment, which is the guard
	// working; see retention.go for the flag itself and the by-hand procedure for how to set it.
	"token_events": {Delete, "audit-guarded; deletable only with the retention-bypass flag set (see internal/audit/retention.go)"},

	// ─── NOT TENANT-SCOPED: carries workspace_id, holds no customer content ───────────────────
	"mint_idempotency": {NotTenantScoped, "idempotency keys; operational replay-protection, describes nobody"},

	// ─── W6.26: THE THIRTEEN THE OLD POPULATION COULD NOT SEE ────────────────────────────────
	//
	// ⚠ THE RULE THAT DECIDES THESE IS THE MANIFEST'S OWN, NOT A NEW ONE. Retain here is reserved
	// for rows migration 0055's audit_block_mutation makes UNDELETABLE — that is the whole
	// argument for the four Retain entries above. NONE of the thirteen below carries that trigger
	// (measured against pg_trigger on the migrated schema: only lens_token_ledger, lxc_ledger,
	// povi_receipts, request_attribution, token_events and reputation_events do). So the existing
	// rule classifies every one of them Delete, and the precedent is concrete rather than
	// analogical: `lens_shadow_mints`, `traffic_mint_holds` and `pattern_mine_credits` are mint
	// tables already classified Delete above.
	//
	// ⚠ NICOLAI, THE ONE THING TO CONFIRM. Six of these are *_mints — a financial-adjacent record.
	// This applies the manifest's stated rule to them rather than deciding anything new, and
	// DeleteOrder is a BY-HAND procedure, so adding a table tells a person to delete it rather
	// than deleting it. But if a mint record is a financial record that must survive erasure, the
	// fix is a trigger in a migration (which would then make Retain the honest classification
	// here), not a quiet edit to this map. Every Why below says which precedent settled it.
	"annotation_tasks": {Delete, "W6.26; key `source_workspace`. Holds response_a/response_b — model OUTPUT " +
		"text drawn from that workspace's traffic (internal/mining/annotation_mining.go). `annotations` " +
		"ON DELETE CASCADEs off it, so this entry is what reaches BOTH; before it, neither was reachable."},
	"benchmark_eval_items": {Delete, "W6.26; key `author_workspace_id`. Holds input/expected_output for a " +
		"contributed benchmark item and names the contributing workspace (internal/benchprobe/store.go)."},
	"confidential_compute_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded; same " +
		"disposition as `lens_shadow_mints` above."},
	"distill_royalty_basis": {Delete, "W6.26; keys `owner_workspace_id`+`requester_workspace_id`. The royalty " +
		"basis names BOTH tenants of a cross-tenant serve — erasing one leaves the other's row naming them."},
	"distill_royalty_mints": {Delete, "W6.26; keys `contributor_workspace_id`+`requester_workspace_id`. Not " +
		"audit-guarded; same disposition as `lens_shadow_mints`."},
	"distill_serve_attribution": {Delete, "W6.26; keys `owner_workspace_id`+`requester_workspace_id`. A per-pair " +
		"serve counter — it describes who served whom, which is that customer's traffic."},
	"eval_contribution_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded."},
	"eval_correctness_attestations": {Delete, "W6.26; key `attester_workspace_id`. Records which workspace " +
		"attested to which item (internal/benchprobe/store.go)."},
	"node_latency_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded."},
	"pool_royalty_mints": {Delete, "W6.26; keys `contributor_workspace_id`+`requester_workspace_id`, and it also " +
		"carries prompt_sha256/answer_sha256 — a hash of the customer's prompt is still derived from it."},
	"pooled_shadow_observations": {Delete, "W4.9; key `workspace_id`. The cross-tenant pooling SHADOW LOG " +
		"(migration 0125). It stores no prompt TEXT, only SHA-256 fingerprints — but `pool_royalty_mints` " +
		"directly above settles that: a hash of the customer's prompt is still DERIVED FROM IT, and the row " +
		"also records that this workspace made a request at this time. Not audit-guarded (no trigger), " +
		"purely observational, and nothing downstream aggregates it into money — so there is no retention " +
		"claim pulling the other way. Delete."},
	"routing_prediction_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded."},
	"royalty_detector_findings": {Delete, "W6.26; keys `contributor_workspace_id`+`requester_workspace_id` plus " +
		"`identity_key` — an abuse-detection finding ABOUT a named workspace."},
	// ⚠ THE ONE THAT MAKES THE PSEUDONYMISATION ARGUMENT ABOVE ACTUALLY TRUE. The Retain block
	// says unlinkability comes from deleting the MAPPING — "the workspace row, its keys and its
	// sessions" — after which the retained ledger rows reference an id that resolves to nobody.
	// There are TWO mapping tables. workspace_configs is keyed by the same id and holds the same
	// `name`, written from the admin route that calls tenant.Store.UpsertConfig. Deleting
	// `workspaces` while leaving this behind does not achieve what that paragraph claims: the
	// customer's NAME survives, still keyed by the id the retained rows point at.
	"workspace_configs": {Delete, "W6.26; keyed by `id` like `workspaces`. Holds name, spending_cap, " +
		"monthly_budget, allowed_models, retention_days. Sibling of `workspace_api_keys` — tenant.Store " +
		"owns exactly these two and only the other one was classified."},

	// ─── DELETE: everything else. The bulk of what is actually sensitive. ─────────────────────
	"agent_lxc_subbudgets":     {Delete, ""},
	"annotator_stakes":         {Delete, ""},
	"api_keys":                 {Delete, ""},
	"batch_jobs":               {Delete, ""},
	"billing_customers":        {Delete, ""},
	"budgets":                  {Delete, ""},
	"cache_nodes":              {Delete, ""},
	"compression_measurements": {Delete, "per-request prompt lengths for one workspace; a byte count is not content, but it is still that customer's traffic"},
	"embedding_nodes":          {Delete, ""},
	"eval_datasets":            {Delete, ""},
	"eval_runs":                {Delete, ""},
	"eval_schedules":           {Delete, ""},
	"eval_test_cases":          {Delete, ""},
	"experiments":              {Delete, ""},
	"guardrail_events":         {Delete, ""},
	"guardrail_policies":       {Delete, ""},
	"inference_nodes":          {Delete, ""},
	"k4_mechanical_verdicts":   {Delete, ""},
	"k4_output_verdicts":       {Delete, ""},
	"keel_findings":            {Delete, ""},
	"lens_shadow_mints":        {Delete, ""},
	"lens_token_balances":      {Delete, ""},
	"lxc_balances":             {Delete, ""},
	"lxc_purchases":            {Delete, ""},
	// W4.6.1 step 1. Classified DELETE for the same reason `lxc_purchases` is: the
	// authoritative record of what was charged lives in STRIPE, which has its own
	// retention and its own erasure obligations. These two tables are Lens's local
	// projection of that — a workspace's subscription state and the events that moved
	// it — so deleting them on erasure removes the customer's data here without
	// destroying the financial record anyone would actually audit.
	// W4.6.1 step 2 — the Model 2 allowance. Same reasoning as the two rows below:
	// Stripe holds the authoritative billing record, this is Lens's local grant-and-
	// consumption projection of it, so erasure removes the customer's data here without
	// destroying anything an audit would reach for.
	"subscription_allowance":        {Delete, ""},
	"subscription_events":           {Delete, ""},
	"subscriptions":                 {Delete, ""},
	"lxc_reservations":              {Delete, ""},
	"output_attributions":           {Delete, ""},
	"pattern_mine_credits":          {Delete, ""},
	"povi_challenges":               {Delete, ""},
	"povi_stakes":                   {Delete, ""},
	"prompt_embeddings":             {Delete, "the cached ANSWERS — the most sensitive thing held"},
	"prompts":                       {Delete, ""},
	"provenance_bonds":              {Delete, ""},
	"quality_scores":                {Delete, ""},
	"routing_brain_autonomous":      {Delete, ""},
	"routing_brain_recommendations": {Delete, ""},
	"routing_decisions":             {Delete, ""},
	"routing_patterns":              {Delete, ""},
	"routing_predictions":           {Delete, ""},
	"served_request_measurements":   {Delete, ""},
	"session_keys":                  {Delete, ""},
	"sessions":                      {Delete, ""},
	"stake_positions":               {Delete, ""},
	"traffic_mint_holds":            {Delete, ""},
	"work_tier_observations":        {Delete, ""},
	"workspace_api_keys":            {Delete, ""},
	"workspace_card_fingerprints":   {Delete, ""},
	// W1.9.1. Each row names a workspace and two of its keys. The keys themselves are Delete, so
	// retaining pointers to deleted keys would preserve no audit anybody could read while still
	// naming the customer.
	"workspace_key_rotations": {Delete, ""},
	"workspace_owner_links":   {Delete, ""},
	"workspace_pattern_optin": {Delete, ""},
	"workspaces":              {Delete, "the mapping itself — deleting this is what makes the retained ledger rows unlinkable"},
}

// DeleteOrder returns the tables to delete, parents only, in an order safe to run top to bottom.
// Ordering is deliberate rather than alphabetical: balances and reservations reference the same
// workspace as the rows that produced them, and the workspace row goes LAST so an interrupted run
// leaves the mapping in place and the procedure can be restarted.
func DeleteOrder() []string {
	out := make([]string, 0, len(Manifest))
	for t, e := range Manifest {
		if e.Disposition == Delete && t != "workspaces" {
			out = append(out, t)
		}
	}
	sortStrings(out)
	return append(out, "workspaces")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
