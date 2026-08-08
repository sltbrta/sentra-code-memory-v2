package hosted

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Provider/model price accounting (issue #302). Costs are estimated against a
// configured USD-per-million-token price table that is pinned by content
// digest, so a run's cost summary always states which table priced it. The
// ledger records only counters (tokens, calls, attempts) — never prompts,
// evidence, or gold — so usage diagnostics stay sanitized-safe.
//
// Env knobs:
//
//	OUROBOROS_ERB_PRICES  full JSON replacement of the embedded default table:
//	                      {"provider/model": {"input_per_mtok": X, "output_per_mtok": Y}}
//	                      Invalid JSON disables costing for the request and is
//	                      stamped as prices_status=invalid_env_json (fail-closed).

//go:embed erb_prices.json
var defaultERBPricesJSON []byte

// priceEntry is one configured USD-per-million-token rate.
type priceEntry struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// priceTable is the effective provider/model price configuration. Keys are
// "provider/model"; bare "model" keys are a fallback for unknown providers.
type priceTable map[string]priceEntry

// loadPriceTable returns the effective table and its source: the
// OUROBOROS_ERB_PRICES override when set (full replacement), else the
// embedded default. A malformed override returns an error; callers must
// stamp the failure rather than silently fall back (config pinning).
func loadPriceTable() (priceTable, string, error) {
	raw := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_PRICES"))
	if raw == "" {
		t, err := parsePriceTable(defaultERBPricesJSON)
		return t, "default", err
	}
	t, err := parsePriceTable([]byte(raw))
	if err != nil {
		return nil, "env:OUROBOROS_ERB_PRICES", err
	}
	return t, "env:OUROBOROS_ERB_PRICES", nil
}

func parsePriceTable(raw []byte) (priceTable, error) {
	var t priceTable
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	if t == nil {
		t = priceTable{}
	}
	return t, nil
}

// digest returns a stable 16-hex content digest of the table (sorted keys,
// fixed-point 6-decimal rates, compact encoding) so receipts can pin exactly
// which prices were applied. The encoding is byte-compatible with the Python
// cost tooling (tools/erb/cost_diagnostics.py), so both sides pin the same
// digest for the same table.
func (t priceTable) digest() string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type flat struct {
		K string `json:"k"`
		I string `json:"i"`
		O string `json:"o"`
	}
	rows := make([]flat, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, flat{
			K: k,
			I: strconv.FormatFloat(t[k].InputPerMTok, 'f', 6, 64),
			O: strconv.FormatFloat(t[k].OutputPerMTok, 'f', 6, 64),
		})
	}
	blob, _ := json.Marshal(rows)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])[:16]
}

// lookup resolves the price for a provider/model pair: exact
// "provider/model" first, then the bare model as a fallback.
func (t priceTable) lookup(provider, model string) (priceEntry, bool) {
	if provider != "" && model != "" {
		if e, ok := t[provider+"/"+model]; ok {
			return e, true
		}
	}
	if model != "" {
		if e, ok := t[model]; ok {
			return e, true
		}
	}
	return priceEntry{}, false
}

// costUSD estimates USD for a token pair under one entry.
func (e priceEntry) costUSD(in, out int) float64 {
	return float64(in)/1e6*e.InputPerMTok + float64(out)/1e6*e.OutputPerMTok
}

// roundCost rounds to nano-dollar so stamped costs are stable across
// encodings without pretending to sub-nanodollar precision.
func roundCost(v float64) float64 {
	return math.Round(v*1e9) / 1e9
}

// stampLLMCost writes the llm_cost block for this request from the ledger's
// per-provider/model usage snapshot. Priced and unpriced usage are reported
// together; the total covers priced entries only, and the block is always
// explicit about which price table (digest) produced it. Missing usage
// (provider returned no token counts) is counted, never guessed.
func stampLLMCost(diag map[string]any, models []usageRow, missing int) {
	if diag == nil || len(models) == 0 {
		return
	}
	table, source, err := loadPriceTable()
	if err != nil {
		diag["llm_cost"] = map[string]any{
			"prices_status": "invalid_env_json",
			"prices_source": source,
			"currency":      "USD",
			"estimated":     true,
			"note": "OUROBOROS_ERB_PRICES is not valid JSON; costing disabled " +
				"for this request (fail-closed). Token usage remains in llm_usage.",
		}
		return
	}
	type costRow = map[string]any
	rows := make([]costRow, 0, len(models))
	var unpriced []string
	total := 0.0
	for _, m := range models {
		provider, model, ok := strings.Cut(m.key, "/")
		if !ok {
			model = m.key
		}
		row := costRow{
			"provider":      provider,
			"model":         model,
			"input_tokens":  m.inTok,
			"output_tokens": m.outTok,
			"total_tokens":  m.totalTok,
			"missing_usage": m.missing,
		}
		if e, found := table.lookup(provider, model); found {
			row["input_per_mtok"] = e.InputPerMTok
			row["output_per_mtok"] = e.OutputPerMTok
			cost := roundCost(e.costUSD(m.inTok, m.outTok))
			row["cost_usd"] = cost
			row["priced"] = true
			total += cost
		} else {
			row["cost_usd"] = 0.0
			row["priced"] = false
			unpriced = append(unpriced, providerModelKey(provider, model))
		}
		rows = append(rows, row)
	}
	block := map[string]any{
		"currency":          "USD",
		"estimated":         true,
		"prices_source":     source,
		"prices_digest":     table.digest(),
		"prices_entries":    len(table),
		"total_cost_usd":    roundCost(total),
		"by_provider_model": rows,
	}
	if len(unpriced) > 0 {
		block["unpriced"] = unpriced
	}
	if missing > 0 {
		block["calls_missing_usage"] = missing
	}
	diag["llm_cost"] = block
}

// providerModelKey rebuilds the ledger key for display.
func providerModelKey(provider, model string) string {
	if provider == "" {
		return model
	}
	if model == "" {
		return provider
	}
	return provider + "/" + model
}
