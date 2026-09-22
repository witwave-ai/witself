package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

// Self's general client structs collapse absent pending/count fields to zero.
// Decode only the observational metadata needed here, retaining JSON presence.
// All raw bytes are collection-local and bounded; neither errors nor source
// records are retained in the summary cache.
const summaryResponseLimit = 2 * 1024 * 1024

func summaryGET(ctx context.Context, cfg Config, path string) (map[string]json.RawMessage, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.Endpoint, "/")+path, nil)
	if err != nil {
		return nil, "unavailable"
	}
	if cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return nil, "unavailable"
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, summaryResponseLimit+1))
	if err != nil || len(body) > summaryResponseLimit {
		return nil, "unavailable"
	}
	fields := summaryObject(body)
	if resp.StatusCode != http.StatusOK {
		// Only the typed feature switch is a disabled source. Never infer state
		// from human-readable errors, generic 403, enrollment, or missing routes.
		var code string
		_ = json.Unmarshal(fields["code"], &code)
		if resp.StatusCode == http.StatusForbidden && code == "feature_not_enabled" {
			return nil, "disabled"
		}
		return nil, "unavailable"
	}
	if fields == nil {
		return nil, "unavailable"
	}
	return fields, "available"
}

func summaryObject(raw []byte) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func summarySelfPath() string {
	q := url.Values{}
	for _, key := range []string{"include_counts", "include_checkpoint", "include_message_checkpoint", "include_email_checkpoint", "include_avatar_checkpoint", "observational"} {
		q.Set(key, "true")
	}
	for _, key := range []string{"include_facts", "include_salient", "include_sensitive", "include_plan_entitlements"} {
		q.Set(key, "false")
	}
	q.Set("max_bytes", "16384")
	return "/v1/self?" + q.Encode()
}

func collectSummary(ctx context.Context, cfg Config, now time.Time) Summary {
	s := emptySummary(now)
	type source struct {
		fields map[string]json.RawMessage
		status string
	}
	sources := make([]source, 6)
	paths := []string{summarySelfPath(), "/v1/transcripts", "/v1/memories?limit=100", "/v1/secrets?include_fields=false&limit=100", "/v1/messages?direction=inbox&limit=100", "/v1/email?limit=100"}
	var usage client.UsageReport
	var usageAvailable bool
	jobs := make(chan int, len(paths)+1)
	for i := 0; i <= len(paths); i++ {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if i == len(paths) {
					usage, usageAvailable = readSummaryUsage(ctx, cfg, s.Window)
				} else {
					sources[i].fields, sources[i].status = summaryGET(ctx, cfg, paths[i])
				}
			}
		})
	}
	wg.Wait()
	if sources[0].status == "available" {
		projectSummarySelf(&s, cfg.Identity, sources[0].fields)
	}
	if usageAvailable {
		projectSummaryUsage(&s, cfg.Identity, usage)
	}
	// Failures are independent: a bad self or memory list cannot erase valid
	// usage, and an unavailable inbox cannot erase outbound recorded activity.
	for _, p := range []struct {
		source, category int
		array            string
	}{{1, 1, "transcripts"}, {2, 3, "items"}, {3, 4, "items"}, {4, 6, "messages"}, {5, 5, "messages"}} {
		src := sources[p.source]
		if p.category != 3 && src.status == "disabled" {
			s.Categories[p.category].Inventory.Status = "disabled"
		}
		if src.status == "available" {
			projectSummaryRecords(&s, p.category, src.fields[p.array])
		}
	}
	// Explicit account switches override only the corresponding inbound source.
	// Email's outbound usage is a different capability and remains independent.
	for _, p := range []struct{ checkpoint, category int }{{1, 6}, {2, 5}} {
		if s.Checkpoints[p.checkpoint].Status == "disabled" {
			s.Categories[p.category].Inventory.Status = "disabled"
			s.Categories[p.category].Inventory.Count = nil
			key := s.Categories[p.category].Key
			filtered := s.Recent[:0]
			for _, update := range s.Recent {
				if update.Key != key {
					filtered = append(filtered, update)
				}
			}
			s.Recent = filtered
		}
	}
	sort.SliceStable(s.Recent, func(i, j int) bool { return s.Recent[i].At.After(s.Recent[j].At) })
	if len(s.Recent) > 12 {
		s.Recent = s.Recent[:12:12]
	}
	return s
}

func summaryIdentityMatches(want client.SelfIdentity, account, realm, agent string) bool {
	return want.AccountID != "" && want.RealmID != "" && want.AgentID != "" && account == want.AccountID && realm == want.RealmID && agent == want.AgentID
}

func projectSummarySelf(s *Summary, want client.SelfIdentity, fields map[string]json.RawMessage) {
	identity := summaryObject(fields["identity"])
	readString := func(key string) string { var v string; _ = json.Unmarshal(identity[key], &v); return v }
	if !summaryIdentityMatches(want, readString("account_id"), readString("realm_id"), readString("agent_id")) {
		return
	}
	counts := summaryObject(summaryObject(fields["index"])["counts"])
	for _, i := range []int{2, 3} {
		var n *int64
		if json.Unmarshal(counts[s.Categories[i].Key], &n) == nil && n != nil && summaryNumber(*n) {
			s.Categories[i].Inventory.Status = "available"
			s.Categories[i].Inventory.Count = n
			s.Categories[i].Inventory.Exact = true
		}
	}
	for i := range s.Checkpoints {
		s.Checkpoints[i].Status = summaryCheckpoint(fields[s.Checkpoints[i].Key+"_checkpoint"])
	}
}

func summaryBool(raw json.RawMessage) (bool, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
		return true, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
		return false, true
	}
	return false, false
}

func summaryCheckpoint(raw json.RawMessage) string {
	f := summaryObject(raw)
	enabled, valid := summaryBool(f["enabled"])
	if valid && !enabled {
		return "disabled"
	}
	if _, exists := f["enabled"]; exists && !valid {
		return "unavailable"
	}
	if unavailable, ok := summaryBool(f["unavailable"]); unavailable || (f["unavailable"] != nil && !ok) {
		return "unavailable"
	}
	pending, ok := summaryBool(f["pending"])
	if !ok {
		return "unavailable"
	}
	if pending {
		return "pending"
	}
	return "clear"
}

func projectSummaryRecords(s *Summary, category int, raw json.RawMessage) {
	var records []map[string]json.RawMessage
	if json.Unmarshal(raw, &records) != nil || records == nil {
		return
	}
	// The transcript API has no pagination parameter; its store supplies the
	// newest 100. Enforce the same projection bound even on a regressed server.
	if len(records) > 100 {
		records = records[:100]
	}
	for _, record := range records {
		if record == nil {
			return
		}
	}
	if category != 3 {
		n := int64(len(records))
		s.Categories[category].Inventory.Status = "available"
		s.Categories[category].Inventory.Count = &n
	}
	d := summaryDefinitions()[category]
	for _, record := range records {
		var candidates []json.RawMessage
		switch category {
		case 1, 3:
			candidates = []json.RawMessage{record["created_at"], record["updated_at"]}
		case 4:
			candidates = []json.RawMessage{record["created_at"], record["updated_at"], record["archived_at"], record["deleted_at"]}
		case 5:
			candidates = []json.RawMessage{record["received_at"], record["delivered_at"]}
		case 6:
			candidates = []json.RawMessage{record["created_at"], summaryObject(record["delivery"])["delivered_at"]}
		}
		var latest time.Time
		for _, raw := range candidates {
			var at time.Time
			if json.Unmarshal(raw, &at) == nil && !at.IsZero() && at.Year() >= 1 && at.Year() <= 9999 && !at.After(s.GeneratedAt) && at.After(latest) {
				latest = at.UTC()
			}
		}
		if !latest.IsZero() {
			s.Recent = append(s.Recent, SummaryUpdate{Key: d.key, At: latest, Action: d.action})
		}
	}
}
