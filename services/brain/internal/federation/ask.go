package federation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

// AskResult is a federated answer with source brain ids.
type AskResult struct {
	Answer    string   `json:"answer"`
	BrainIDs  []string `json:"brain_ids"`
	Failure   string   `json:"failure,omitempty"`
	Denied    bool     `json:"denied,omitempty"`
	Partial   bool     `json:"partial,omitempty"`
	Citations []string `json:"citations,omitempty"`
}

// AskOpts configures federated ask (MVP: local paths only).
type AskOpts struct {
	Principal string
	Tenant    string
	Region    string
	Query     string
	TopK      int
	MaxBrains int
	Cards     []BrainCard
	Now       time.Time
}

// Ask runs authorize → filter → rank → local ask with capability check.
// Never invents cites from unauthorized brains (FED-005/008/009).
func Ask(ctx context.Context, opts AskOpts) AskResult {
	if strings.TrimSpace(opts.Query) == "" {
		return AskResult{Failure: "empty query"}
	}
	if opts.Principal == "" {
		return AskResult{Failure: "denied", Denied: true}
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	// FED-002: filter before any open.
	eligible := FilterCards(opts.Cards, opts.Principal, opts.Tenant, opts.Region)
	if len(eligible) == 0 {
		return AskResult{Failure: "denied", Denied: true}
	}
	selected := RankCards(eligible, opts.Query, opts.MaxBrains)
	var (
		answers []string
		brains  []string
		cites   []string
	)
	for _, card := range selected {
		cap := MintCapability(opts.Principal, card.BrainID, "ask", 30*time.Second, now)
		if !cap.Valid(opts.Principal, card.BrainID, "ask", now) {
			continue
		}
		// Open local brain; apply multi_principal if security present.
		c, err := hosted.OpenLocal(card.Path, card.BrainID)
		if err != nil {
			continue
		}
		// Card already passed FilterCards (authorize-before-fanout). For
		// multi_principal brains re-check grants; for single_user run as owner.
		// Fail closed: ACL load errors must not fall through to single_user.
		sec, err := productsec.ContextFromBrain(card.Path, opts.Principal, "")
		if err != nil {
			_ = c.Close()
			continue
		}
		if sec.Profile == productsec.ProfileMultiPrincipal {
			c.SetSecurity(sec)
			ans := c.AnswerOpts(ctx, hosted.AnswerOptions{
				Question: opts.Query, TopK: opts.TopK, Principal: opts.Principal,
				Profile: string(productsec.ProfileMultiPrincipal),
			})
			_ = c.Close()
			if ans.Failure == "denied" {
				continue
			}
			if ans.Answer != "" {
				answers = append(answers, ans.Answer)
				brains = append(brains, card.BrainID)
				cites = append(cites, ans.CitedDocumentIDs...)
			}
			continue
		}
		// single_user: principal already authorized via BrainCard.AllowedFor.
		c.SetSecurity(productsec.Context{
			Profile: productsec.ProfileSingleUser, Owner: sec.Owner, Principal: sec.Owner,
		})
		ans := c.AnswerOpts(ctx, hosted.AnswerOptions{
			Question: opts.Query, TopK: opts.TopK,
		})
		_ = c.Close()
		if ans.Failure == "denied" {
			continue
		}
		if ans.Answer != "" {
			answers = append(answers, ans.Answer)
			brains = append(brains, card.BrainID)
			cites = append(cites, ans.CitedDocumentIDs...)
		}
	}
	if len(answers) == 0 {
		return AskResult{Failure: "no_authorized_evidence", Partial: true}
	}
	// Merge: concatenate with brain labels (central ground placeholder).
	var b strings.Builder
	for i, a := range answers {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "[%s] %s", brains[i], a)
	}
	return AskResult{
		Answer: b.String(), BrainIDs: brains, Citations: cites,
		Partial: len(answers) < len(selected),
	}
}
