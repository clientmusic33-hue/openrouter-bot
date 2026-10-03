package provider

import (
	"sort"
	"strings"
	"time"
)

// LongContextTokens is the context window a model needs to be considered
// "long context" by the picker filter.
const LongContextTokens = 128_000

// AvailabilityIcon renders the status of a model. The status comes from real
// request outcomes only, never from the model simply being listed.
func AvailabilityIcon(availability ModelAvailability) string {
	switch availability {
	case ModelAvailable:
		return "🟢"
	case ModelLimited:
		return "🟡"
	case ModelUnavailable:
		return "🔴"
	default:
		return "⚪"
	}
}

// StatusCounts summarises a set of models for the provider and catalogue
// headers.
type StatusCounts struct {
	Working     int
	Limited     int
	Unavailable int
	Unknown     int
	Free        int
	Total       int
}

// Count tallies the statuses and free models of a list.
func Count(refs []ModelRef) StatusCounts {
	counts := StatusCounts{Total: len(refs)}

	for _, ref := range refs {
		if ref.Free && ref.PriceKnown {
			counts.Free++
		}

		switch ref.Availability {
		case ModelAvailable:
			counts.Working++
		case ModelLimited:
			counts.Limited++
		case ModelUnavailable:
			counts.Unavailable++
		default:
			counts.Unknown++
		}
	}

	return counts
}

// -----------------------------------------------------------------------------
// FILTERS
// -----------------------------------------------------------------------------

// Filter flags travel inside callback data, so they stay a small bitmask.
const (
	FilterFree = 1 << iota
	FilterWorking
	FilterFast
	FilterReasoning
	FilterCoding
	FilterVision
	FilterLongContext
	FilterFavourites
)

// ModelFilter narrows a catalogue down to what the user tapped.
type ModelFilter struct {
	Free        bool
	Working     bool
	Fast        bool
	Reasoning   bool
	Coding      bool
	Vision      bool
	LongContext bool
	Favourites  []string
	Query       string
}

// Bits encodes the toggles, without favourites or the search text.
func (f ModelFilter) Bits() int {
	bits := 0

	if f.Free {
		bits |= FilterFree
	}
	if f.Working {
		bits |= FilterWorking
	}
	if f.Fast {
		bits |= FilterFast
	}
	if f.Reasoning {
		bits |= FilterReasoning
	}
	if f.Coding {
		bits |= FilterCoding
	}
	if f.Vision {
		bits |= FilterVision
	}
	if f.LongContext {
		bits |= FilterLongContext
	}
	if len(f.Favourites) > 0 {
		bits |= FilterFavourites
	}

	return bits
}

// FilterFromBits rebuilds the toggles a callback carried.
func FilterFromBits(bits int, favourites []string, query string) ModelFilter {
	return ModelFilter{
		Free:        bits&FilterFree != 0,
		Working:     bits&FilterWorking != 0,
		Fast:        bits&FilterFast != 0,
		Reasoning:   bits&FilterReasoning != 0,
		Coding:      bits&FilterCoding != 0,
		Vision:      bits&FilterVision != 0,
		LongContext: bits&FilterLongContext != 0,
		Favourites:  favourites,
		Query:       strings.TrimSpace(query),
	}
}

// Active reports whether anything would be filtered out.
func (f ModelFilter) Active() bool {
	return f.Bits() != 0 || strings.TrimSpace(f.Query) != ""
}

// Match reports whether one model passes every active filter.
func (f ModelFilter) Match(ref ModelRef) bool {
	if f.Free && !(ref.Free && ref.PriceKnown) {
		return false
	}
	if f.Working && ref.Availability != ModelAvailable {
		return false
	}
	if f.Fast && !ref.Fast {
		return false
	}
	if f.Reasoning && !ref.Reasoning {
		return false
	}
	if f.Coding && !ref.Coding {
		return false
	}
	if f.Vision && !ref.Vision {
		return false
	}
	if f.LongContext && ref.ContextLength < LongContextTokens {
		return false
	}
	if f.Bits()&FilterFavourites != 0 && !f.isFavourite(ref.Model) {
		return false
	}

	return matchesQuery(ref, f.Query)
}

func (f ModelFilter) isFavourite(model string) bool {
	for _, favourite := range f.Favourites {
		if strings.EqualFold(favourite, model) {
			return true
		}
	}

	return false
}

// matchesQuery supports "name, id, provider, capability" lookups. Every
// space separated term has to match somewhere.
func matchesQuery(ref ModelRef, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}

	haystack := strings.ToLower(strings.Join([]string{
		ref.Model, ref.DisplayName, ref.Provider, ref.SourceProvider, ref.Description,
		capabilityKeywords(ref),
	}, " "))

	for _, term := range strings.Fields(query) {
		if !strings.Contains(haystack, term) {
			return false
		}
	}

	return true
}

// capabilityKeywords makes capability filters searchable as words.
func capabilityKeywords(ref ModelRef) string {
	var words []string

	if ref.Free && ref.PriceKnown {
		words = append(words, "free")
	}
	if ref.Fast {
		words = append(words, "fast")
	}
	if ref.Vision {
		words = append(words, "vision", "image")
	}
	if ref.Coding {
		words = append(words, "coding", "code")
	}
	if ref.Reasoning {
		words = append(words, "reasoning", "think")
	}
	if ref.ContextLength >= LongContextTokens {
		words = append(words, "long context")
	}

	return strings.Join(words, " ")
}

// Filter keeps the models a filter accepts.
func Filter(refs []ModelRef, filter ModelFilter) []ModelRef {
	if !filter.Active() {
		return refs
	}

	out := make([]ModelRef, 0, len(refs))
	for _, ref := range refs {
		if filter.Match(ref) {
			out = append(out, ref)
		}
	}

	return out
}

// -----------------------------------------------------------------------------
// ORDERING
// -----------------------------------------------------------------------------

// availabilityGroup is the availability/relevance ordering of the catalogue.
// It is about what can be used right now, not about model quality.
func availabilityGroup(ref ModelRef) int {
	if ref.Availability == ModelUnavailable {
		return 7
	}

	free := ref.Free && ref.PriceKnown

	switch ref.Availability {
	case ModelAvailable:
		if free {
			return 1
		}
		return 4
	case ModelLimited:
		if free {
			return 3
		}
		return 6
	default:
		if free {
			return 2
		}
		return 5
	}
}

// relevance orders models inside one availability group: observed health and
// latency first, then what the user pinned or bookmarked, then capabilities.
func relevance(ref ModelRef, current string, favourites []string) float64 {
	score := 0.0

	if ref.Availability == ModelAvailable {
		score += 12
	}

	failures := ref.Failures
	if failures > 4 {
		failures = 4
	}
	score -= float64(failures) * 8

	switch {
	case ref.Latency > 0 && ref.Latency < 1200*time.Millisecond:
		score += 10
	case ref.Latency > 8*time.Second:
		score -= 8
	case ref.Latency > 0:
		score += 4
	}

	if current != "" && strings.EqualFold(ref.Model, current) {
		score += 50
	}
	for _, favourite := range favourites {
		if strings.EqualFold(favourite, ref.Model) {
			score += 30
			break
		}
	}

	if ref.ContextLength > 0 {
		score += float64(min(ref.ContextLength/64_000, 6))
	}
	if ref.Vision {
		score += 2
	}
	if ref.Coding {
		score += 2
	}
	if ref.Reasoning {
		score += 2
	}
	if ref.Fast {
		score += 3
	}

	return score
}

// Sort orders a catalogue by availability first, then by relevance inside
// each group. It is a stable, deterministic ordering, which is what keeps the
// index a model button carries valid between rendering and the tap.
func Sort(refs []ModelRef, current string, favourites []string) []ModelRef {
	return SortBy(refs, func(ref ModelRef) ModelRef { return ref }, current, favourites)
}

// SortBy orders any slice carrying model references, so a paged UI list keeps
// the extra data it travels with.
func SortBy[T any](items []T, refOf func(T) ModelRef, current string, favourites []string) []T {
	if len(items) < 2 {
		return items
	}

	out := make([]T, len(items))
	copy(out, items)

	type scored struct {
		group     int
		relevance float64
		model     string
	}

	scores := make([]scored, len(out))
	for i, item := range out {
		ref := refOf(item)
		scores[i] = scored{
			group:     availabilityGroup(ref),
			relevance: relevance(ref, current, favourites),
			model:     strings.ToLower(ref.Model),
		}
	}

	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}

	sort.SliceStable(order, func(a, b int) bool {
		left, right := scores[order[a]], scores[order[b]]
		if left.group != right.group {
			return left.group < right.group
		}
		if left.relevance != right.relevance {
			return left.relevance > right.relevance
		}

		return left.model < right.model
	})

	sorted := make([]T, len(out))
	for i, index := range order {
		sorted[i] = out[index]
	}

	return sorted
}

// TopFree picks the free models worth showing at the top of /models. Only
// models the provider priced at zero for both prompt and completion are
// considered, and unavailable ones are left out.
func TopFree(refs []ModelRef, limit int) []ModelRef {
	free := make([]ModelRef, 0, 8)
	for _, ref := range refs {
		if !ref.Free || !ref.PriceKnown || ref.Availability == ModelUnavailable {
			continue
		}
		free = append(free, ref)
	}

	if limit <= 0 || len(free) <= limit {
		return free
	}

	return free[:limit]
}
