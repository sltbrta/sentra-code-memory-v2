// product-brain memory subcommands.
package main

import (
	"flag"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

func runMemory(args []string) {
	if len(args) < 1 {
		fatal("memory: claim-admit|claim-supersede|claim-list|episode-list|episode-bind|episode-resegment|put|get|search|utility required")
	}
	cmd := args[0]
	fs := flag.NewFlagSet("memory-"+cmd, flag.ExitOnError)
	dir := fs.String("dir", "", "brain directory")
	subject := fs.String("subject", "", "claim subject")
	pred := fs.String("predicate", "", "claim predicate")
	obj := fs.String("object", "", "claim object")
	docs := fs.String("docs", "", "comma document ids")
	principal := fs.String("principal", "", "agent principal")
	kind := fs.String("kind", "note", "agent memory kind")
	text := fs.String("text", "", "agent memory text")
	q := fs.String("q", "", "search query")
	epID := fs.String("episode-id", "", "episode id")
	epKind := fs.String("episode-kind", "custom", "episode kind")
	title := fs.String("title", "", "episode title")
	supersedeID := fs.String("supersede", "", "claim id to supersede")
	sources := fs.String("sources", "", "comma episode ids to resegment from")
	_ = fs.Parse(args[1:])
	if *dir == "" {
		fatal("memory: --dir required")
	}
	st, err := memory.Open(*dir)
	if err != nil {
		fatal(err.Error())
	}
	parseDocs := func() []string {
		var docIDs []string
		for _, d := range strings.Split(*docs, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				docIDs = append(docIDs, d)
			}
		}
		return docIDs
	}
	switch cmd {
	case "claim-admit":
		cl, contested, err := st.AdmitClaim(memory.Claim{
			Subject: *subject, Predicate: *pred, Object: *obj, DocumentIDs: parseDocs(),
		})
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{
			"event": "claim_admit", "claim": cl, "contested": contested, "product_owned": true,
		})
	case "claim-supersede":
		if *supersedeID == "" {
			fatal("memory claim-supersede: --supersede <old-claim-id> required")
		}
		cl, err := st.SupersedeClaim(*supersedeID, memory.Claim{
			Subject: *subject, Predicate: *pred, Object: *obj, DocumentIDs: parseDocs(),
		}, time.Now().UTC())
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{
			"event": "claim_supersede", "claim": cl, "superseded": *supersedeID, "product_owned": true,
		})
	case "claim-list":
		include := true
		emitJSON(map[string]any{
			"event": "claim_list", "current": st.CurrentClaims(time.Time{}, false),
			"contested_groups": st.ContestedGroups(), "include_contested_hint": include,
			"product_owned": true,
		})
	case "episode-list":
		emitJSON(map[string]any{
			"event": "episode_list", "episodes": st.ListEpisodes(), "product_owned": true,
		})
	case "episode-bind":
		ep, err := st.BindEpisode(memory.Episode{
			ID: *epID, Kind: *epKind, Title: *title, DocumentIDs: parseDocs(),
		})
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{"event": "episode_bind", "episode": ep, "product_owned": true})
	case "episode-resegment":
		var src []string
		for _, s := range strings.Split(*sources, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				src = append(src, s)
			}
		}
		if len(src) == 0 {
			fatal("memory episode-resegment: --sources e1,e2 required")
		}
		target := *epID
		if target == "" {
			target = "reseg-" + strings.Join(src, "-")
		}
		ep, err := st.ResegmentEpisode(target, src, *title)
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{
			"event": "episode_resegment", "episode": ep, "sources": src, "product_owned": true,
		})
	case "put":
		e, err := st.PutAgentMemory(*principal, *kind, *text, nil)
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{"event": "agent_memory_put", "entry": e, "product_owned": true})
	case "get":
		emitJSON(map[string]any{
			"event": "agent_memory_get", "entries": st.GetAgentMemory(*principal, 50), "product_owned": true,
		})
	case "search":
		emitJSON(map[string]any{
			"event": "agent_memory_search", "entries": st.SearchAgentMemory(*principal, *q, 20), "product_owned": true,
		})
	case "utility":
		ids := []string{}
		for _, ep := range st.ListEpisodes() {
			ids = append(ids, ep.DocumentIDs...)
		}
		ranked := st.RankDocumentsByUtility(ids)
		scores := map[string]float64{}
		for _, id := range ranked {
			scores[id] = st.GetUtility(id)
		}
		emitJSON(map[string]any{
			"event": "utility", "ranked": ranked, "scores": scores,
			"ppr_edges": len(st.DocEdges()), "summaries": len(st.ListSummaries()),
			"product_owned": true,
		})
	default:
		fatal("memory: unknown subcommand " + cmd)
	}
}
