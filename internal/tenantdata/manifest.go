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
// table with a workspace_id column is not classified here. Without it this file decays the moment
// someone adds a migration, and a by-hand deletion silently misses a table — which is worse than
// having no procedure, because it produces a confident claim that someone's data is gone.
package tenantdata

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
	"sessions":                      {Delete, ""},
	"stake_positions":               {Delete, ""},
	"traffic_mint_holds":            {Delete, ""},
	"work_tier_observations":        {Delete, ""},
	"workspace_api_keys":            {Delete, ""},
	"workspace_card_fingerprints":   {Delete, ""},
	"workspace_owner_links":         {Delete, ""},
	"workspace_pattern_optin":       {Delete, ""},
	"workspaces":                    {Delete, "the mapping itself — deleting this is what makes the retained ledger rows unlinkable"},
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
