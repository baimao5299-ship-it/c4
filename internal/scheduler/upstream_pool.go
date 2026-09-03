// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/logx"
)

// upstreamSnapshot is deliberately separate from accountSnapshot. An upstream
// has no account identity, quota ledger, or account-level credential extension;
// only its endpoint, key, and group-local concurrency/cooldown participate in
// request routing.
type upstreamSnapshot struct {
	member   *domain.GroupUpstream
	upstream *domain.Upstream
	// endpoint/key identify the configuration that owns runtime state. If an
	// upstream is edited in place, its ID remains stable but cooldown and
	// in-flight counters must not be carried to the new connection.
	endpoint string
	host     string
	key      string
	// modelIDs maps the user-facing normalized model key to the exact model
	// identifier advertised by this upstream. The normalized key is used only
	// for route lookup and deduplication; proxy requests must use the raw value.
	modelIDs       map[string]string
	formatModelIDs map[modelFormatKey]string
	concurrency    *atomic.Int64
	state          *atomic.Pointer[upstreamState]
}

type modelFormatKey struct {
	model  string
	format domain.RequestFormat
}

type upstreamState struct {
	cooldownUntil *time.Time
	failureStreak int
	lastError     string
}

type upstreamScope struct {
	groupID    int64
	upstreamID int64
}

type upstreamRoute struct {
	seq      *upstreamWeightedSeq
	fallback []*upstreamWeightedSeq
}

type upstreamWeightedSeq struct {
	seq    []*upstreamSnapshot
	cursor atomic.Uint64
}

func (u *upstreamSnapshot) statePtr() *upstreamState {
	if u == nil || u.state == nil {
		return &upstreamState{}
	}
	st := u.state.Load()
	if st == nil {
		st = &upstreamState{}
		u.state.Store(st)
	}
	return st
}

func upstreamStateFromMember(member *domain.GroupUpstream, current *upstreamState) *upstreamState {
	if current == nil {
		current = &upstreamState{}
	}
	next := *current
	if next.failureStreak < 0 {
		next.failureStreak = 0
	}
	if member != nil && member.FailureStreak > next.failureStreak {
		next.failureStreak = member.FailureStreak
	}
	// A persisted relation cooldown is authoritative when present. Copy the
	// timestamp so the published snapshot does not retain a pointer into a
	// repository-owned domain object that may be reused or mutated on reload.
	if member != nil && member.CooldownUntil != nil {
		until := *member.CooldownUntil
		next.cooldownUntil = &until
		if member.LastError != nil {
			next.lastError = domain.TruncateErrMsg(*member.LastError)
		}
	}
	return &next
}

func newUpstreamSnapshot(member *domain.GroupUpstream, old *upstreamSnapshot) *upstreamSnapshot {
	if member == nil {
		return nil
	}
	us := &upstreamSnapshot{
		member: member, upstream: member.Upstream,
		concurrency: new(atomic.Int64), state: new(atomic.Pointer[upstreamState]),
		modelIDs: make(map[string]string), formatModelIDs: make(map[modelFormatKey]string),
	}
	if member.Upstream != nil {
		for _, raw := range member.Upstream.Models {
			id := canonicalModelID(raw)
			if id == "" {
				continue
			}
			key := modelMatchKey(id)
			if _, exists := us.modelIDs[key]; !exists {
				us.modelIDs[key] = strings.TrimSpace(raw)
			}
		}
		// A manual probe can be the only source of a tenant-scoped model until
		// the provider's catalogue catches up, so retain those raw keys too.
		formatModels := make([]string, 0, len(member.Upstream.ModelFormats))
		for raw := range member.Upstream.ModelFormats {
			formatModels = append(formatModels, raw)
		}
		slices.Sort(formatModels)
		for _, raw := range formatModels {
			id := canonicalModelID(raw)
			if id == "" {
				continue
			}
			key := modelMatchKey(id)
			if _, exists := us.modelIDs[key]; !exists {
				us.modelIDs[key] = strings.TrimSpace(raw)
			}
			for _, format := range member.Upstream.ModelFormats[raw] {
				key := modelFormatKey{model: modelMatchKey(id), format: format}
				if _, exists := us.formatModelIDs[key]; !exists {
					us.formatModelIDs[key] = strings.TrimSpace(raw)
				}
			}
		}
		us.endpoint = normalizeUpstreamEndpoint(member.Upstream.BaseURL)
		us.host = sanitizedUpstreamHost(us.endpoint)
		if member.Upstream.UpstreamKey != nil {
			us.key = normalizeUpstreamKey(member.Upstream.UpstreamKey)
		}
	}
	var current *upstreamState
	identityMatches := old != nil && old.member != nil &&
		old.member.GroupID == member.GroupID &&
		old.member.UpstreamID == member.UpstreamID &&
		old.endpoint == us.endpoint && old.key == us.key
	if identityMatches {
		// Share the atomics instead of copying their values. In-flight requests
		// may still release the previous snapshot after this reload; sharing the
		// runtime state keeps those releases visible to the new route and avoids
		// a permanently inflated concurrency count.
		us.concurrency = old.concurrency
		us.state = old.state
		current = old.state.Load()
	} else if old != nil {
		// The relation cooldown belongs to the previous endpoint/key too. An
		// upstream edit therefore starts with a clean breaker; only a fresh
		// relation (old == nil) may import a persisted cooldown below.
		us.state.Store(&upstreamState{})
		return us
	}
	us.state.Store(upstreamStateFromMember(member, current))
	return us
}

// rawModelFor returns the provider spelling for a normalized route key.
// Keeping this lookup on the immutable upstream snapshot makes request routing
// independent of later catalogue refreshes and avoids rewriting provider IDs.
func (u *upstreamSnapshot) rawModelFor(model string, format domain.RequestFormat, requested string) string {
	if u != nil {
		id := modelMatchKey(model)
		if raw := u.formatModelIDs[modelFormatKey{model: id, format: format}]; raw != "" {
			return raw
		}
		if raw := u.modelIDs[id]; raw != "" {
			return raw
		}
	}
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested
	}
	return strings.TrimSpace(model)
}

// normalizeUpstreamEndpoint keeps the scheduler's endpoint identity aligned
// with the service and aiclient conventions: the stored upstream URL is a
// bare root, while OpenAI protocol paths add /v1 themselves. Older rows may
// still contain a trailing /v1 (in any case or with extra slashes); treating
// that spelling as equivalent prevents requests from being sent to /v1/v1.
func normalizeUpstreamEndpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Path == "" {
		return base
	}
	path := strings.TrimRight(parsed.Path, "/")
	lower := strings.ToLower(path)
	if lower == "/v1" || strings.HasSuffix(lower, "/v1") {
		path = path[:len(path)-len("/v1")]
		if path == "/" {
			path = ""
		}
		parsed.Path = path
		parsed.RawPath = ""
		base = parsed.String()
	}
	return strings.TrimRight(base, "/")
}

// sanitizedUpstreamHost returns only the URL authority host (including an
// explicit port). User info, path, query, and fragment are never retained in
// log attribution. Invalid or relative endpoints stay unknown instead of
// falling back to the raw configured string.
func sanitizedUpstreamHost(base string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

func buildUpstreamSnapshots(groups map[int64]*groupSnapshot, configs map[int64]*domain.Group, old map[int64]*upstreamSnapshot) map[int64]*upstreamSnapshot {
	byID := make(map[int64]*upstreamSnapshot)
	// Relation rows are replaced by SetGroupUpstreams, so their database IDs
	// are not stable across a policy edit. Keep a scoped index for state
	// continuity while still indexing the published map by relation ID for
	// in-flight release and writeback.
	oldByScope := make(map[upstreamScope]*upstreamSnapshot, len(old))
	for _, candidate := range old {
		if candidate == nil || candidate.member == nil {
			continue
		}
		oldByScope[upstreamScope{groupID: candidate.member.GroupID, upstreamID: candidate.member.UpstreamID}] = candidate
	}
	for gid, cfg := range configs {
		if cfg == nil {
			continue
		}
		gs := groups[gid]
		if gs == nil {
			gs = &groupSnapshot{}
			groups[gid] = gs
		}
		gs.routingMode = cfg.EffectiveRoutingMode()
		gs.allowedModels = append([]string(nil), cfg.AllowedModels...)
		gs.upstreams = nil
		gs.upstreamRoutes = nil
		if gs.routingMode != domain.GroupRoutingModeUpstreams {
			continue
		}
		for _, member := range cfg.UpstreamMembers {
			if member == nil || member.ID <= 0 || member.Upstream == nil || member.Upstream.DeletedAt != nil || !member.Upstream.Enabled || !member.Enabled || strings.TrimSpace(member.Upstream.BaseURL) == "" {
				continue
			}
			// Never mutate a published snapshot in place. Requests can retain an
			// old selection across a reload, so replacing member/upstream pointers
			// on that object would race with the hot path and change its history.
			previous := old[member.ID]
			if previous == nil {
				previous = oldByScope[upstreamScope{groupID: member.GroupID, upstreamID: member.UpstreamID}]
			}
			us := newUpstreamSnapshot(member, previous)
			byID[member.ID] = us
			gs.upstreams = append(gs.upstreams, us)
		}
		gs.upstreamRoutes = buildUpstreamRoutes(gs.upstreams, gs.allowedModels)
		gs.modelAliases = buildModelAliases(gs.routes, gs.upstreamRoutes)
	}
	return byID
}

func buildUpstreamRoutes(pool []*upstreamSnapshot, allowed []string) map[routeKey]*upstreamRoute {
	routes := make(map[routeKey]*upstreamRoute)
	build := func(candidates []*upstreamSnapshot) *upstreamRoute {
		byPriority := make(map[int][]*upstreamSnapshot)
		priorities := make([]int, 0, len(candidates))
		for _, item := range candidates {
			if item == nil || item.member == nil {
				continue
			}
			priority := item.member.Priority
			if _, exists := byPriority[priority]; !exists {
				priorities = append(priorities, priority)
			}
			byPriority[priority] = append(byPriority[priority], item)
		}
		sort.Ints(priorities)
		route := &upstreamRoute{}
		for i, priority := range priorities {
			seq := newUpstreamWeightedSeq(byPriority[priority])
			if len(seq.seq) == 0 {
				continue
			}
			if i == 0 || route.seq == nil {
				route.seq = seq
			} else {
				route.fallback = append(route.fallback, seq)
			}
		}
		return route
	}
	// Keep the route matrix aligned with the complete RequestFormat enum.  The
	// capability snapshot is the authority for protocol support, so adding a
	// format here does not advertise it unless that exact model/format pair was
	// verified (legacy snapshots still take the bounded Chat fallback below).
	formats := []domain.RequestFormat{
		domain.FormatOpenAIChat,
		domain.FormatOpenAIResponses,
		domain.FormatOpenAIResponsesWS,
		domain.FormatAnthropic,
		domain.FormatOpenAIImages,
		domain.FormatOpenAISearch,
	}
	if len(allowed) == 0 {
		models, legacyFallback := upstreamModelIntersection(pool)
		if legacyFallback {
			// Rows created before capability snapshots existed remain routable until
			// their first model read. Once a catalogue is recorded, an empty or
			// failed read produces no route rather than an unrestricted fallback.
			routes[routeKey{format: domain.FormatOpenAIChat}] = build(pool)
			return routes
		}
		for _, model := range models {
			for _, format := range formats {
				candidates := upstreamCandidatesForModelFormat(pool, model, format)
				if len(candidates) == 0 {
					continue
				}
				routes[routeKey{format: format, model: model}] = build(candidates)
			}
		}
		return routes
	}
	byKey := make(map[string]string, len(allowed))
	for _, rawModel := range allowed {
		model := canonicalModelID(rawModel)
		if model == "" {
			continue
		}
		key := modelMatchKey(model)
		if previous := byKey[key]; previous == "" || model < previous {
			byKey[key] = model
		}
	}
	models := make([]string, 0, len(byKey))
	for _, model := range byKey {
		models = append(models, model)
	}
	slices.Sort(models)
	for _, model := range models {
		for _, format := range formats {
			candidates := upstreamCandidatesForModelFormat(pool, model, format)
			if len(candidates) == 0 {
				continue
			}
			routes[routeKey{format: format, model: model}] = build(candidates)
		}
	}
	return routes
}

// upstreamModelIntersection returns the union of confirmed model snapshots.
// The historical name is retained because this helper used to publish the
// intersection. The union is required for an upstream pool: each model route
// is built with only the members that advertise that model, so one provider's
// narrower catalogue cannot hide models offered by another provider.
//
// The bool is true only when every member lacks a capability snapshot,
// preserving legacy rows until an operator performs the first model read.
func upstreamModelIntersection(pool []*upstreamSnapshot) ([]string, bool) {
	confirmed := make(map[string]string)
	checked := false
	unknown := false
	for _, item := range pool {
		if item == nil || item.upstream == nil || item.member == nil || !item.member.Enabled || !item.upstream.Enabled {
			continue
		}
		if item.upstream.ModelsCheckedAt == nil {
			unknown = true
			continue
		}
		checked = true
		// A failed refresh keeps the last known catalogue in service. Continue
		// using that bounded snapshot instead of dropping a healthy route during
		// a transient probe failure; an endpoint/key edit clears the snapshot.
		for _, rawModel := range item.upstream.Models {
			model := canonicalModelID(rawModel)
			if model != "" {
				key := modelMatchKey(model)
				if previous := confirmed[key]; previous == "" || model < previous {
					confirmed[key] = model
				}
			}
		}
		// Explicit/manual probes can record a tenant-scoped model in the
		// protocol map before the provider's catalogue is updated. Include those
		// keys when building the pool's model union so the verified route is not
		// lost at snapshot publication.
		for rawModel := range item.upstream.ModelFormats {
			model := canonicalModelID(rawModel)
			if model != "" {
				key := modelMatchKey(model)
				if previous := confirmed[key]; previous == "" || model < previous {
					confirmed[key] = model
				}
			}
		}
	}
	if !checked {
		return nil, unknown
	}
	models := make([]string, 0, len(confirmed))
	for _, model := range confirmed {
		models = append(models, model)
	}
	slices.Sort(models)
	return models, false
}

func upstreamCandidatesForModelFormat(pool []*upstreamSnapshot, model string, format domain.RequestFormat) []*upstreamSnapshot {
	out := make([]*upstreamSnapshot, 0, len(pool))
	for _, item := range pool {
		if item == nil || item.member == nil || item.upstream == nil || !item.member.Enabled || !item.upstream.Enabled || !upstreamSupportsModelFormat(item.upstream, model, format) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func upstreamSupportsModelFormat(u *domain.Upstream, model string, format domain.RequestFormat) bool {
	model = canonicalModelID(model)
	if !upstreamSupportsModel(u, model) {
		return false
	}
	// Rows written before protocol-aware probing have a checked catalogue but an
	// empty format map. Preserve their historical Chat route so an upgrade does
	// not strand existing accounts. Once any per-model capability is present,
	// however, a missing model key should not invent Responses or Messages
	// capability. Legacy snapshots and compatibility stores can contain a model
	// without a per-model format entry; retain the broadly supported Chat route.
	if len(u.ModelFormats) == 0 {
		return format == domain.FormatOpenAIChat
	}
	formats, recorded := upstreamModelFormats(u.ModelFormats, model)
	if !recorded {
		return format == domain.FormatOpenAIChat
	}
	return slices.Contains(formats, format)
}

func upstreamSupportsModel(u *domain.Upstream, model string) bool {
	model = canonicalModelID(model)
	if u == nil || model == "" {
		return false
	}
	if u.ModelsCheckedAt == nil {
		return true
	}
	for _, candidate := range u.Models {
		if modelMatchKey(candidate) == modelMatchKey(model) {
			return true
		}
	}
	// A manual probe or a partially written capability snapshot can contain a
	// usable model before the catalogue list is updated.  ModelFormats is
	// concrete evidence for that model and must keep it routable; otherwise a
	// successful manual test is immediately hidden by the scheduler.
	if _, recorded := upstreamModelFormats(u.ModelFormats, model); recorded {
		return true
	}
	return false
}

// canonicalModelID is the user-facing spelling. Case is normalized, but the
// familiar punctuation from an existing group or catalogue is retained.
func canonicalModelID(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// modelMatchKey collapses cosmetic separator differences for equality only.
// It is never sent upstream and therefore cannot alter provider identifiers.
func modelMatchKey(model string) string {
	model = canonicalModelID(model)
	if model == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(model))
	lastDash := false
	for _, r := range model {
		if r == '_' || r == '.' {
			r = '-'
		}
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), "-")
}

// upstreamModelFormats resolves legacy JSON keys that contain surrounding
// whitespace using the same canonical model ID as the Models catalogue.
func upstreamModelFormats(all map[string][]domain.RequestFormat, model string) ([]domain.RequestFormat, bool) {
	var merged []domain.RequestFormat
	found := false
	for candidate, formats := range all {
		if modelMatchKey(candidate) == modelMatchKey(model) {
			found = true
			for _, format := range formats {
				if !slices.Contains(merged, format) {
					merged = append(merged, format)
				}
			}
		}
	}
	return merged, found
}

func newUpstreamWeightedSeq(pool []*upstreamSnapshot) *upstreamWeightedSeq {
	// Normalize the weights before applying the sequence cap. Appending until
	// maxSeqLen directly lets a single high-weight member fill the sequence and
	// silently starves every later member; scaling preserves at least one slot
	// for each candidate while keeping the requested ratio as close as possible.
	items := make([]*upstreamSnapshot, 0, len(pool))
	weights := make([]int, 0, len(pool))
	g := 0
	for _, item := range pool {
		if item == nil || item.member == nil {
			continue
		}
		weight := item.member.Weight
		if weight <= 0 {
			weight = 100
		}
		if weight > 10000 {
			weight = 10000
		}
		items = append(items, item)
		weights = append(weights, weight)
		g = gcdInt(g, weight)
	}
	if len(items) == 0 {
		return &upstreamWeightedSeq{}
	}
	if g <= 0 {
		g = 1
	}
	total := 0
	for _, weight := range weights {
		total += weight / g
	}
	scale := 1
	if total > maxSeqLen {
		scale = (total + maxSeqLen - 1) / maxSeqLen
	}
	seq := make([]*upstreamSnapshot, 0, minInt(total/scale+len(items), maxSeqLen))
	for i, item := range items {
		n := weights[i] / g / scale
		if n < 1 {
			n = 1
		}
		for j := 0; j < n; j++ {
			seq = append(seq, item)
		}
	}
	return &upstreamWeightedSeq{seq: seq}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Scheduler) selectUpstream(gs *groupSnapshot, groupID int64, format domain.RequestFormat, model, requestedModel string, excluded []int64) (*Selection, error) {
	model = canonicalModelID(model)
	rt, ok := gs.upstreamRoutes[routeKey{format: format, model: model}]
	if !ok {
		rt, ok = gs.upstreamRoutes[routeKey{format: format}]
	}
	if !ok || rt == nil || rt.seq == nil || len(rt.seq.seq) == 0 {
		return nil, ErrFormatUnavailable
	}
	now := s.timeNow()
	sequences := make([]*upstreamWeightedSeq, 0, 1+len(rt.fallback))
	if rt.seq != nil {
		sequences = append(sequences, rt.seq)
	}
	sequences = append(sequences, rt.fallback...)
	for _, seq := range sequences {
		for i := 0; i < len(seq.seq); i++ {
			item := seq.seq[int(seq.cursor.Add(1))%len(seq.seq)]
			if item == nil || item.member == nil || item.upstream == nil || !item.member.Enabled || !item.upstream.Enabled || containsID(item.member.ID, excluded) {
				continue
			}
			st := item.statePtr()
			if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
				continue
			}
			limit := item.member.MaxConcurrency
			if limit < 1 {
				limit = 8
			}
			cur := item.concurrency.Load()
			if cur >= int64(limit) || !item.concurrency.CompareAndSwap(cur, cur+1) {
				continue
			}
			return &Selection{
				TargetKind: TargetKindUpstreamMember, TargetID: item.member.ID, GroupID: groupID,
				AccountID: 0, TemplateID: 0, BaseURL: item.endpoint,
				UpstreamID: item.upstream.ID, UpstreamName: item.upstream.Name,
				UpstreamHost: item.host, UpstreamMultiplierBP: item.upstream.MultiplierBP,
				Format: format, UpstreamKey: item.key, CredentialType: credential.TypeAPIKey, Model: item.rawModelFor(model, format, requestedModel),
				upstreamRef: item,
			}, nil
		}
	}
	return nil, ErrNoAvailable
}

// normalizeUpstreamKey keeps old rows that stored a copied Authorization
// value compatible with the scheduler's raw-key contract. The proxy adds the
// Bearer scheme when it forwards a selection, so it must appear only once.
func normalizeUpstreamKey(key *string) string {
	if key == nil {
		return ""
	}
	value := strings.TrimSpace(*key)
	for value != "" {
		fields := strings.Fields(value)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "bearer") {
			break
		}
		value = strings.TrimSpace(value[len(fields[0]):])
	}
	return value
}

func containsID(id int64, ids []int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func (s *Scheduler) ReleaseSelection(sel *Selection) {
	if sel == nil || !sel.released.CompareAndSwap(false, true) {
		return
	}
	if sel.TargetKind != TargetKindUpstreamMember {
		if sel.accountRef != nil {
			decrementSlot(&sel.accountRef.concurrency)
			return
		}
		s.Release(sel.AccountID)
		return
	}
	if sel.upstreamRef != nil {
		decrementSlot(sel.upstreamRef.concurrency)
		return
	}
	if all, ok := s.store.upstreams.Load().(map[int64]*upstreamSnapshot); ok {
		if item := all[sel.TargetID]; item != nil {
			decrementSlot(item.concurrency)
		}
	}
}

func decrementSlot(counter *atomic.Int64) {
	for {
		cur := counter.Load()
		if cur <= 0 || counter.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// MarkSelectionResult applies a small group-local breaker for upstream members.
// Account selections retain the existing rule-engine path unchanged.
func (s *Scheduler) MarkSelectionResult(sel *Selection, kind rule.Kind, resetAt *time.Time, httpStatus int, errMsg, model string) {
	if sel == nil {
		return
	}
	if sel.TargetKind != TargetKindUpstreamMember {
		s.markResult(sel.AccountID, sel.GroupID, kind, resetAt, httpStatus, errMsg, model)
		return
	}
	item := sel.upstreamRef
	if item == nil {
		all, ok := s.store.upstreams.Load().(map[int64]*upstreamSnapshot)
		if !ok {
			return
		}
		item = all[sel.TargetID]
	}
	if item == nil {
		return
	}
	// Only retryable outcomes own the member-local breaker. A 4xx is a
	// deterministic request error and must never cool an otherwise healthy
	// upstream if a caller invokes this method directly.
	if kind != rule.KindOK && kind != rule.Kind429 && kind != rule.Kind5xx && kind != rule.KindNetwork {
		return
	}
	// The result path is concurrent: a request that started before a failure can
	// finish successfully after the failure has published its cooldown. Use a
	// compare-and-swap loop so a stale success (or another failure) cannot
	// overwrite a newer breaker state. This mirrors the account rule apply path.
	for {
		st := item.statePtr()
		now := s.timeNow()
		if kind == rule.KindOK {
			if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
				return
			}
			next := &upstreamState{}
			if !item.state.CompareAndSwap(st, next) {
				continue
			}
			s.enqueueUpstreamStatus(item, nil, 0, nil)
			return
		}
		streak := st.failureStreak + 1
		if streak < 1 {
			streak = 1
		}
		var until time.Time
		if kind == rule.Kind429 {
			until = *retry429Deadline(now, uint32(streak), resetAt)
		} else {
			until = now.Add(2 * time.Second)
		}
		msg := domain.TruncateErrMsg(errMsg)
		next := &upstreamState{cooldownUntil: &until, failureStreak: streak, lastError: msg}
		if !item.state.CompareAndSwap(st, next) {
			continue
		}
		var lastErr *string
		if msg != "" {
			lastErr = &msg
		}
		s.enqueueUpstreamStatus(item, &until, streak, lastErr)
		return
	}
}

func (s *Scheduler) enqueueUpstreamStatus(item *upstreamSnapshot, cooldown *time.Time, streak int, lastErr *string) {
	if s == nil || item == nil || item.member == nil || s.upstreamWriter == nil || !s.startOnce.Load() {
		return
	}
	var until *time.Time
	if cooldown != nil {
		v := *cooldown
		until = &v
	}
	var msg *string
	if lastErr != nil {
		v := domain.TruncateErrMsg(*lastErr)
		msg = &v
	}
	write := upstreamStatusWrite{id: item.member.ID, endpoint: item.endpoint, key: item.key, cooldown: until, streak: streak, lastErr: msg}
	select {
	case s.upstreamWriteCh <- write:
	default:
		if s.log != nil {
			s.log.Warn("upstream status writeback queue full", logx.Int64("group_upstream_id", item.member.ID))
		}
	}
}

func (s *Scheduler) ClassifySelection(sel *Selection, ev rule.Event) (domain.RuleThen, bool) {
	if sel == nil || sel.TargetKind != TargetKindUpstreamMember {
		if sel != nil && sel.GroupID > 0 {
			return s.classify(ev, sel.GroupID)
		}
		return s.Classify(ev)
	}
	// Upstream pools use the same retry decision semantics without pretending an
	// upstream is an account in rule events.
	return domain.RuleThen{}, ev.Kind == rule.Kind429 || ev.Kind == rule.Kind5xx || ev.Kind == rule.KindNetwork
}
